//go:build linux

package afxdp

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"

	xdp "github.com/atoonk/go-afxdp"
	"github.com/atoonk/packetio"
)

// adapt turns a go-afxdp error into one a caller can branch on with errors.Is.
//
// The contract in BACKENDS.md is that errors.Is means the same thing on every
// backend, and go-afxdp does not know about packetio's sentinels: it returns
// net.ErrClosed for a closed socket and a plain formatted error for a build
// callback that returned a bad length. A caller writing errors.Is(err,
// packetio.ErrClosed) got false here and true on the other two backends, which
// is exactly the kind of quiet difference this package exists to stop.
func adapt(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, packetio.ErrClosed), errors.Is(err, net.ErrClosed):
		return fmt.Errorf("%w: %v", packetio.ErrClosed, err)
	case errors.Is(err, packetio.ErrBadLength), isBadLength(err):
		return fmt.Errorf("%w: %v", packetio.ErrBadLength, err)
	}
	return err
}

// isBadLength recognises go-afxdp's build-callback error, which is a formatted
// string rather than a sentinel. Matching on the text is unpleasant and is
// marked as such: if go-afxdp ever exports a sentinel, use it instead.
func isBadLength(err error) bool {
	return err != nil && strings.Contains(err.Error(), "build returned length")
}

// TxQueue is one AF_XDP socket's transmit side.
//
// It is owned by one goroutine, as everywhere in this API. The first call from
// that goroutine also places it on the queue's processor; see go-afxdp's Pin.
type TxQueue struct {
	s      *xdp.Socket
	region *region

	// Scratch reused by Alloc, Transmit and Complete, so none of them allocate.
	descs []packetio.Desc
	xdesc []xdp.Desc

	// Frames handed out by Alloc and given back by Free. go-afxdp has no way to
	// return one to its own pool, so they are kept here and Alloc draws on them
	// first. They come back to the socket when it is closed.
	spare []packetio.Desc

	// rejected counts descriptors refused by Free or Transmit as not naming
	// bytes inside one frame of this region. Anything but zero is a bug above:
	// the same pool_rejected the other backends report. Atomic because Stats
	// reads it from a monitoring goroutine while the owner writes it.
	rejected atomic.Uint64

	// regionLen and frameSize bound what Free and Transmit accept, captured at
	// construction. Zero means a queue built without a region -- only the unit
	// tests do that -- and then Free has nothing to check against.
	regionLen uint64
	frameSize uint64

	// dead is set when the device is closed, so that Err can say so. It is
	// atomic because Err may be called from a monitoring goroutine.
	dead atomic.Bool
}

// closed reports whether the device this queue belongs to has been closed.
// go-afxdp has no accessor for it, so the device sets it on the way down.
func (q *TxQueue) closed() bool { return q.dead.Load() }

// Region is the frame memory this queue draws on. Each queue has its own.
func (q *TxQueue) Region() packetio.Region { return q.region }

// Pin places the calling goroutine on this queue's processor and reports which
// one, or -1 if placement is off. The packet path does it on first use; a
// caller wanting to know before the first batch asks here.
func (q *TxQueue) Pin() (int, error) { return q.s.Pin() }

// Socket is the underlying go-afxdp socket, for what this API does not
// describe: the wakeup flags, the kernel's own ring counters, placement.
func (q *TxQueue) Socket() *xdp.Socket { return q.s }

// Alloc takes up to n frames from the queue's free pool.
func (q *TxQueue) Alloc(n int) []packetio.Desc {
	if n <= 0 {
		return nil
	}
	q.descs = q.descs[:0]
	for len(q.spare) > 0 && len(q.descs) < n {
		q.descs = append(q.descs, q.spare[len(q.spare)-1])
		q.spare = q.spare[:len(q.spare)-1]
	}
	if len(q.descs) < n {
		q.descs = toDescs(q.s.Alloc(n-len(q.descs)), q.descs)
	}
	if len(q.descs) > n {
		q.descs = q.descs[:n]
	}
	// The contract says a fresh descriptor has Len zero, and a caller that
	// trusts it would otherwise transmit whatever the last packet in that
	// frame was. go-afxdp leaves the length from the frame's previous life.
	for i := range q.descs {
		q.descs[i].Len = 0
		q.descs[i].Options = 0
	}
	return q.descs
}

// Transmit hands descriptors to the kernel and returns how many it took, always
// a prefix. It publishes the batch before returning; there is no separate kick.
//
// A descriptor that does not name at least one byte inside one frame ends the
// batch: the prefix before it is transmitted and the rest is left with the
// caller. That is checked here rather than left to the kernel, which counts a
// bad descriptor in tx_invalid_descs and carries on -- so the caller would get
// a full-length return and no way to know which descriptor was dropped.
func (q *TxQueue) Transmit(descs []packetio.Desc) int {
	n := len(descs)
	for i := 0; i < n; i++ {
		if !q.descOK(descs[i]) {
			n = i
			q.rejected.Add(1)
			break
		}
	}
	if n == 0 {
		return 0
	}
	q.xdesc = fromDescs(descs[:n], q.xdesc[:0])
	return q.s.Transmit(q.xdesc)
}

// descOK reports whether a descriptor names at least one byte inside one frame
// of this queue's region. See the note on the mlx5 backend's version: the same
// rule, for the same reason, and the arithmetic subtracts rather than adds
// because on a forwarding path the length came from the hardware.
func (q *TxQueue) descOK(d packetio.Desc) bool {
	if q.frameSize == 0 {
		return true // a queue built without a region; only the unit tests do that
	}
	return d.Addr < q.regionLen &&
		uint64(d.Len)-1 < q.frameSize-(d.Addr%q.frameSize)
}

// Complete reclaims up to max frames the kernel has finished with, returning
// them to the pool, and reports how many.
func (q *TxQueue) Complete(max int) int { return q.s.Complete(max) }

// Reclaim is Complete for frames that belong somewhere else.
//
// AF_XDP cannot name the frames it reclaimed: Complete drains the completion
// ring straight into a pool, and there is no way to see which addresses came
// back. So this completes and returns nothing, and a forwarder on this backend
// must be opened with go-afxdp's WithTxReuseRxFrames, which makes Complete
// return each frame to the pool its address belongs to rather than to the
// transmit pool. Without it a receive frame transmitted here leaks into the
// transmit pool and the receive side starves -- go-afxdp keeps a separate pool
// per direction, so this is a real trap and not a theoretical one.
//
// Capabilities.HandsBackFrames is false here, which is how a caller written
// against the interface finds this out rather than discovering it as a stall.
func (q *TxQueue) Reclaim(max int, out []packetio.Desc) []packetio.Desc {
	q.s.Complete(max)
	return out
}

// Free returns frames to the pool without transmitting them.
//
// A descriptor this queue's region does not contain is refused and counted, not
// appended: it would come back from a later Alloc as an address the kernel
// rejects, or worse, one belonging to another socket. Each AF_XDP queue maps
// its own region, so "another queue's frame" is a real and easy mistake here.
func (q *TxQueue) Free(descs []packetio.Desc) {
	for _, d := range descs {
		addr := d.Addr
		if q.frameSize != 0 {
			if addr >= q.regionLen {
				q.rejected.Add(1)
				continue
			}
			// A descriptor may point past its frame's start, so round down:
			// what goes back on the free list is always a whole frame.
			addr -= addr % q.frameSize
		}
		q.spare = append(q.spare, packetio.Desc{Addr: addr})
	}
}

// NumCompleted is how many frames Complete would reclaim right now.
func (q *TxQueue) NumCompleted() int { return q.s.NumCompleted() }

// NumInFlight is how many frames the kernel currently owns.
func (q *TxQueue) NumInFlight() int { return q.s.NumTransmitted() }

// NumFreeSlots is how many more frames the transmit ring can accept.
func (q *TxQueue) NumFreeSlots() int { return q.s.NumFreeTxSlots() }

// NumFreeFrames is how many frames are in the free pool.
func (q *TxQueue) NumFreeFrames() int { return q.s.FreeTxFrames() + len(q.spare) }

// SendFunc is the whole transmit cycle in one call.
func (q *TxQueue) SendFunc(count int, build func(i int, frame []byte) int) (int, error) {
	n, err := q.s.SendFunc(count, build)
	return n, adapt(err)
}

// Err reports that the queue is out of service, or nil.
//
// AF_XDP has no failure that outlives one call: the kernel refuses a bad
// descriptor and carries on, and its counters say how often. So this reports
// only that the socket is closed, which is the honest answer here.
func (q *TxQueue) Err() error {
	if q.closed() {
		return packetio.ErrClosed
	}
	return nil
}

// Stats reports what this queue has done.
func (q *TxQueue) Stats() (packetio.TxStats, error) {
	s, err := q.s.Stats()
	if err != nil {
		return packetio.TxStats{}, err
	}
	return packetio.TxStats{
		Packets: s.Transmitted,
		Backend: map[string]uint64{
			"in_flight":       s.Transmitted - s.Completed,
			"tx_invalid_desc": s.KernelStats.Tx_invalid_descs,
			"tx_ring_empty":   s.KernelStats.Tx_ring_empty_descs,
			"pool_rejected":   q.rejected.Load(),
		},
	}, nil
}

// Close releases the queue. The socket is shared with the receive side and is
// closed with the device.
func (q *TxQueue) Close() error { return nil }

// RxQueue is one AF_XDP socket's receive side.
type RxQueue struct {
	s      *xdp.Socket
	region *region

	descs []packetio.Desc
	xdesc []xdp.Desc

	// batches counts receive calls that returned something. The socket
	// reports polls, which is not the same question: a Receive that takes
	// packets without polling is still a batch, and packets over batches is
	// the number that says whether the loop is taking them in useful sizes.
	// Atomic because Stats reads it from a monitoring goroutine while the
	// queue's own goroutine bumps it, once per batch.
	batches atomic.Uint64

	// dead is set when the device is closed; see the note on TxQueue.
	dead atomic.Bool
}

// Region is the frame memory this queue receives into.
func (q *RxQueue) Region() packetio.Region { return q.region }

// Pin places the calling goroutine on this queue's processor and reports which.
func (q *RxQueue) Pin() (int, error) { return q.s.Pin() }

// Socket is the underlying go-afxdp socket.
func (q *RxQueue) Socket() *xdp.Socket { return q.s }

// Fill posts up to n frames for the kernel to receive into and returns how
// many it posted. A receiver that stops filling stops receiving.
//
// Fill also wakes the driver when it has parked. go-afxdp's Fill only writes
// the ring -- its Poll is what restarts a parked driver -- but the packetio
// contract is that Fill/Receive with no Poll must work, because that is how
// every other backend is driven. Without this kick, a queue whose NAPI loop
// completed while the ring was quiet never posts another descriptor: measured
// on the ConnectX-6 Dx, seven of eight queues took exactly their initial
// 1,024 frames and then nothing, for hours, while the port dropped 2.5
// billion packets and the fill rings sat provably full. The check is one
// atomic load when the driver is running; the recvfrom is paid only when it
// parked.
func (q *RxQueue) Fill(n int) int {
	posted := q.s.Fill(n)
	if q.s.NeedsWakeupRx() {
		q.s.WakeupRx()
	}
	return posted
}

// Poll waits until at least one packet has arrived or timeout elapses. Unlike
// the mlx5 backend this really sleeps rather than spinning.
func (q *RxQueue) Poll(timeout time.Duration) (int, error) {
	if q.dead.Load() {
		return 0, packetio.ErrClosed
	}
	n, err := q.s.Poll(timeout)
	return n, adapt(err)
}

// Receive takes up to max received packets. The returned slice is owned by the
// queue and reused by the next call.
func (q *RxQueue) Receive(max int) []packetio.Desc {
	q.descs = toDescs(q.s.Receive(max), q.descs[:0])
	if len(q.descs) > 0 {
		q.batches.Add(1)
	}
	return q.descs
}

// Recycle returns received frames to the pool.
func (q *RxQueue) Recycle(descs []packetio.Desc) {
	q.xdesc = fromDescs(descs, q.xdesc[:0])
	q.s.Recycle(q.xdesc)
}

// Err reports that the queue is out of service, or nil.
//
// AF_XDP has no failure that outlives one call: the kernel drops what it has
// nowhere to put and counts it. So this reports only that the socket is
// closed.
func (q *RxQueue) Err() error {
	if q.dead.Load() {
		return packetio.ErrClosed
	}
	return nil
}

// NumFreeFillSlots is how many more frames the receive ring can hold.
func (q *RxQueue) NumFreeFillSlots() int { return q.s.NumFreeFillSlots() }

// NumReceived is how many packets are ready right now. AF_XDP has no way to ask
// without taking them, so this polls without waiting.
func (q *RxQueue) NumReceived() int {
	n, err := q.s.Poll(0)
	if err != nil {
		return 0
	}
	return n
}

// NumFreeFrames is how many frames are in the free pool.
func (q *RxQueue) NumFreeFrames() int { return q.s.FreeRxFrames() }

// Stats reports what this queue has done.
func (q *RxQueue) Stats() (packetio.RxStats, error) {
	s, err := q.s.Stats()
	if err != nil {
		return packetio.RxStats{}, err
	}
	return packetio.RxStats{
		Packets: s.Received,
		Batches: q.batches.Load(),
		Backend: map[string]uint64{
			"outstanding":        s.Filled - s.Received,
			"rx_dropped":         s.KernelStats.Rx_dropped,
			"rx_ring_full":       s.KernelStats.Rx_ring_full,
			"rx_fill_ring_empty": s.KernelStats.Rx_fill_ring_empty_descs,
			// Times Fill woke a parked driver. Zero under load with the
			// port's rx_out_of_buffer climbing was the signature of the
			// dead-queue bug this counter exists to keep visible.
			"rx_kicks": s.RxKicks,
		},
	}, nil
}

// Close releases the queue; the socket is closed with the device.
func (q *RxQueue) Close() error { return nil }

// The two methods below are AF_XDP's own, not part of the backend-neutral API.
// They exist because a packet may span several frames here, which the common
// Desc cannot describe on its own, and an application that sends or receives
// jumbo frames needs them. Reach them by type asserting for the interface you
// need, the way packetio.OffloadReceiver is reached.

// SendBatch transmits whole payloads, splitting any that do not fit a frame
// across several. It reports how many payloads went out.
func (q *TxQueue) SendBatch(payloads [][]byte) (int, error) { return q.s.SendBatch(payloads) }

// ReceivePackets takes up to maxFrames frames' worth of packets, each returned
// as the run of frames it occupies. Do not mix it with Receive on one queue:
// they consume the same ring and count differently.
func (q *RxQueue) ReceivePackets(maxFrames int) []xdp.Packet { return q.s.ReceivePackets(maxFrames) }

// RecyclePackets returns whole packets, however many frames each took.
func (q *RxQueue) RecyclePackets(pkts []xdp.Packet) { q.s.RecyclePackets(pkts) }
