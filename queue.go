package packetio

import "time"

// TxQueue is one hardware transmit queue.
//
// A TxQueue is owned by one goroutine. The transmit cycle is:
//
//	q.Complete(q.NumCompleted())    // reclaim frames the NIC is done with
//	descs := q.Alloc(n)             // take frames from the pool
//	for i := range descs {          // fill them
//	        b := q.Region().Writable(descs[i])
//	        descs[i].Len = uint32(build(b))
//	}
//	sent := q.Transmit(descs)       // hand them to the NIC
//
// Alloc never returns more frames than the following Transmit can accept, so a
// caller that transmits exactly what it allocated can never leak a frame.
//
// A forwarder sends frames it received rather than frames it allocated. Every
// queue of a Device shares one Region, so that needs no copy, but the frames
// belong to the receive queue's pool and must find their way back there:
//
//	descs := rx.Receive(n)          // frames from the receive pool
//	... rewrite them in place, set Len ...
//	sent := tx.Transmit(descs)      // the NIC owns them now
//	back = tx.Reclaim(max, back[:0])
//	rx.Recycle(back)                // home again
//	rx.Fill(rx.NumFreeFillSlots())
//
// Complete would put them on the transmit pool instead, where the receive
// queue can never find them again. A receive queue and every transmit queue
// that carries its frames must be driven by the same goroutine, because the
// pools are not synchronised.
type TxQueue interface {
	// Region is the frame memory this queue draws from.
	//
	// Whether it is the same Region as another queue's is
	// Capabilities.SharedRegion. Where it is, a frame received on one queue
	// may be transmitted on another without a copy; where it is not -- AF_XDP
	// maps one region per socket -- moving a frame between queues means
	// copying it. A queue refuses a foreign descriptor where it can tell --
	// the address is outside its region -- but two regions of one size look
	// alike to a bounds check, so keeping descriptors with the device they
	// came from is the caller's half of the contract.
	Region() Region

	// Alloc takes up to n frames from the queue's free pool and returns
	// descriptors for them, with Len set to zero. It returns fewer than n, or
	// none at all, when the pool or the transmit ring is short.
	//
	// The returned slice is owned by the queue and is reused by the next call
	// to Alloc; copy it if it must outlive that.
	Alloc(n int) []Desc

	// Transmit hands descriptors to the NIC and returns how many it accepted,
	// always a prefix of descs. Frames in the accepted prefix now belong to
	// the NIC and must not be touched until Complete or Reclaim returns them.
	// Frames in the unaccepted suffix still belong to the caller, who must
	// transmit them later or return them with Free. No backend returns an
	// accepted frame to its pool on its own.
	//
	// Every descriptor must name at least one byte inside one frame of the
	// Region. A backend that checks refuses the first descriptor that does
	// not, returning the prefix before it, so a short return with a free ring
	// points at the offending descriptor.
	//
	// Transmit publishes the batch to the hardware before it returns; there is
	// no separate kick or flush step.
	//
	// A short return is backpressure, not an error: the ring is full, or a
	// descriptor was refused. Ask Err whether the queue is still alive.
	Transmit(descs []Desc) int

	// Err reports that the queue is out of service, or nil while it is
	// healthy. It wraps ErrQueueFailed, and is safe to call from any
	// goroutine.
	//
	// A dead queue and a full one both make Transmit return zero, so without
	// this a caller cannot tell "try again in a moment" from "this will never
	// work again" -- and the second one looks exactly like a slow link
	// forever. A caller that loops on Transmit should ask after a run of
	// zeroes. Close it and open a new device; nothing revives a failed queue.
	Err() error

	// Complete reclaims frames whose transmission has finished, returning them
	// to the pool, and reports how many it reclaimed.
	//
	// max bounds the work rather than the result: a backend stops looking once
	// it has that many, but it will not split what one completion covers, and
	// on a backend that reports one completion per batch that means a whole
	// batch comes back at once. Pass the largest number that is useful and
	// treat the result as the answer.
	Complete(max int) int

	// Reclaim is Complete for frames that belong somewhere else: it takes up
	// to max frames whose transmission has finished and appends them to out
	// instead of returning them to this queue's pool. The caller owns them
	// and must Recycle them to the receive queue they came from, or Free them
	// here. max bounds the work the same way it does for Complete.
	Reclaim(max int, out []Desc) []Desc

	// Free returns frames to the pool without transmitting them. It is for
	// descriptors Alloc handed out that will not be sent, and for the suffix
	// Transmit did not accept.
	Free(descs []Desc)

	// NumCompleted is how many frames Complete would reclaim right now.
	NumCompleted() int

	// NumInFlight is how many frames the NIC currently owns.
	NumInFlight() int

	// NumFreeSlots is how many more frames the transmit ring can accept.
	NumFreeSlots() int

	// NumFreeFrames is how many frames are in the free pool.
	NumFreeFrames() int

	// SendFunc is the whole transmit cycle in one call: it reclaims
	// completions, takes up to count frames, calls build for each, and
	// transmits them. build writes into frame and returns the packet length.
	// Zero means there is nothing more to send: the batch ends there and what
	// was built is transmitted. A length below zero or past len(frame) is
	// ErrBadLength: the entire batch is abandoned, every frame returns to the
	// pool, and nothing is transmitted. SendFunc reports how many packets
	// reached the NIC.
	SendFunc(count int, build func(i int, frame []byte) int) (int, error)

	// Stats reports counters for this queue. Like Err, it is safe to call
	// from another goroutine while the queue's own goroutine drives it --
	// that is what it is for, and every example does it.
	Stats() (TxStats, error)

	// Close releases the queue. It is not safe to call while another goroutine
	// is inside any other method of this queue.
	Close() error
}

// RxQueue is one hardware receive queue.
//
// An RxQueue is owned by one goroutine. The receive cycle is:
//
//	q.Fill(q.NumFreeFillSlots())    // give the NIC frames to receive into
//	n, err := q.Poll(timeout)       // wait for packets
//	descs := q.Receive(n)           // take them
//	... use descs ...
//	q.Recycle(descs)                // give the frames back
//
// A backend may need frames posted before it can receive anything, so Fill
// comes first, and a receiver that never recycles will starve itself.
type RxQueue interface {
	// Region is the frame memory this queue receives into.
	Region() Region

	// Fill posts up to n frames from the pool for the NIC to receive into and
	// returns how many it posted.
	Fill(n int) int

	// Poll waits until at least one packet has arrived or timeout elapses, and
	// returns how many packets are ready.
	//
	// A zero timeout polls without blocking. A negative timeout waits
	// indefinitely, and is only useful on a backend whose Close can wake it --
	// Capabilities.BlockingPoll says which. Backends that have no way to block
	// spin for up to timeout.
	//
	// Poll returns ErrClosed if the queue is closed while it waits.
	Poll(timeout time.Duration) (int, error)

	// Receive takes up to max received packets and returns their descriptors.
	// The frames belong to the caller until Recycle. The returned slice is
	// owned by the queue and is reused by the next call.
	Receive(max int) []Desc

	// Err reports that the queue is out of service, or nil while it is
	// healthy. It wraps ErrQueueFailed, and is safe to call from any
	// goroutine.
	//
	// A receive queue that has stopped looks exactly like a quiet link:
	// Receive returns nothing either way, and the two want opposite
	// responses. A receiver that has seen no packets for a while should ask.
	Err() error

	// Recycle returns received frames to the pool.
	Recycle(descs []Desc)

	// NumFreeFillSlots is how many more frames the receive ring can hold.
	NumFreeFillSlots() int

	// NumReceived is how many packets are ready for Receive right now.
	NumReceived() int

	// NumFreeFrames is how many frames are in the free pool.
	NumFreeFrames() int

	// Stats reports counters for this queue. Like Err, it is safe to call
	// from another goroutine while the queue's own goroutine drives it.
	Stats() (RxStats, error)

	// Close releases the queue.
	Close() error
}

// Device is a NIC opened for packet I/O: a set of queues sharing one Region.
type Device interface {
	// Capabilities describes what this backend and this NIC can do.
	Capabilities() Capabilities

	// NumTxQueues and NumRxQueues are how many queues were opened.
	NumTxQueues() int
	NumRxQueues() int

	// TxQueue and RxQueue return queue i, or nil if i is out of range.
	TxQueue(i int) TxQueue
	RxQueue(i int) RxQueue

	// Close shuts down every queue and releases the Region. No queue method may
	// be running in another goroutine.
	Close() error
}

// Capabilities reports what a Device supports. A field that is false or zero
// means "not available here", never "unknown".
type Capabilities struct {
	// Backend names the implementation: "mlx5", "afxdp", "afpacket", "dpdk".
	Backend string

	// ZeroCopy is true when the NIC DMAs directly to and from the Region, with
	// no copy in the kernel.
	ZeroCopy bool

	// KernelCoexistence is true when the kernel keeps its own interface to
	// this device while it is open: packets the steering filter does not
	// match still reach Linux, so SSH, ARP and monitoring carry on. It is
	// false when opening the device took the port away from the kernel
	// entirely -- then nothing but these queues sees the wire, whatever the
	// filter says. A property of the opened device, not the backend: dpdk
	// answers true on a bifurcated ConnectX and false on an Intel card bound
	// to vfio-pci. A program must not open a device it also manages itself
	// over -- its management NIC -- unless this is true.
	KernelCoexistence bool

	// MultiBuffer is true when a packet may span several frames, marked with
	// OptContinued.
	MultiBuffer bool

	// RSS is true when receive traffic can be spread over several queues by a
	// per-flow hash -- the NIC's RSS, or the kernel's fanout hash where the
	// backend is a packet socket. What a caller may rely on is the spreading,
	// and that one flow stays on one queue; not where the hash is computed.
	RSS bool

	// TxChecksumOffload is true when the NIC can compute L3 and L4 checksums on
	// transmit.
	TxChecksumOffload bool

	// Offload is true when the queues implement [OffloadReceiver] and
	// [OffloadTransmitter], so segmentation and checksum metadata travels with
	// each frame and a super-frame can cross this device whole.
	Offload bool

	// RxChecksumFlags is true when received descriptors carry OptChecksumOK.
	RxChecksumFlags bool

	// BlockingPoll is true when RxQueue.Poll can sleep rather than spin.
	BlockingPoll bool

	// SharedRegion is true when all queues draw on one Region, so a frame can
	// move between queues without a copy.
	SharedRegion bool

	// HandsBackFrames is true when TxQueue.Reclaim can name the frames it
	// reclaimed and hand them to the caller.
	//
	// Where it is false the backend cannot see which frames came back -- AF_XDP
	// drains its completion ring straight into a pool -- so Reclaim completes
	// and returns nothing, and a forwarder must arrange for the frames to reach
	// the receive side another way. A forwarder written to the interface should
	// check this rather than discover it as a queue that slowly stops
	// receiving.
	HandsBackFrames bool

	// MaxFrameSize is the largest single frame, and MaxQueues the most queues
	// of either direction that can be opened: the backend's ceiling, lowered
	// to the device's own limit where the backend can see it.
	MaxFrameSize int
	MaxQueues    int
}
