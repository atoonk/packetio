//go:build linux && cgo && mlx5 && (amd64 || arm64)

package mlx5

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/internal/pool"
	"github.com/atoonk/packetio/mlx5/internal/dv"
	"github.com/atoonk/packetio/mlx5/internal/ring"
)

// RxQueue is one receive queue: the buffers the NIC writes into, the queue that
// says what arrived, and the frames it draws on.
//
// It is owned by one goroutine, like a transmit queue, and for the same reason.
type RxQueue struct {
	dev  *Device
	dvq  *dv.RxQueue
	ring *ring.Rx
	pool *pool.Frames

	// Scratch slices reused by Fill and Receive, so neither allocates.
	fill     []packetio.Desc
	addrs    []uint64
	received []packetio.Desc

	poolEmpty atomic.Uint64

	cqn     uint32
	entries int

	// Placement, done once by whichever goroutine first drives this queue.
	pinOnce sync.Once
	pinCPU  atomic.Int64
	pinErr  atomic.Pointer[error]

	index int

	// closed is atomic and ringMu guards the queue memory, so that Close can
	// tear a queue down while another goroutine sits in Poll. Poll is the only
	// method that may run concurrently with Close -- BACKENDS.md requires a
	// blocked poller to be woken -- and it reads the completion queue, which
	// Close unmaps. A plain bool would be a race by the memory model whatever
	// the compiler emits today, and the unmapped read is a fault the race
	// detector never sees.
	//
	// Neither is on the packet path: Receive and Fill take nothing, and are
	// not safe to run during Close.
	ringMu sync.Mutex
	closed atomic.Bool
}

func (d *Device) newRxQueue(index, firstFrame, frames int) (*RxQueue, error) {
	dvq, err := d.dev.CreateRxQueue(d.cfg.rxDepth)
	if err != nil {
		return nil, err
	}

	r, err := ring.NewRx(ring.RxConfig{
		RQ:         dvq.RQ,
		CQ:         dvq.CQ,
		RQDbrec:    dvq.RQDbrec,
		CQDbrec:    dvq.CQDbrec,
		Stride:     dvq.Stride,
		LKey:       d.dev.LKey(),
		Region:     d.region.b,
		RegionVA:   d.region.va(),
		BufferSize: uint32(d.cfg.frameSize),
	})
	if err != nil {
		dvq.Close()
		return nil, err
	}

	q := &RxQueue{
		dev:     d,
		dvq:     dvq,
		ring:    r,
		pool:    pool.New(firstFrame, frames, d.cfg.frameSize),
		index:   index,
		cqn:     dvq.CQN,
		entries: r.Entries(),
	}
	q.fill = make([]packetio.Desc, 0, r.Entries())
	q.addrs = make([]uint64, 0, r.Entries())
	q.received = make([]packetio.Desc, 0, r.Entries())
	return q, nil
}

// Region is the frame memory this queue receives into.
func (q *RxQueue) Region() packetio.Region { return q.dev.region }

// Pin places the calling goroutine on a processor of its own and reports which
// one, or -1 if the device was opened with WithoutAffinity. The packet path
// calls this itself the first time the queue is used.
func (q *RxQueue) Pin() (int, error) { return q.autoPin() }

// PinnedCPU is the processor this queue's worker was placed on, or -1.
func (q *RxQueue) PinnedCPU() int {
	if q.pinCPU.Load() == 0 {
		return -1
	}
	return int(q.pinCPU.Load()) - 1
}

func (q *RxQueue) autoPin() (int, error) {
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

// Fill posts up to n frames for the NIC to receive into and returns how many it
// posted. A receiver that stops filling stops receiving: the NIC drops what it
// has nowhere to put, and the port's counters are where that shows up.
func (q *RxQueue) Fill(n int) int {
	if q.closed.Load() || n <= 0 {
		return 0
	}
	q.autoPin()
	if free := q.ring.FreeSlots(); n > free {
		n = free
	}
	if have := q.pool.Len(); n > have {
		if have == 0 {
			q.poolEmpty.Add(1)
			return 0
		}
		n = have
	}
	if n == 0 {
		return 0
	}

	q.addrs = q.pool.Pop(n, q.addrs[:0])
	q.fill = q.fill[:0]
	for _, addr := range q.addrs {
		q.fill = append(q.fill, packetio.Desc{Addr: addr})
	}
	posted := q.ring.Fill(q.fill)

	// Anything the queue would not take goes straight back, rather than being
	// held by nobody.
	for _, d := range q.fill[posted:] {
		q.pool.Push(q.pool.Base(d.Addr))
	}
	return posted
}

// Poll waits until at least one packet has arrived or timeout elapses, and
// returns how many are ready.
//
// There is nothing to sleep on: the NIC does not interrupt a queue that has not
// asked to be told, and this one does not ask, so waiting means looking. A
// receiver in a loop of its own should call Receive directly and skip this.
func (q *RxQueue) Poll(timeout time.Duration) (int, error) {
	if q.closed.Load() {
		return 0, packetio.ErrClosed
	}
	deadline := time.Now().Add(timeout)
	for {
		// ringMu is held only around the look at the completion queue, so that
		// Close cannot unmap it in between. Close takes the same lock, and this
		// is not the packet path: a receiver that cares about cost calls
		// Receive directly, as the comment above says.
		n, closed := q.readyLocked()
		if closed {
			return 0, packetio.ErrClosed
		}
		if n > 0 {
			return n, nil
		}
		if timeout == 0 || (timeout > 0 && !time.Now().Before(deadline)) {
			return 0, nil
		}
		// A negative timeout means wait indefinitely, and there is nothing
		// here to sleep on, so this spins. Notice a Close rather than spinning
		// forever: Capabilities.BlockingPoll is false for this backend, which
		// is the standing warning that a negative timeout burns a core.
	}
}

// readyLocked looks at the completion queue under ringMu, and reports that the
// queue was closed rather than reading memory that Close has unmapped.
func (q *RxQueue) readyLocked() (n int, closed bool) {
	q.ringMu.Lock()
	defer q.ringMu.Unlock()
	if q.closed.Load() {
		return 0, true
	}
	return q.ring.ReadyCount(q.entries), false
}

// Receive takes up to max received packets. The frames belong to the caller
// until Recycle. The returned slice is owned by the queue and is reused by the
// next call.
//
// Frames whose packet failed do not appear: they hold nothing worth looking at
// and go straight back to the pool.
func (q *RxQueue) Receive(max int) []packetio.Desc {
	if q.closed.Load() {
		return nil
	}
	q.autoPin()
	q.received = q.ring.Receive(max, q.received[:0])
	for _, d := range q.ring.Failed() {
		q.pool.Push(q.pool.Base(d.Addr))
	}
	return q.received
}

// Recycle returns received frames to the pool.
func (q *RxQueue) Recycle(descs []packetio.Desc) {
	for _, d := range descs {
		q.pool.Push(q.pool.Base(d.Addr))
	}
}

// NumFreeFillSlots is how many more frames the queue can hold.
func (q *RxQueue) NumFreeFillSlots() int {
	if q.closed.Load() {
		return 0
	}
	return q.ring.FreeSlots()
}

// NumReceived is how many packets are ready right now.
func (q *RxQueue) NumReceived() int {
	if q.closed.Load() {
		return 0
	}
	return q.ring.ReadyCount(q.entries)
}

// NumOutstanding is how many frames the NIC currently holds.
func (q *RxQueue) NumOutstanding() int { return q.ring.Outstanding() }

// NumFreeFrames is how many frames are on the free list.
func (q *RxQueue) NumFreeFrames() int { return q.pool.Len() }

// Stats reports what this queue has done. It reads only counters, so it is safe
// to call from another goroutine.
func (q *RxQueue) Stats() (packetio.RxStats, error) {
	s := &q.ring.Stats
	return packetio.RxStats{
		Packets:     s.Packets.Load(),
		Bytes:       s.Bytes.Load(),
		Filled:      s.Filled.Load(),
		Batches:     s.Batches.Load(),
		Completions: s.Completions.Load(),
		Errors:      s.Errors.Load(),
		PoolEmpty:   q.poolEmpty.Load(),
		Backend: map[string]uint64{
			"cqn":        uint64(q.cqn),
			"rq_entries": uint64(q.entries),
			// The gauge published at the last Sync, not the live cursors: the
			// cursors belong to the queue's own goroutine.
			"outstanding": s.Outstanding.Load(),
			// See the note on the transmit queue: anything but zero is a bug
			// above the pool.
			"pool_rejected": q.pool.Rejected(),
			// Completions whose byte count was longer than the buffer the NIC
			// was given, which means hardware and software disagree about the
			// queue.
			"oversize": q.ring.Stats.Oversize.Load(),
		},
	}, nil
}

// Err reports the error that took this queue out of service, or nil.
//
// A receive queue stops for two reasons that look identical from outside: the
// link is quiet, or the queue and the card have lost track of each other and
// nothing will arrive again. This is how to tell them apart.
func (q *RxQueue) Err() error {
	if q.closed.Load() {
		return packetio.ErrClosed
	}
	if err := q.ring.Err(); err != nil {
		return fmt.Errorf("%w: %v", packetio.ErrQueueFailed, err)
	}
	return nil
}

// Close releases the queue. Use Device.Close to shut down a whole device.
func (q *RxQueue) Close() error { return q.close() }

func (q *RxQueue) close() error {
	if q.closed.Swap(true) {
		return nil
	}
	// Take ringMu so that a goroutine in Poll has left the completion queue
	// before it is unmapped. It sees the flag on its next turn round the loop.
	q.ringMu.Lock()
	defer q.ringMu.Unlock()
	if q.dvq == nil {
		return nil
	}
	err := q.dvq.Close()
	q.dvq = nil
	return err
}
