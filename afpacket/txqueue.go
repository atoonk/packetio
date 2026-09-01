//go:build linux

package afpacket

import (
	"fmt"
	"sync/atomic"
	"unsafe"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/internal/pool"
	"golang.org/x/sys/unix"
)

// txBatch is the most messages handed to one sendmmsg call. The kernel copies
// each frame, so a larger batch mostly buys fewer system calls; past a few
// hundred that is no longer the cost that matters.
const txBatch = 256

// maxPending bounds how many sent frames wait to be handed back. It plays the
// part the ring size plays on a real NIC: past it Transmit reports a full ring,
// which is the backpressure a caller that never reclaims deserves to see
// instead of an unbounded slice.
const maxPending = 4096

// TxQueue transmits with batched sendmmsg.
//
// A transmit ring (PACKET_TX_RING) was tried in the code this is ported from
// and measured slower than sendmmsg, so it is deliberately not here: TPACKET_V2
// is the only transmit ring the kernel offers, its frames are fixed size, and
// the doorbell is still a system call. One sendmmsg per batch with iovecs
// pointing straight into the region is both faster and much simpler.
//
// sendmmsg is synchronous, so a frame the kernel accepted is finished the
// moment Transmit returns. It is still not handed back until Complete or
// Reclaim asks for it, because that is the contract every backend shares. The
// forwarding cycle in the packetio package doc transmits frames belonging to a
// receive queue and gets them back through Reclaim; a backend that quietly
// returned them to its own pool would starve that receive queue and alias its
// frames into this one. So sent frames wait on a pending list -- this backend's
// stand-in for a ring -- until they are asked for.
//
// One goroutine per queue, as everywhere in packetio. There is no lock: the
// pool is unsynchronised by design and the scratch slices are reused.
type TxQueue struct {
	region *region
	fd     int
	gso    bool
	pool   *pool.Frames

	msgs []mmsghdr
	iovs []unix.Iovec
	// hdrs holds one virtio-net header per message, and iovs2 the second
	// iovec slot each message needs to point at it. Only allocated in GSO
	// mode, where a message is [header | frame] rather than just the frame.
	hdrs  [][packetio.OffloadHdrLen]byte
	iovs2 []unix.Iovec

	// descs and addrs are Alloc's scratch, reused by every call; pending holds
	// the frames the kernel has taken but nobody has asked for yet.
	descs   []packetio.Desc
	addrs   []uint64
	pending []packetio.Desc
	// pendingN mirrors len(pending) for Stats, which reads it from a
	// monitoring goroutine while the owner grows and shrinks the list.
	pendingN atomic.Int64

	// The counters are atomics because Stats reads them from a monitoring
	// goroutine while the queue's own goroutine drives it. The owner adds
	// once per batch, never per packet.
	sent     atomic.Uint64
	bytes    atomic.Uint64
	batches  atomic.Uint64
	drops    atomic.Uint64 // frames lost to a fatal sendmmsg error
	ringFull atomic.Uint64 // calls that could queue nothing
	rejected atomic.Uint64 // descriptors refused before the syscall
	errno    atomic.Int32
	// closed is atomic because Err and Stats may be called from a monitoring
	// goroutine while the queue's own goroutine closes it.
	closed atomic.Bool
}

// mmsghdr is struct mmsghdr: a msghdr plus the length the kernel reports back.
type mmsghdr struct {
	hdr unix.Msghdr
	len uint32
	_   [4]byte
}

func newTxQueue(r *region, fd, firstFrame, frames int, gso bool) *TxQueue {
	q := &TxQueue{
		region:  r,
		fd:      fd,
		gso:     gso,
		pool:    pool.New(firstFrame, frames, r.frameSize),
		msgs:    make([]mmsghdr, txBatch),
		iovs:    make([]unix.Iovec, txBatch),
		descs:   make([]packetio.Desc, 0, txBatch),
		addrs:   make([]uint64, 0, txBatch),
		pending: make([]packetio.Desc, 0, maxPending),
	}
	for i := range q.msgs {
		q.msgs[i].hdr.Iov = &q.iovs[i]
		q.msgs[i].hdr.Iovlen = 1
	}
	if gso {
		// Two iovecs per message, laid out flat and consecutively so that
		// msghdr.Iov can point at the pair.
		q.hdrs = make([][packetio.OffloadHdrLen]byte, txBatch)
		q.iovs2 = make([]unix.Iovec, 2*txBatch)
		for i := range q.msgs {
			q.msgs[i].hdr.Iov = &q.iovs2[2*i]
			q.msgs[i].hdr.Iovlen = 2
			q.iovs2[2*i].Base = &q.hdrs[i][0]
			q.iovs2[2*i].SetLen(packetio.OffloadHdrLen)
		}
	}
	return q
}

// Region is the frame memory this queue draws on.
func (q *TxQueue) Region() packetio.Region { return q.region }

// Alloc takes up to n frames from this queue's pool, never more than the
// pending list has room for. The returned slice is reused by the next call.
func (q *TxQueue) Alloc(n int) []packetio.Desc {
	if q.closed.Load() {
		return nil
	}
	q.addrs = q.pool.Pop(min(n, q.NumFreeSlots()), q.addrs[:0])
	q.descs = q.descs[:0]
	for _, a := range q.addrs {
		// The same headroom Receive gives, for the same two reasons; see
		// frameHeadroom. Free and Complete round down with pool.Base, so a
		// frame goes home whole regardless of the offset it was used at.
		q.descs = append(q.descs, packetio.Desc{Addr: a + frameHeadroom})
	}
	return q.descs
}

// Transmit sends descs and returns how many the kernel accepted, always a
// prefix. Accepted frames wait for Complete or Reclaim; the unaccepted suffix
// still belongs to the caller.
func (q *TxQueue) Transmit(descs []packetio.Desc) int {
	n, _ := q.transmit(descs, nil)
	return n
}

// TransmitOffload is Transmit with segmentation and checksum metadata for each
// frame, so a super-frame is cut up by the kernel rather than here. A zero
// Offload sends an ordinary frame.
//
// It implements [packetio.OffloadTransmitter]: it returns the accepted prefix,
// and an error when a descriptor or its Offload was refused, or when this
// queue was not opened with [WithGSO].
func (q *TxQueue) TransmitOffload(descs []packetio.Desc, offs []packetio.Offload) (int, error) {
	if !q.gso {
		return 0, fmt.Errorf("%w: TransmitOffload needs WithGSO; without PACKET_VNET_HDR "+
			"the kernel would put the virtio header on the wire as packet data", packetio.ErrUnsupported)
	}
	if len(offs) != len(descs) {
		return 0, fmt.Errorf("%w: %d descriptors but %d offloads",
			packetio.ErrBadLength, len(descs), len(offs))
	}
	return q.transmit(descs, offs)
}

// checkOffload reports why o's offsets do not fit a frame of n bytes, or nil.
// Every field is caller-supplied and the kernel refuses a bad one with EINVAL
// for the whole batch, so they are checked here where the blame is clear.
// Subtract rather than add: the offsets are uint16 widened to uint32, and an
// addition on the left of the comparison is how this kind of check goes wrong.
func checkOffload(o packetio.Offload, n uint32) error {
	if o.Flags&packetio.OffloadNeedsCsum != 0 {
		start, off := uint32(o.CsumStart), uint32(o.CsumOff)
		if start > n || off > n-start || off+2 > n-start {
			return fmt.Errorf("checksum field at %d+%d does not fit %d bytes", start, off, n)
		}
	}
	if o.Segmented() {
		if o.GSOSize == 0 {
			return fmt.Errorf("segmented but the segment size is zero")
		}
		if uint32(o.HdrLen) > n {
			return fmt.Errorf("header length %d is past the %d-byte frame", o.HdrLen, n)
		}
	}
	return nil
}

func (q *TxQueue) transmit(descs []packetio.Desc, offs []packetio.Offload) (int, error) {
	if q.closed.Load() {
		return 0, packetio.ErrClosed
	}
	if len(descs) == 0 {
		return 0, nil
	}
	room := q.NumFreeSlots()
	if room == 0 {
		q.ringFull.Add(1)
		return 0, nil
	}
	if len(descs) > room {
		descs = descs[:room]
		if offs != nil {
			offs = offs[:room]
		}
	}

	total := 0
	for off := 0; off < len(descs); off += txBatch {
		batch := descs[off:min(off+txBatch, len(descs))]

		// Validate before touching the socket and stop at the first bad
		// descriptor: what is accepted has to be a prefix, and one wild
		// pointer in an iovec is EFAULT for the whole batch rather than for
		// the frame that caused it.
		n := 0
		var refused error
		for i, d := range batch {
			w := q.region.Writable(d)
			if w == nil || d.Len == 0 || int(d.Len) > len(w) {
				refused = fmt.Errorf("%w: descriptor %d names %d bytes at offset %d, "+
					"which is not inside one frame", packetio.ErrBadLength, off+i, d.Len, d.Addr)
				break
			}
			if offs != nil {
				if err := checkOffload(offs[off+i], d.Len); err != nil {
					refused = fmt.Errorf("%w: descriptor %d: %v", packetio.ErrBadLength, off+i, err)
					break
				}
			}
			n++
		}

		if n > 0 {
			for i, d := range batch[:n] {
				if q.gso {
					var o packetio.Offload
					if offs != nil {
						o = offs[off+i]
					}
					o.Marshal(q.hdrs[i][:])
					q.iovs2[2*i+1].Base = &q.region.b[d.Addr]
					q.iovs2[2*i+1].SetLen(int(d.Len))
				} else {
					q.iovs[i].Base = &q.region.b[d.Addr]
					q.iovs[i].SetLen(int(d.Len))
				}
			}
			sent := q.sendAll(n)
			q.batches.Add(1)
			var bytes uint64
			for _, d := range batch[:sent] {
				bytes += uint64(d.Len)
			}
			q.bytes.Add(bytes)
			// sent is published before pendingN: a concurrent Stats then sees
			// Completed transiently high by at most one batch, never wrapped
			// below zero by the unsigned subtraction.
			q.sent.Add(uint64(sent))
			q.pending = append(q.pending, batch[:sent]...)
			q.pendingN.Store(int64(len(q.pending)))
			total += sent
			if sent < n {
				// The kernel stopped short: a full device queue or a fatal
				// error, both already counted by sendAll. The rest of descs
				// stays with the caller. Report the refusal too if there was
				// one -- it names a specific descriptor, and losing that
				// because the queue also happened to fill would send the
				// caller looking in the wrong place.
				if refused != nil {
					q.rejected.Add(1)
				}
				return total, refused
			}
		}
		if refused != nil {
			q.rejected.Add(1)
			return total, refused
		}
	}
	return total, nil
}

// sendAll transmits msgs[0:n] and returns how many the kernel accepted.
//
// A full device queue (EAGAIN or ENOBUFS) is not an error and not something to
// retry here: Transmit reports how many frames it took, and deciding what to do
// about the rest is the caller's business. The code this was ported from
// retried instead, because its caller could not, and spinning turned out to
// cost several cores once more than one socket pushed at the same interface.
//
// EINTR is retried, but only a bounded number of times. The bound is the point:
// looping forever on a signal storm, or looping without progress when sendmmsg
// returns zero with no error, pins the caller at 100% CPU inside this function.
// A caller that never returns is a far worse failure than a lost batch.
func (q *TxQueue) sendAll(n int) int {
	const maxStall = 16
	sent, stall := 0, 0
	for sent < n {
		got, _, errno := unix.Syscall6(unix.SYS_SENDMMSG, uintptr(q.fd),
			uintptr(unsafe.Pointer(&q.msgs[sent])), uintptr(n-sent), 0, 0, 0)
		if errno == 0 && got > 0 {
			sent += int(got)
			stall = 0
			continue
		}
		if errno == unix.EAGAIN || errno == unix.ENOBUFS {
			// The device queue is full. Hand back what went, and count it, so
			// a caller offering more than the link can carry can see why
			// rather than guessing.
			q.ringFull.Add(1)
			break
		}
		stall++
		if errno != 0 && errno != unix.EINTR || stall >= maxStall {
			// The interface is gone, or the frame is too large for its MTU.
			// The rest of this batch is lost; counting it is the difference
			// between a visible drop and what looks to the far end like a
			// lossy link.
			q.drops.Add(uint64(n - sent))
			if errno != 0 {
				q.errno.Store(int32(errno))
			}
			break
		}
	}
	return sent
}

// SendFunc is the whole transmit cycle in one call, as [packetio.TxQueue]
// describes it: reclaim, allocate, build, transmit.
func (q *TxQueue) SendFunc(count int, build func(i int, frame []byte) int) (int, error) {
	if q.closed.Load() {
		return 0, packetio.ErrClosed
	}
	q.Complete(count + q.NumInFlight())

	descs := q.Alloc(count)
	if len(descs) == 0 {
		return 0, nil
	}
	n := 0
	for i := range descs {
		frame := q.region.Writable(descs[i])
		l := build(i, frame)
		if l == 0 {
			break // nothing more to send
		}
		if l < 0 || l > len(frame) {
			q.Free(descs)
			return 0, fmt.Errorf("%w: %d bytes into a %d-byte frame",
				packetio.ErrBadLength, l, len(frame))
		}
		descs[i].Len = uint32(l)
		n++
	}
	if n < len(descs) {
		q.Free(descs[n:])
	}
	sent := q.Transmit(descs[:n])
	if sent < n {
		q.Free(descs[sent:n])
	}
	return sent, nil
}

// Complete returns up to max sent frames to this queue's pool and reports how
// many. Frames that came from a receive queue must go back through Reclaim
// instead; this pool refuses them.
func (q *TxQueue) Complete(max int) int {
	n := min(max, len(q.pending))
	if n <= 0 {
		return 0
	}
	for _, d := range q.pending[:n] {
		q.pool.Push(q.pool.Base(d.Addr))
	}
	q.drop(n)
	return n
}

// Reclaim hands up to max sent frames to the caller instead of returning them
// to this queue's pool, for frames that belong to a receive queue.
func (q *TxQueue) Reclaim(max int, out []packetio.Desc) []packetio.Desc {
	n := min(max, len(q.pending))
	if n <= 0 {
		return out
	}
	out = append(out, q.pending[:n]...)
	q.drop(n)
	return out
}

// drop removes the first n frames from the pending list, keeping its order and
// its backing array. Read them before calling it.
func (q *TxQueue) drop(n int) {
	q.pending = q.pending[:copy(q.pending, q.pending[n:])]
	q.pendingN.Store(int64(len(q.pending)))
}

// Free returns frames to the pool without transmitting them. A frame this pool
// does not own is refused there and counted, not silently accepted.
func (q *TxQueue) Free(descs []packetio.Desc) {
	for _, d := range descs {
		q.pool.Push(q.pool.Base(d.Addr))
	}
}

// NumCompleted is how many frames Complete or Reclaim would hand back now.
func (q *TxQueue) NumCompleted() int { return len(q.pending) }

// NumInFlight is the same number. sendmmsg is synchronous, so everything the
// kernel took is already on the wire and merely waiting to be handed back;
// there is no moment when the NIC owns a frame this queue does not.
func (q *TxQueue) NumInFlight() int { return len(q.pending) }

// NumFreeSlots is how many more frames Transmit will accept before the pending
// list is full.
func (q *TxQueue) NumFreeSlots() int { return maxPending - len(q.pending) }

// NumFreeFrames is how many frames are in the free pool.
func (q *TxQueue) NumFreeFrames() int { return q.pool.Len() }

// Stats reports this queue's counters. Backend holds "rejected_descs", the
// descriptors refused before the syscall, and "pool_rejected", frames the
// pool refused as foreign or surplus; either being non-zero is a bug in the
// caller.
func (q *TxQueue) Stats() (packetio.TxStats, error) {
	return packetio.TxStats{
		Packets:   q.sent.Load(),
		Bytes:     q.bytes.Load(),
		Completed: q.sent.Load() - uint64(q.pendingN.Load()),
		Batches:   q.batches.Load(),
		RingFull:  q.ringFull.Load(),
		Errors:    q.drops.Load() + q.rejected.Load(),
		Backend: map[string]uint64{
			"rejected_descs": q.rejected.Load(),
			"pool_rejected":  q.pool.Rejected(),
		},
	}, nil
}

// LastErrno returns the errno that ended the most recent failed batch, or zero.
// Specific to this backend: it is the only way to tell a full device queue from
// an interface that has gone away.
func (q *TxQueue) LastErrno() int32 { return q.errno.Load() }

// Close stops the queue. The socket belongs to the Device and is closed there.
func (q *TxQueue) Close() error {
	q.closed.Store(true)
	return nil
}

// Err reports that the queue is out of service, or nil.
//
// A packet socket has no failure that outlives one call: a send that fails
// fails for that batch, and the queue is as usable afterwards as before. So
// this is nil until the queue is closed, and that is the honest answer rather
// than a stub that can never say anything.
func (q *TxQueue) Err() error {
	if q.closed.Load() {
		return packetio.ErrClosed
	}
	return nil
}

var (
	_ packetio.TxQueue            = (*TxQueue)(nil)
	_ packetio.OffloadTransmitter = (*TxQueue)(nil)
)
