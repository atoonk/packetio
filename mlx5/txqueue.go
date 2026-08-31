//go:build linux && cgo && mlx5 && (amd64 || arm64)

package mlx5

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/internal/pool"
	"github.com/atoonk/packetio/mlx5/internal/dv"
	"github.com/atoonk/packetio/mlx5/internal/ring"
	"github.com/atoonk/packetio/mlx5/internal/wqe"
)

// TxQueue is one transmit queue: a send queue, its completion queue, and the
// frames it draws on.
//
// It is owned by one goroutine. Nothing here is synchronised, because the free
// list, the queue indices and the scratch slices all belong to that goroutine,
// and that is what keeps the packet path free of atomics.
type TxQueue struct {
	dev *Device
	// One logical queue may drive several hardware send queues. The card
	// processes copied-in packets at a bounded rate per send queue -- about a
	// quarter of what one core can produce -- so in copied mode a queue is
	// given several rings and Transmit deals batches round-robin, the way
	// every high-rate example used to by hand. In pointer mode one ring
	// already outruns a core and rings has one element.
	dvqs  []*dv.TxQueue
	rings []*ring.Tx
	next  int // the ring the next Transmit goes to
	pool  *pool.Frames

	// Scratch slices reused by Alloc and Complete, so neither allocates.
	alloc    []packetio.Desc
	addrs    []uint64
	released []packetio.Desc

	// Stalls, counted here rather than in the ring because this is where a
	// packet is turned away: the ring never sees a batch that Alloc could not
	// find room or frames for. Counted plainly and published with the rest,
	// since a locked add on the packet path is dearer than the work it counts.
	ringFull  atomic.Uint64
	poolEmpty atomic.Uint64
	stalls    struct{ ringFull, poolEmpty uint64 }

	// Immutable after creation, so that Stats can be read from another
	// goroutine without touching anything the packet loop owns.
	qpn, cqn          uint32
	blocks, cqEntries int
	inlineLen         int

	// Placement, done once by whichever goroutine first drives this queue.
	pinOnce sync.Once
	pinCPU  atomic.Int64
	pinErr  atomic.Pointer[error]

	index int

	// closed is atomic for the reason given on RxQueue: Close may run while
	// another goroutine is in a method here, and a plain bool read against a
	// plain bool write is a race whatever the compiler emits.
	closed atomic.Bool
}

func (d *Device) newTxQueue(index, firstFrame, frames, rings, inlineLen int, multiPacket, mpwPointer bool) (*TxQueue, error) {
	var csFlags uint8
	if d.cfg.checksums {
		csFlags = wqe.EthWQEL3Csum | wqe.EthWQEL4Csum
	}

	q := &TxQueue{
		dev:       d,
		pool:      pool.New(firstFrame, frames, d.cfg.frameSize),
		index:     index,
		inlineLen: inlineLen,
	}
	for i := 0; i < rings; i++ {
		dvq, err := d.dev.CreateTxQueue(d.cfg.txDepth)
		if err != nil {
			q.close()
			return nil, err
		}
		q.dvqs = append(q.dvqs, dvq)
		r, err := ring.NewTx(ring.TxConfig{
			SQ:                 dvq.SQ,
			CQ:                 dvq.CQ,
			SQDbrec:            dvq.SQDbrec,
			CQDbrec:            dvq.CQDbrec,
			UAR:                dvq.UAR,
			QPN:                dvq.QPN,
			LKey:               d.dev.LKey(),
			Region:             d.region.b,
			RegionVA:           d.region.va(),
			FrameSize:          d.region.FrameSize(),
			InlineLen:          inlineLen,
			CSFlags:            csFlags,
			SignalEvery:        d.cfg.signalEvery,
			MultiPacket:        multiPacket,
			MultiPacketPointer: mpwPointer,
			MultiPacketMaxLen:  d.cfg.mpwMaxLen,
		})
		if err != nil {
			q.close()
			return nil, err
		}
		q.rings = append(q.rings, r)
	}
	// Identity in Stats is the first ring's; the rest are siblings of it.
	q.qpn, q.cqn = q.dvqs[0].QPN, q.dvqs[0].CQN
	q.blocks, q.cqEntries = q.dvqs[0].Blocks(), q.dvqs[0].CQEntries()

	// Sized from what one ring can actually hold rather than from the depth
	// asked for: the provider rounds a queue up, and a scratch slice that has
	// to grow would allocate on the packet path. One ring's worth is enough,
	// because Alloc and Complete work against one ring at a time.
	capacity := q.rings[0].Capacity()
	q.alloc = make([]packetio.Desc, 0, capacity)
	q.addrs = make([]uint64, 0, capacity)
	q.released = make([]packetio.Desc, 0, capacity)
	return q, nil
}

// Region is the frame memory this queue draws on, shared with every other queue
// of the device.
func (q *TxQueue) Region() packetio.Region { return q.dev.region }

// Pin places the calling goroutine on a processor of its own and reports which
// one, or -1 if the device was opened with WithoutAffinity.
//
// The packet path calls this itself the first time it is used, so a program
// needs it only to find out where it ended up, or to place a worker before it
// starts rather than on its first batch.
func (q *TxQueue) Pin() (int, error) { return q.autoPin() }

// PinnedCPU is the processor this queue's worker was placed on, or -1.
func (q *TxQueue) PinnedCPU() int {
	if q.pinCPU.Load() == 0 {
		return -1
	}
	return int(q.pinCPU.Load()) - 1
}

// autoPin places the worker the first time the queue is driven. It runs after
// the closed check of its caller, so draining a closed queue never places
// anything.
func (q *TxQueue) autoPin() (int, error) {
	q.pinOnce.Do(func() {
		cpu, err := pin(q.dev.place)
		if err != nil {
			q.pinErr.Store(&err)
			return
		}
		if cpu >= 0 {
			q.pinCPU.Store(int64(cpu) + 1)
		}
	})
	if e := q.pinErr.Load(); e != nil {
		return -1, *e
	}
	return q.PinnedCPU(), nil
}

// Alloc takes up to n frames from the queue's free list.
//
// It never returns more than the following Transmit could accept, so a caller
// that transmits everything it allocated cannot leak a frame. The returned
// slice belongs to the queue and is reused by the next call.
func (q *TxQueue) Alloc(n int) []packetio.Desc {
	if q.closed.Load() || n <= 0 {
		return nil
	}
	q.autoPin()
	// Clamped against the ring the next Transmit will use, not the sum over
	// every ring: Alloc promises never to return more than the following
	// Transmit can accept, and the following Transmit goes to one ring.
	if free := q.rings[q.next].FreeSlots(); n > free {
		if free == 0 {
			q.stalls.ringFull++
			q.syncStalls()
			return nil
		}
		n = free
	}
	if have := q.pool.Len(); n > have {
		if have == 0 {
			q.stalls.poolEmpty++
			q.syncStalls()
			return nil
		}
		n = have
	}

	q.addrs = q.pool.Pop(n, q.addrs[:0])
	q.alloc = q.alloc[:0]
	for _, addr := range q.addrs {
		q.alloc = append(q.alloc, packetio.Desc{Addr: addr})
	}
	return q.alloc
}

// syncStalls publishes the stall counts. It runs only when a batch was turned
// away, which is to say only when nothing else is happening.
func (q *TxQueue) syncStalls() {
	q.ringFull.Store(q.stalls.ringFull)
	q.poolEmpty.Store(q.stalls.poolEmpty)
	for _, r := range q.rings {
		r.Sync()
	}
}

// Transmit hands descriptors to the NIC and returns how many it took, always a
// prefix of descs. The batch is published to the hardware before it returns.
//
// Frames in the prefix belong to the NIC until Complete returns them. Frames in
// the rest still belong to the caller, who must transmit them later or hand
// them back with Free.
func (q *TxQueue) Transmit(descs []packetio.Desc) int {
	if q.closed.Load() {
		return 0
	}
	q.autoPin()
	n := q.rings[q.next].Post(descs)
	// The next batch goes to the next ring, full or not: while one ring
	// drains, the others have room, and the core has time to fill them.
	if q.next++; q.next == len(q.rings) {
		q.next = 0
	}
	return n
}

// Complete reclaims up to max frames the NIC has finished with, returning them
// to the free list, and reports how many.
func (q *TxQueue) Complete(max int) int {
	if q.closed.Load() {
		return 0
	}
	total := 0
	for _, r := range q.rings {
		if total >= max {
			break
		}
		q.released = r.Complete(max-total, q.released[:0])
		for _, d := range q.released {
			q.pool.Push(q.pool.Base(d.Addr))
		}
		total += len(q.released)
	}
	return total
}

// Reclaim takes up to max frames the NIC has finished with and appends them to
// out. They are the caller's: this is how a frame that came from a receive
// queue gets back to it.
func (q *TxQueue) Reclaim(max int, out []packetio.Desc) []packetio.Desc {
	if q.closed.Load() {
		return out
	}
	before := len(out)
	for _, r := range q.rings {
		got := len(out) - before
		if got >= max {
			break
		}
		out = r.Complete(max-got, out)
	}
	return out
}

// Free returns frames to the free list without transmitting them.
func (q *TxQueue) Free(descs []packetio.Desc) {
	for _, d := range descs {
		q.pool.Push(q.pool.Base(d.Addr))
	}
}

// NumCompleted is how many frames Complete could reclaim right now.
func (q *TxQueue) NumCompleted() int {
	if q.closed.Load() {
		return 0
	}
	n := 0
	for _, r := range q.rings {
		n += r.Completable()
	}
	return n
}

// NumInFlight is how many frames the NIC currently owns, over every ring this
// queue drives.
func (q *TxQueue) NumInFlight() int {
	n := 0
	for _, r := range q.rings {
		n += r.InFlight()
	}
	return n
}

// NumFreeSlots is how many more packets the next Transmit will accept. That is
// one ring's room, not the sum over every ring: a batch goes to one ring, so
// the sum would promise space no single call can use.
func (q *TxQueue) NumFreeSlots() int {
	if q.closed.Load() {
		return 0
	}
	return q.rings[q.next].FreeSlots()
}

// NumFreeFrames is how many frames are on the free list.
func (q *TxQueue) NumFreeFrames() int { return q.pool.Len() }

// SendFunc is the whole transmit cycle in one call: reclaim what the NIC has
// finished with, take up to count frames, let build fill each one, and transmit
// them. It reports how many packets reached the NIC.
//
// build writes into frame and returns the packet's length. A length outside the
// frame abandons the entire batch: every frame goes back on the free list and
// nothing is transmitted, because a caller that has miscounted once cannot be
// trusted about the packets either side of it.
func (q *TxQueue) SendFunc(count int, build func(i int, frame []byte) int) (int, error) {
	if q.closed.Load() {
		return 0, packetio.ErrClosed
	}
	if err := q.failedInfo(); err != nil {
		return 0, fmt.Errorf("%w: %s", packetio.ErrQueueFailed, wqe.SyndromeString(err.Syndrome))
	}
	q.autoPin()

	q.Complete(count + q.NumInFlight())

	descs := q.Alloc(count)
	if len(descs) == 0 {
		return 0, nil
	}

	region := q.dev.region
	built := 0
	for i := range descs {
		frame := region.Writable(descs[i])
		n := build(i, frame)
		if n == 0 {
			// Nothing more to send: the batch ends here. An entry describing
			// no bytes is not an empty packet to the hardware, it is a
			// malformed descriptor that puts the queue out of service, so a
			// zero never reaches Transmit.
			break
		}
		if n < 0 || n > len(frame) {
			q.Free(descs)
			return 0, fmt.Errorf("%w: %d bytes into a %d-byte frame", packetio.ErrBadLength, n, len(frame))
		}
		descs[i].Len = uint32(n)
		built++
	}
	if built < len(descs) {
		q.Free(descs[built:])
	}

	sent := q.Transmit(descs[:built])
	if sent < built {
		q.Free(descs[sent:built])
	}
	return sent, nil
}

// Stats reports what this queue has done.
//
// It reads only counters, so it is safe to call from a goroutine other than the
// one driving the queue: a run that prints its rate every second does not have
// to interrupt the packet loop to do it.
func (q *TxQueue) Stats() (packetio.TxStats, error) {
	st := packetio.TxStats{
		RingFull:  q.ringFull.Load(),
		PoolEmpty: q.poolEmpty.Load(),
		Backend: map[string]uint64{
			// The identity fields are the first ring's; rings says how many
			// siblings it has behind this one queue.
			"qpn":          uint64(q.qpn),
			"cqn":          uint64(q.cqn),
			"rings":        uint64(len(q.rings)),
			"sq_blocks":    uint64(q.blocks),
			"cq_entries":   uint64(q.cqEntries),
			"inline_bytes": uint64(q.inlineLen),
			"multi_packet": boolCount(q.rings[0].MultiPacket()),
			// Anything but zero here is a bug above the pool: a frame returned
			// twice, or to the wrong queue. It is the one counter worth
			// checking when packets are wrong rather than missing.
			"pool_rejected": q.pool.Rejected(),
		},
	}
	var badDesc uint64
	for _, r := range q.rings {
		s := &r.Stats
		st.Packets += s.Packets.Load()
		st.Bytes += s.Bytes.Load()
		st.Completed += s.Completed.Load()
		st.Batches += s.Batches.Load()
		st.Completions += s.Completions.Load()
		st.RingFull += s.RingFull.Load()
		st.Errors += s.Errors.Load()
		badDesc += s.BadDesc.Load()
		if err := r.Failed(); err != nil {
			st.Backend["last_syndrome"] = uint64(err.Syndrome)
			st.Backend["last_vendor_syndrome"] = uint64(err.VendorSyndrome)
		}
	}
	// Descriptors refused because they did not name bytes inside one frame. A
	// bug above this layer, and never zero-cost to ignore.
	st.Backend["bad_desc"] = badDesc
	// What the NIC still owns, from the counters rather than from the queue's
	// own indices, which belong to the goroutine driving it.
	if st.Packets >= st.Completed {
		st.Backend["in_flight"] = st.Packets - st.Completed
	}
	return st, nil
}

func boolCount(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// Err reports the hardware error that put this queue out of service, or nil.
// A failed queue accepts no more packets; close the device and open it again.
//
// The syndrome is the card's own word for what went wrong, and is worth
// keeping in whatever the caller logs: it is the difference between "the queue
// died" and a diagnosis.
func (q *TxQueue) Err() error {
	if q.closed.Load() {
		return packetio.ErrClosed
	}
	info := q.failedInfo()
	if info == nil {
		return nil
	}
	return fmt.Errorf("%w: %s (vendor %#x) on queue pair %d", packetio.ErrQueueFailed,
		wqe.SyndromeString(info.Syndrome), info.VendorSyndrome, info.QPN)
}

// failedInfo is the first ring's fatal error, or nil. One dead ring fails the
// whole queue: a failed send queue never recovers, and a queue quietly down a
// quarter of its rings would look like a slow link forever.
func (q *TxQueue) failedInfo() *wqe.ErrorInfo {
	for _, r := range q.rings {
		if info := r.Failed(); info != nil {
			return info
		}
	}
	return nil
}

// Close releases the queue. Use Device.Close to shut down a whole device;
// closing one queue of a device that is still open is only for tests.
func (q *TxQueue) Close() error { return q.close() }

func (q *TxQueue) close() error {
	if q.closed.Swap(true) {
		return nil
	}
	var first error
	for _, dvq := range q.dvqs {
		if err := dvq.Close(); err != nil && first == nil {
			first = err
		}
	}
	q.dvqs = nil
	return first
}
