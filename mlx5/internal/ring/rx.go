package ring

import (
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"unsafe"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/mlx5/internal/arch"
	"github.com/atoonk/packetio/mlx5/internal/wqe"
)

// RxConfig describes the memory of one receive queue and its completion queue.
type RxConfig struct {
	// RQ is the receive work queue entry buffer and CQ its completion queue's.
	RQ []byte
	CQ []byte

	// RQDbrec is where the number of posted buffers is published, and CQDbrec
	// how far software has read the completions.
	RQDbrec *uint32
	CQDbrec *uint32

	// Stride is the size of one receive entry, which must be the sixteen bytes
	// of a single data segment: one buffer per entry.
	Stride uint32

	// LKey is the memory key of the region, Region the frame memory and
	// RegionVA the address the NIC knows it by.
	LKey     uint32
	Region   []byte
	RegionVA uint64

	// BufferSize is how many bytes of each frame the NIC may fill. A packet
	// longer than this is not received; the NIC has nowhere to put the rest.
	BufferSize uint32
}

// RxStats counts what a receive ring has done. The fields are atomic because a
// monitoring goroutine reads them while the queue's own goroutine writes them,
// and they are published now and then rather than added to on every call, for
// the reason given on TxStats.
type RxStats struct {
	Packets     atomic.Uint64
	Bytes       atomic.Uint64
	Filled      atomic.Uint64
	Batches     atomic.Uint64
	Completions atomic.Uint64
	Errors      atomic.Uint64

	// Oversize counts completions whose byte count was longer than the buffer
	// the NIC was given. It is never non-zero on a healthy queue.
	Oversize atomic.Uint64

	// Outstanding is a gauge, not a counter: how many buffers the NIC held
	// at the last publish. It exists so that a monitoring goroutine's Stats
	// never reads the ring's own cursors, which belong to the owner.
	Outstanding atomic.Uint64
}

// Rx is one receive queue.
//
// Buffers are posted in order and consumed in order, so the entry a completion
// refers to is always the oldest outstanding one. The queue reports which entry
// it was as well, and that is checked rather than trusted: a mismatch means
// software and hardware disagree about what is outstanding, and going on from
// there would hand the application a frame the NIC is still writing.
type Rx struct {
	rq      []byte
	entries uint32
	mask    uint32
	stride  uint32

	cq      wqe.CQ
	rqDbrec *uint32
	cqDbrec *uint32

	region   []byte
	regionVA uint64
	bufSize  uint32 // the most bytes the NIC may write into one buffer

	pi   uint32 // buffers posted
	ci   uint32 // buffers the NIC has reported on
	cqci uint32 // completions read

	posted []packetio.Desc
	failed []packetio.Desc

	// dead is the error that took the queue out of service, if any. It is
	// atomic because a monitoring goroutine reads it through Err while the
	// queue's own goroutine sets it.
	dead atomic.Pointer[error]

	c struct {
		packets      uint64
		bytes        uint64
		filled       uint64
		batches      uint64
		completions  uint64
		errors       uint64
		oversize     uint64
		sincePublish uint32
	}

	Stats RxStats
}

// Sync makes the queue's counters current, from the goroutine that drives it.
func (r *Rx) Sync() {
	c := &r.c
	c.sincePublish = 0
	r.Stats.Packets.Store(c.packets)
	r.Stats.Bytes.Store(c.bytes)
	r.Stats.Filled.Store(c.filled)
	r.Stats.Batches.Store(c.batches)
	r.Stats.Completions.Store(c.completions)
	r.Stats.Errors.Store(c.errors)
	r.Stats.Oversize.Store(c.oversize)
	r.Stats.Outstanding.Store(uint64(r.pi - r.ci))
}

func (r *Rx) tick() {
	r.c.sincePublish++
	if r.c.sincePublish >= publishEvery {
		r.Sync()
	}
}

// NewRx prepares a receive ring over the given queue memory. It pre-fills the
// constant half of every entry, so that posting a buffer afterwards is a single
// store of its address.
func NewRx(cfg RxConfig) (*Rx, error) {
	if cfg.Stride != 16 {
		return nil, fmt.Errorf("ring: a receive entry of %d bytes, want the 16 of one data segment", cfg.Stride)
	}
	if len(cfg.RQ)%int(cfg.Stride) != 0 || len(cfg.RQ) == 0 {
		return nil, fmt.Errorf("ring: a receive queue of %d bytes is not a whole number of entries", len(cfg.RQ))
	}
	entries := uint32(len(cfg.RQ)) / cfg.Stride
	if entries&(entries-1) != 0 {
		return nil, fmt.Errorf("ring: a receive queue of %d entries is not a power of two", entries)
	}
	cq, err := wqe.NewCQ(cfg.CQ, wqe.CQESize)
	if err != nil {
		return nil, err
	}
	if cq.Entries() < entries {
		return nil, fmt.Errorf("ring: a completion queue of %d entries is smaller than the %d buffers that can be outstanding", cq.Entries(), entries)
	}
	if cfg.RQDbrec == nil || cfg.CQDbrec == nil {
		return nil, fmt.Errorf("ring: the receive queue is missing a doorbell record")
	}
	if cfg.BufferSize == 0 {
		return nil, fmt.Errorf("ring: buffers of no bytes")
	}

	r := &Rx{
		rq: cfg.RQ, entries: entries, mask: entries - 1, stride: cfg.Stride,
		cq: cq, rqDbrec: cfg.RQDbrec, cqDbrec: cfg.CQDbrec,
		region: cfg.Region, regionVA: cfg.RegionVA, bufSize: cfg.BufferSize,
		posted: make([]packetio.Desc, entries),
	}

	// How long a buffer is and which region it belongs to never change, so
	// they are written once here and a refill only writes the address.
	for i := uint32(0); i < entries; i++ {
		off := i * cfg.Stride
		binary.BigEndian.PutUint32(r.rq[off:], cfg.BufferSize)
		binary.BigEndian.PutUint32(r.rq[off+4:], cfg.LKey)
	}
	return r, nil
}

// Entries is how many buffers the queue holds.
func (r *Rx) Entries() int { return int(r.entries) }

// FreeSlots is how many more buffers can be posted.
func (r *Rx) FreeSlots() int { return int(r.entries - (r.pi - r.ci)) }

// Outstanding is how many buffers the NIC currently holds.
func (r *Rx) Outstanding() int { return int(r.pi - r.ci) }

// Fill posts buffers for the NIC to receive into and returns how many it took,
// always a prefix of descs. Those frames belong to the NIC until they come back
// from Receive.
//
// A buffer is only ever posted into a slot whose previous occupant has been
// reported on. Reusing a slot sooner would hand the NIC an address it is
// already writing to, and the packet that lands there is corrupted with nothing
// to show for it.
func (r *Rx) Fill(descs []packetio.Desc) int {
	n := len(descs)
	if free := r.FreeSlots(); n > free {
		n = free
	}
	if n == 0 {
		return 0
	}

	for i := 0; i < n; i++ {
		d := descs[i]
		slot := r.pi & r.mask
		binary.BigEndian.PutUint64(r.rq[slot*r.stride+8:], r.regionVA+d.Addr)
		r.posted[slot] = d
		r.pi++
	}

	// Telling the NIC how many buffers are available is the whole publication:
	// a receive queue has no register to write, because the NIC reads the
	// record itself when it needs somewhere to put a packet. A striding queue
	// counts entries rather than buffers, so what is published is the number
	// of whole entries filled.
	var db [4]byte
	binary.BigEndian.PutUint32(db[:], r.pi&0xffff)
	arch.PublishDbrec(r.rqDbrec, nativeU32(db[:]))

	r.c.filled += uint64(n)
	r.tick()
	return n
}

// Receive takes up to max received packets and appends them to out, returning
// the grown slice. The frames belong to the caller until they are posted again.
//
// Frames whose packet failed are not returned: they are put aside for Failed,
// because they hold nothing worth looking at but must still find their way back
// to the pool.
func (r *Rx) Receive(max int, out []packetio.Desc) []packetio.Desc {
	r.failed = r.failed[:0]
	if max <= 0 || r.dead.Load() != nil {
		return out
	}

	var (
		got   int
		bytes uint64
		read  uint64
	)
	for got < max {
		// The barrier belongs to this entry, not to the round. What the
		// ordering rule requires is that the owner bit saying THIS entry is
		// ready is seen before anything in THIS entry is read, and only a
		// barrier between the two gives that: on arm64 the control dependency
		// from the ownership test orders stores, not loads, so without it the
		// body read may be satisfied by a fetch made before the device wrote
		// it. A fence on the first entry of a round says nothing about the
		// second. rdma-core and DPDK both fence per completion; the saving of
		// doing it once was not ours to take.
		header := arch.LoadCQEHeader(r.cq.HeaderPtr(r.cqci))
		if !r.cq.Owned(r.cqci, header) {
			break
		}
		if wqe.Format(header) != wqe.CQEFormatNoData {
			// A compressed completion stands for several packets and needs a
			// decoder this does not have. Consuming it would put the consumer
			// index out of step with the hardware for good, so stop -- but
			// latch it, because the entry is still there next time. Breaking
			// without latching left the queue re-reading the same entry on
			// every call, returning nothing, counting an error, forever, with
			// no way for the caller to find out.
			r.c.errors++
			r.fail(fmt.Errorf("ring: a completion of format %d, which needs a decoder this does not have",
				wqe.Format(header)))
			break
		}
		r.cqci++
		read++

		opcode := wqe.Opcode(header)
		if opcode != wqe.CQERespSend && opcode != wqe.CQERespErr {
			// Nothing else belongs on a receive queue, and none of it consumes
			// a buffer, so there is nothing to give back.
			r.c.errors++
			continue
		}

		// The queue consumes its buffers in order, so the completion belongs
		// to the oldest outstanding one. The hardware says which as well;
		// disagreement means the two have lost track of each other.
		if r.pi == r.ci {
			r.c.errors++
			continue
		}
		slot := r.ci & r.mask
		if counter := wqe.WQECounter(header); uint32(counter)&r.mask != slot {
			// Hardware and software disagree about which buffer this is. The
			// consumer index has already moved past the completion, but the
			// buffer it named is not the one we think, so advancing ci would
			// hand out the wrong frame and every later completion would
			// mismatch too -- leaking the lot. Stop instead.
			r.c.errors++
			r.fail(fmt.Errorf("ring: completion names buffer %d, expected %d; "+
				"the queue and the card have lost track of each other",
				uint32(counter)&r.mask, slot))
			break
		}
		d := r.posted[slot]
		r.ci++

		// The frame the NIC just filled is in no cache: inbound DMA lands in
		// memory on hosts without a cache-placement path (EPYC has none), so
		// the caller's first read of it would pay the full memory latency,
		// packet after packet. Hint it here instead -- one line per
		// completion consumed, so the walk itself paces the hints and the
		// misses overlap. Measured forwarding 64-byte frames on 8 cores:
		// 70.6 Mpps without this, 139.3 with. See arch.Prefetch.
		arch.Prefetch(unsafe.Pointer(&r.region[d.Addr]))

		if opcode == wqe.CQERespErr {
			r.c.errors++
			r.failed = append(r.failed, d)
			continue
		}

		entry := r.cq.Entry(r.cqci - 1)
		length := wqe.ByteCount(entry)
		if length > r.bufSize {
			// The NIC was told how many bytes each buffer holds, so a longer
			// count is the hardware and this code disagreeing about the queue.
			// Believing it would hand the caller a descriptor running past its
			// frame, and on a forwarding path straight back to a transmit queue
			// as an instruction to put the neighbouring frames on the wire.
			r.c.errors++
			r.c.oversize++
			r.failed = append(r.failed, d)
			continue
		}
		d.Len = length
		d.Options = 0
		if f := wqe.ChecksumFlags(entry); f&wqe.CQEL3OK != 0 {
			// The header checksum on its own is worth reporting: a forwarder
			// checks the header and never looks at the payload, and a UDP
			// packet may legitimately carry no L4 checksum at all.
			d.Options |= packetio.OptL3ChecksumOK
			if f&wqe.CQEL4OK != 0 {
				d.Options |= packetio.OptChecksumOK
			}
		}
		out = append(out, d)
		got++
		bytes += uint64(length)
	}

	if read == 0 {
		if r.pi == r.ci {
			r.Sync() // the queue is idle, so this costs nothing
		}
		return out
	}
	r.c.completions += read
	r.c.batches++
	r.c.packets += uint64(got)
	r.c.bytes += bytes

	// Releasing these completions lets the NIC write over them, so it comes
	// after every read of them.
	var db [4]byte
	binary.BigEndian.PutUint32(db[:], r.cqci&0xffffff)
	arch.ReleaseCQ(r.cqDbrec, nativeU32(db[:]))
	return out
}

// fail takes the queue out of service. The first reason is kept: it is the one
// that explains the rest.
func (r *Rx) fail(err error) {
	r.dead.CompareAndSwap(nil, &err)
}

// Err reports the error that took this queue out of service, or nil. It is safe
// to call from any goroutine.
//
// A receive queue that stops receiving otherwise looks exactly like a quiet
// link, and the two want opposite responses.
func (r *Rx) Err() error {
	if e := r.dead.Load(); e != nil {
		return *e
	}
	return nil
}

// Failed returns the frames of packets that failed, which the last Receive put
// aside. They carry nothing but must go back to the pool.
func (r *Rx) Failed() []packetio.Desc { return r.failed }

// Ready reports whether a completion is waiting, without consuming it.
func (r *Rx) Ready() bool {
	header := arch.LoadCQEHeaderRaw(r.cq.HeaderPtr(r.cqci))
	return r.cq.Owned(r.cqci, header)
}

// ReadyCount is how many completions are waiting, up to max, without consuming
// any of them.
//
// A receive queue has no counter to read for this: the only way to know is to
// look at the completions themselves, which is why a receiver that polls in a
// loop should call Receive directly rather than ask first.
func (r *Rx) ReadyCount(max int) int {
	n := 0
	for ci := r.cqci; n < max; ci++ {
		header := arch.LoadCQEHeaderRaw(r.cq.HeaderPtr(ci))
		if !r.cq.Owned(ci, header) {
			break
		}
		n++
	}
	return n
}
