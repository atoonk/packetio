//go:build linux && cgo && dpdk && amd64

package dpdk

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/dpdk/internal/eal"
	"github.com/atoonk/packetio/dpdk/internal/queue"
	"github.com/atoonk/packetio/internal/affinity"
)

// region is the frame memory, as packetio describes it.
//
// FrameSize is the whole frame, not the packet area: the descriptors this
// backend hands out already start past the mbuf, and Writable runs from there
// to the end of the frame. A caller who never looks below a descriptor's own
// address cannot reach anything the driver owns.
type region struct{ d *Device }

func (r *region) Bytes() []byte  { return r.d.region.Bytes }
func (r *region) FrameSize() int { return r.d.layout.FrameSize }
func (r *region) NumFrames() int { return r.d.cfg.frames }

// Frame is the bytes a descriptor names, or nil when it names anything else.
func (r *region) Frame(d packetio.Desc) []byte {
	b := r.d.region.Bytes
	if d.Addr >= uint64(len(b)) || d.Addr+uint64(d.Len) > uint64(len(b)) {
		return nil
	}
	return b[d.Addr : d.Addr+uint64(d.Len) : d.Addr+uint64(d.Len)]
}

// Writable is everything from a descriptor's address to the end of its frame.
func (r *region) Writable(d packetio.Desc) []byte {
	b := r.d.region.Bytes
	if d.Addr >= uint64(len(b)) {
		return nil
	}
	end := r.d.layout.FrameOf(d.Addr) + uint64(r.d.layout.FrameSize)
	if end > uint64(len(b)) {
		end = uint64(len(b))
	}
	return b[d.Addr:end:end]
}

var _ packetio.Region = (*region)(nil)

// TxQueue is one transmit queue.
//
// It is owned by one goroutine. Nothing here is synchronised except the closed
// flag, which exists so that Close may be called from another one.
type TxQueue struct {
	// One logical queue may drive several hardware send queues, each with
	// its own mempool over its own slice of the region. Transmit deals each
	// batch to one ring and advances; the driver frees every mbuf back to
	// the ring that sent it, so frame ownership stays provable per ring.
	qs    []*queue.Tx
	pools []*eal.Mempool
	next  int // the ring the next Transmit goes to
	dev   *Device

	index   int
	pinOnce sync.Once
	pinCPU  atomic.Int64
	pinErr  atomic.Pointer[error]
	closed  atomic.Bool
}

// Region is the frame memory this queue draws on, shared with every other queue
// of the device.
func (q *TxQueue) Region() packetio.Region { return &region{d: q.dev} }

// Pin places the calling goroutine on a processor of its own and reports which,
// or -1 if the device was opened with WithoutAffinity. The packet path does it
// on first use.
func (q *TxQueue) Pin() (int, error) { return q.autoPin() }

// PinnedCPU is the processor this queue's worker was placed on, or -1.
func (q *TxQueue) PinnedCPU() int {
	if q.pinCPU.Load() == 0 {
		return -1
	}
	return int(q.pinCPU.Load()) - 1
}

func (q *TxQueue) autoPin() (int, error) {
	return pinOnce(&q.pinOnce, &q.pinCPU, &q.pinErr, q.dev.place)
}

// Alloc takes up to n frames from the free list of the ring the next
// Transmit will use, so a caller that transmits everything it allocated
// cannot leak a frame.
func (q *TxQueue) Alloc(n int) []packetio.Desc {
	if q.closed.Load() {
		return nil
	}
	q.autoPin()
	return q.qs[q.next].Alloc(n)
}

// Transmit hands descriptors to the driver and returns how many it took, always
// a prefix. Each batch goes to one ring; the next batch goes to the next.
func (q *TxQueue) Transmit(descs []packetio.Desc) int {
	if q.closed.Load() {
		return 0
	}
	q.autoPin()
	n := q.qs[q.next].Transmit(descs)
	if q.next++; q.next == len(q.qs) {
		q.next = 0
	}
	return n
}

// Complete returns finished frames to their rings' free lists.
func (q *TxQueue) Complete(max int) int {
	if q.closed.Load() {
		return 0
	}
	total := 0
	for _, t := range q.qs {
		if total >= max {
			break
		}
		total += t.Complete(max - total)
	}
	return total
}

// Reclaim hands finished frames to the caller instead, for frames that belong
// to a receive queue.
func (q *TxQueue) Reclaim(max int, out []packetio.Desc) []packetio.Desc {
	if q.closed.Load() {
		return out
	}
	before := len(out)
	for _, t := range q.qs {
		got := len(out) - before
		if got >= max {
			break
		}
		out = t.Reclaim(max-got, out)
	}
	return out
}

// TransmitOffload is Transmit with segmentation and checksum metadata for each
// frame, so the NIC computes the checksums and cuts a super-frame into
// MTU-sized segments. A zero Offload sends an ordinary frame.
//
// It implements [packetio.OffloadTransmitter]: it returns the accepted prefix,
// and an error naming the first descriptor whose Offload was refused --
// metadata that does not describe the packet it came with, or work this device
// was not opened for, which is [packetio.ErrUnsupported].
func (q *TxQueue) TransmitOffload(descs []packetio.Desc, offs []packetio.Offload) (int, error) {
	if q.closed.Load() {
		return 0, packetio.ErrClosed
	}
	if !q.dev.txChecksums {
		return 0, fmt.Errorf("%w: TransmitOffload needs WithChecksumOffload, and a device "+
			"that offers it", packetio.ErrUnsupported)
	}
	n, err := q.qs[q.next].TransmitOffload(descs, offs)
	if q.next++; q.next == len(q.qs) {
		q.next = 0
	}
	return n, err
}

// Free returns frames to the pool without transmitting them. Each frame goes
// back to the ring whose slice of the region it came from; one that belongs
// to none of them is handed to the first ring, whose pool refuses it and
// counts the refusal, which is more honest than dropping it silently.
func (q *TxQueue) Free(descs []packetio.Desc) {
	for i := range descs {
		owner := q.qs[0]
		for _, t := range q.qs {
			if t.Owns(descs[i].Addr) {
				owner = t
				break
			}
		}
		owner.Free(descs[i : i+1])
	}
}

// NumCompleted is how many frames Complete would hand back right now, without
// asking the driver.
func (q *TxQueue) NumCompleted() int {
	if q.closed.Load() {
		return 0
	}
	n := 0
	for _, t := range q.qs {
		n += t.NumCompleted()
	}
	return n
}

// NumInFlight is how many frames the driver currently owns, over every ring
// this queue drives.
func (q *TxQueue) NumInFlight() int {
	n := 0
	for _, t := range q.qs {
		n += t.NumInFlight()
	}
	return n
}

// NumFreeSlots is how many more frames the next Transmit will accept. That is
// one ring's room, not the sum over every ring: a batch goes to one ring, so
// the sum would promise space no single call can use.
func (q *TxQueue) NumFreeSlots() int {
	if q.closed.Load() {
		return 0
	}
	return q.qs[q.next].NumFreeSlots()
}

// NumFreeFrames is how many frames are on this queue's free lists.
func (q *TxQueue) NumFreeFrames() int {
	n := 0
	for _, t := range q.qs {
		n += t.NumFreeFrames()
	}
	return n
}

// SendFunc is the whole transmit cycle in one call: reclaim, allocate, build,
// transmit.
func (q *TxQueue) SendFunc(count int, build func(i int, frame []byte) int) (int, error) {
	if q.closed.Load() {
		return 0, packetio.ErrClosed
	}
	q.autoPin()
	// The whole cycle runs against one ring, and the next call takes the
	// next ring: while one drains, the others have room, and the core has
	// time to fill them.
	t := q.qs[q.next]
	if q.next++; q.next == len(q.qs) {
		q.next = 0
	}
	t.Complete(count + t.NumInFlight())

	descs := t.Alloc(count)
	if len(descs) == 0 {
		return 0, nil
	}
	r := &region{d: q.dev}
	built := 0
	for i := range descs {
		frame := r.Writable(descs[i])
		n := build(i, frame)
		if n == 0 {
			// Nothing more to send: the batch ends here.
			break
		}
		if n < 0 || n > len(frame) {
			t.Free(descs)
			return 0, fmt.Errorf("%w: %d bytes into a %d-byte frame",
				packetio.ErrBadLength, n, len(frame))
		}
		descs[i].Len = uint32(n)
		built++
	}
	if built < len(descs) {
		t.Free(descs[built:])
	}
	sent := t.Transmit(descs[:built])
	if sent < built {
		t.Free(descs[sent:built])
	}
	return sent, nil
}

// Err reports that the queue is out of service, or nil. Safe to call from a
// monitoring goroutine while the queue's own goroutine drives it.
//
// Operational shortfalls never land here: a driver refuses what it cannot
// take and says so by taking less, and the counters name the reason. What
// does land here, and stays forever, is any event after which this backend
// can no longer prove where every frame is -- an address from outside the
// region, or a free that did not fit the returned ring and was lost. Such a
// queue refuses further work; close the device and open a new one.
func (q *TxQueue) Err() error {
	if q.closed.Load() {
		return packetio.ErrClosed
	}
	for _, t := range q.qs {
		if err := t.Dead(); err != nil {
			return err
		}
	}
	for _, mp := range q.pools {
		if d := mp.Drops(); d > 0 {
			return fmt.Errorf("%w: %d frames were lost when the driver's frees did not fit the returned ring",
				packetio.ErrQueueFailed, d)
		}
	}
	return nil
}

// Stats reports what this queue has done. Safe to call from a monitoring
// goroutine while the queue's own goroutine drives it.
func (q *TxQueue) Stats() (packetio.TxStats, error) {
	var st packetio.TxStats
	var badDesc, foreign, badOffload, pokes, drops, rejected uint64
	for _, t := range q.qs {
		s := &t.Stats
		st.Packets += s.Packets.Load()
		st.Bytes += s.Bytes.Load()
		st.Completed += s.Completed.Load()
		st.Batches += s.Batches.Load()
		st.RingFull += s.RingFull.Load()
		st.PoolEmpty += s.PoolEmpty.Load()
		badDesc += s.BadDesc.Load()
		foreign += s.Foreign.Load()
		badOffload += s.BadOffload.Load()
		pokes += s.Pokes.Load()
		rejected += t.PoolRejected()
	}
	for _, mp := range q.pools {
		drops += mp.Drops()
	}
	st.Errors = badDesc + foreign + badOffload
	st.Backend = map[string]uint64{
		"in_flight": uint64(q.NumInFlight()),
		"rings":     uint64(len(q.qs)),
		// Descriptors refused because they did not name bytes inside one
		// frame's packet area. Never non-zero without a bug above.
		"bad_desc": badDesc,
		// Times the driver had to be asked to look at its completions.
		"pokes": pokes,
		// Addresses the driver returned that this region does not contain,
		// and frees that did not fit the ring. Both must be zero.
		"foreign": foreign,
		// Offload metadata refused: it did not describe the packet it
		// came with, or asked for work this device does not do.
		"bad_offload":   badOffload,
		"mempool_drops": drops,
		"pool_rejected": rejected,
	}
	return st, nil
}

// Close releases the queue. Use Device.Close to shut down a whole device.
func (q *TxQueue) Close() error {
	q.closed.Store(true)
	for _, t := range q.qs {
		t.Close()
	}
	return nil
}

// RxQueue is one receive queue.
type RxQueue struct {
	q    *queue.Rx
	dev  *Device
	pool *eal.Mempool

	index int

	// ringMu guards the question "may this goroutine still call the driver".
	// Close takes it, so a goroutine inside Poll has left the driver before
	// the port is stopped underneath it. It is not on the packet path: a
	// receiver that cares about cost calls Receive directly.
	ringMu sync.Mutex
	closed atomic.Bool

	// stash holds what a Poll took, so the packets it found are not lost. DPDK
	// has no way to ask whether a queue has something without taking it.
	stash []packetio.Desc

	pinOnce sync.Once
	pinCPU  atomic.Int64
	pinErr  atomic.Pointer[error]
}

// Region is the frame memory this queue receives into.
func (q *RxQueue) Region() packetio.Region { return &region{d: q.dev} }

// Pin places the calling goroutine on a processor of its own and reports which.
func (q *RxQueue) Pin() (int, error) { return q.autoPin() }

// PinnedCPU is the processor this queue's worker was placed on, or -1.
func (q *RxQueue) PinnedCPU() int {
	if q.pinCPU.Load() == 0 {
		return -1
	}
	return int(q.pinCPU.Load()) - 1
}

func (q *RxQueue) autoPin() (int, error) {
	return pinOnce(&q.pinOnce, &q.pinCPU, &q.pinErr, q.dev.place)
}

// Fill makes frames available for the driver to receive into and returns how
// many.
//
// The driver takes them when it wants them, in bulks it chooses, and a bulk it
// cannot satisfy in full it does not take at all -- so keep the supply
// comfortably full rather than topping it up a frame at a time.
func (q *RxQueue) Fill(n int) int {
	if q.closed.Load() {
		return 0
	}
	q.autoPin()
	q.q.DrainReturned()
	return q.q.Fill(n)
}

// Poll waits until at least one packet has arrived or timeout elapses.
//
// There is nothing to sleep on: no interrupt is armed for these queues, so
// waiting means looking, and what it finds it keeps for the next Receive
// because DPDK has no way to look without taking. Capabilities.BlockingPoll is
// false, which is the standing warning that a negative timeout burns a core.
func (q *RxQueue) Poll(timeout time.Duration) (int, error) {
	if q.closed.Load() {
		return 0, packetio.ErrClosed
	}
	if len(q.stash) > 0 {
		return len(q.stash), nil
	}
	deadline := time.Now().Add(timeout)
	for {
		n, closed := q.pollOnce()
		if closed {
			return 0, packetio.ErrClosed
		}
		if n > 0 {
			return n, nil
		}
		if timeout == 0 || (timeout > 0 && !time.Now().Before(deadline)) {
			return 0, nil
		}
		// A negative timeout waits indefinitely, and there is nothing here to
		// sleep on, so this spins -- noticing a Close rather than spinning for
		// ever.
	}
}

// pollOnce looks at the driver under ringMu, so that Close cannot stop the port
// while the driver is being called.
func (q *RxQueue) pollOnce() (n int, closed bool) {
	q.ringMu.Lock()
	defer q.ringMu.Unlock()
	if q.closed.Load() {
		return 0, true
	}
	got := q.q.Receive(pollBatch)
	if len(got) > 0 {
		q.stash = append(q.stash, got...)
	}
	return len(q.stash), false
}

// pollBatch is how many packets one look takes. It is a batch rather than one
// so that a poll on a busy queue is not a pessimisation.
const pollBatch = 64

// Receive takes up to max received packets. The frames belong to the caller
// until Recycle.
func (q *RxQueue) Receive(max int) []packetio.Desc {
	if q.closed.Load() || max <= 0 {
		return nil
	}
	q.autoPin()
	if len(q.stash) > 0 {
		// Whatever a Poll took comes first, or those packets would be lost.
		if max >= len(q.stash) {
			out := q.stash
			q.stash = q.stash[:0:0]
			return out
		}
		out := q.stash[:max:max]
		q.stash = q.stash[max:]
		return out
	}
	return q.q.Receive(max)
}

// Recycle returns received frames to the pool.
func (q *RxQueue) Recycle(descs []packetio.Desc) { q.q.Recycle(descs) }

// NumFreeFillSlots is how many more frames the supply will accept.
func (q *RxQueue) NumFreeFillSlots() int {
	if q.closed.Load() {
		return 0
	}
	return q.q.NumFreeFillSlots()
}

// NumReceived is how many packets are ready right now. DPDK has no way to ask
// without taking, so this takes them and keeps them for the next Receive.
func (q *RxQueue) NumReceived() int {
	if q.closed.Load() {
		return 0
	}
	n, _ := q.pollOnce()
	return n
}

// NumFreeFrames is how many frames are on this queue's free list.
func (q *RxQueue) NumFreeFrames() int { return q.q.NumFreeFrames() }

// NumOutstanding is how many frames the driver currently holds.
func (q *RxQueue) NumOutstanding() int { return q.q.NumOutstanding() }

// Err reports that the queue is out of service, or nil. See TxQueue.Err for
// what ends up here and why it is permanent. Safe to call from a monitoring
// goroutine while the queue's own goroutine drives it.
func (q *RxQueue) Err() error {
	if q.closed.Load() {
		return packetio.ErrClosed
	}
	if err := q.q.Dead(); err != nil {
		return err
	}
	if d := q.pool.Drops(); d > 0 {
		return fmt.Errorf("%w: %d frames were lost when the driver's frees did not fit the returned ring",
			packetio.ErrQueueFailed, d)
	}
	return nil
}

// Stats reports what this queue has done. Safe to call from a monitoring
// goroutine while the queue's own goroutine drives it.
func (q *RxQueue) Stats() (packetio.RxStats, error) {
	s := &q.q.Stats
	chained, badLen, foreign := s.Chained.Load(), s.BadLen.Load(), s.Foreign.Load()
	return packetio.RxStats{
		Packets:   s.Packets.Load(),
		Bytes:     s.Bytes.Load(),
		Filled:    s.Filled.Load(),
		Batches:   s.Batches.Load(),
		PoolEmpty: s.PoolEmpty.Load(),
		Errors:    chained + badLen + foreign,
		Backend: map[string]uint64{
			"outstanding": uint64(q.q.NumOutstanding()),
			// Packets the driver reported as spanning several buffers, which
			// this backend refuses.
			"chained": chained,
			// Lengths the driver reported that do not fit their frame. A
			// separate count from chained: the causes differ, and one
			// counter named after only one of them sends the reader to the
			// wrong place.
			"bad_len": badLen,
			"foreign": foreign,
			// Bulk gets the driver could not satisfy: the supply ran dry, and
			// packets were dropped for want of a buffer.
			"supply_empty":  q.pool.Empty(),
			"mempool_drops": q.pool.Drops(),
			"pool_rejected": q.q.PoolRejected(),
		},
	}, nil
}

// Close releases the queue.
func (q *RxQueue) Close() error {
	q.shut()
	return nil
}

// shut stops the queue and waits for any poller to leave the driver.
func (q *RxQueue) shut() {
	if q.closed.Swap(true) {
		return
	}
	q.ringMu.Lock()
	defer q.ringMu.Unlock()
	q.q.Close()
}

// pinOnce places a worker the first time its queue is driven.
func pinOnce(once *sync.Once, cpu *atomic.Int64, errp *atomic.Pointer[error], p *affinity.Placement) (int, error) {
	once.Do(func() {
		c, err := p.Pin()
		if err != nil {
			err = fmt.Errorf("dpdk: %w", err)
			errp.Store(&err)
			return
		}
		if c >= 0 {
			cpu.Store(int64(c) + 1)
		}
	})
	if e := errp.Load(); e != nil {
		return -1, *e
	}
	if cpu.Load() == 0 {
		return -1, nil
	}
	return int(cpu.Load()) - 1, nil
}

var (
	_ packetio.TxQueue            = (*TxQueue)(nil)
	_ packetio.RxQueue            = (*RxQueue)(nil)
	_ packetio.OffloadTransmitter = (*TxQueue)(nil)
)
