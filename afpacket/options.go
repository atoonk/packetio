//go:build linux

package afpacket

import "fmt"

// maxQueues bounds a fanout group. The kernel's own limit is higher, but a
// fanout group this wide on a backend this slow is a configuration mistake
// rather than a plan.
const maxQueues = 64

type config struct {
	txQueues     int
	rxQueues     int
	frames       int
	frameSize    int
	socketBuffer int
	promiscuous  bool
	gso          bool
	// framesSet and frameSizeSet record that the caller chose these, so
	// WithGSO knows whether it may change them. Without that, the answer
	// would depend on the order the options were given in.
	framesSet    bool
	frameSizeSet bool

	// multiBuffer lets a received packet span several frames; see
	// WithMultiBuffer.
	multiBuffer bool
}

func defaults() config {
	return config{
		txQueues:  1,
		rxQueues:  1,
		frames:    4096,
		frameSize: 2048,
		// 8 MiB each way. The mapped ring absorbs receive bursts, so this
		// mostly matters for transmit.
		socketBuffer: 8 << 20,
	}
}

func (c config) validate() error {
	if c.txQueues < 0 || c.rxQueues < 0 {
		return fmt.Errorf("afpacket: %d transmit and %d receive queues", c.txQueues, c.rxQueues)
	}
	if c.txQueues == 0 && c.rxQueues == 0 {
		return fmt.Errorf("afpacket: a device with no queues at all does nothing")
	}
	if c.txQueues > maxQueues || c.rxQueues > maxQueues {
		return fmt.Errorf("afpacket: at most %d queues either way", maxQueues)
	}
	if c.frameSize&(c.frameSize-1) != 0 || c.frameSize < 2*frameHeadroom {
		// At least two headrooms: a packet starts frameHeadroom into its
		// frame, so a frame that size or smaller would put the descriptor at
		// the base of the NEXT frame -- which the pool still believes is
		// free, and would hand to a second owner. Refuse it here rather than
		// hand out two descriptors for one piece of memory.
		return fmt.Errorf("afpacket: frame size %d must be a power of two of at least %d "+
			"(a packet starts %d bytes into its frame)", c.frameSize, 2*frameHeadroom, frameHeadroom)
	}
	switch {
	case c.gso && c.rxQueues > 0 && !c.multiBuffer && c.frameSize < 1<<16:
		// A super-frame arrives whole, so it needs somewhere whole to go:
		// either one frame that holds it, or -- with WithMultiBuffer -- a
		// chain of small ones. Transmit never needed the room, because a
		// packet given as a chain is gathered by the kernel, which is what a
		// forwarder wants when its bytes already sit in small buffers: there,
		// one descriptor per 64 KB costs a copy that one descriptor per
		// buffer does not.
		//
		// The headroom comes off the top: a 65536-byte frame carries a
		// super-frame of 65472, which Capabilities.MaxFrameSize reports and
		// anything larger is counted oversize rather than truncated.
		return fmt.Errorf("afpacket: WithGSO with receive queues needs a frame size of at "+
			"least 65536, not %d -- or WithMultiBuffer, which lays an arriving "+
			"super-frame across small frames instead. A transmit-only device needs "+
			"neither: a packet given as a chain is gathered by the kernel.", c.frameSize)
	case !c.gso && c.frameSize > rxFrameSize:
		// A received frame is copied out of a ring whose frames are this size,
		// so anything larger can never be filled from the ring and would just
		// waste memory.
		return fmt.Errorf("afpacket: frame size %d is larger than the %d-byte ring frame; "+
			"use WithGSO for frames bigger than that", c.frameSize, rxFrameSize)
	}
	if c.frames < 64 {
		return fmt.Errorf("afpacket: %d frames is too few", c.frames)
	}
	if c.gso && c.multiBuffer && c.rxQueues > 0 {
		// A chained super-frame needs a whole chain's worth of frames at once,
		// from one queue's own share of the pool. A queue that can never hold
		// one would take the packet, fail part way, put it back, and try again
		// for as long as the device is open -- receiving nothing and counting
		// nothing. Refuse it here, where the numbers are visible.
		per := c.frames / (c.txQueues + c.rxQueues)
		room := c.frameSize - frameHeadroom
		need := (1<<16 + room - 1) / room
		if per < need {
			return fmt.Errorf("afpacket: %d frames over %d queues is %d each, and a "+
				"64 KB packet chained into %d-byte frames needs %d at once; "+
				"ask for at least %d frames",
				c.frames, c.txQueues+c.rxQueues, per, c.frameSize, need,
				need*(c.txQueues+c.rxQueues)*2)
		}
	}
	// The region is frames x frameSize and is populated at map time, so a
	// careless pair reserves an enormous amount of resident memory before a
	// single packet moves. Refuse rather than let the machine find out.
	if bytes := int64(c.frames) * int64(c.frameSize); bytes > maxRegionBytes {
		return fmt.Errorf("afpacket: %d frames of %d bytes is %d MiB of locked memory; "+
			"lower WithFrames or WithFrameSize", c.frames, c.frameSize, bytes>>20)
	}
	return nil
}

// Option configures a Device.
type Option func(*config)

// WithTxQueues sets how many transmit queues to open. Zero is allowed, for a
// receive-only device.
func WithTxQueues(n int) Option { return func(c *config) { c.txQueues = n } }

// WithRxQueues sets how many receive queues to open. More than one joins the
// sockets into a PACKET_FANOUT group, so the kernel spreads received packets
// across them by flow hash. Zero is allowed, for a transmit-only device.
func WithRxQueues(n int) Option { return func(c *config) { c.rxQueues = n } }

// WithQueues sets both directions at once.
func WithQueues(n int) Option {
	return func(c *config) { c.txQueues, c.rxQueues = n, n }
}

// WithFrames sets how many frames the region holds, shared out evenly between
// the queues.
func WithFrames(n int) Option {
	return func(c *config) { c.frames, c.framesSet = n, true }
}

// WithFrameSize sets the size of one frame. A packet starts a little way into
// its frame, so the largest packet is smaller than this; ask
// Capabilities().MaxFrameSize rather than assuming. It must be a power of two and no larger than the 2048-byte
// ring frame the kernel receives into.
func WithFrameSize(n int) Option {
	return func(c *config) { c.frameSize, c.frameSizeSet = n, true }
}

// WithSocketBuffer sets SO_RCVBUF and SO_SNDBUF on every socket.
func WithSocketBuffer(bytes int) Option { return func(c *config) { c.socketBuffer = bytes } }

// maxRegionBytes caps what Open will map for frame memory. It is a guard
// against a slip, not a tuning knob: 64 K frames of 2 KB, or 1 K super-frames
// of 64 KB, are both well inside it.
const maxRegionBytes = 512 << 20

// WithGSO turns on PACKET_VNET_HDR, so segmentation and checksum offload
// metadata travels with each frame in both directions.
//
// With it on, the kernel delivers a TCP super-frame of up to 64 KB whole rather
// than dropping or truncating it, and a frame transmitted through
// [TxQueue.TransmitOffload] is segmented by the kernel instead of here. That is
// worth a great deal: one descriptor carries what would otherwise be forty
// packets. The cost is memory, because every frame must now be big enough to
// hold a super-frame; use [WithFrames] to keep the region a sensible size.
//
// It also makes the queues implement [packetio.OffloadReceiver] and
// [packetio.OffloadTransmitter].
func WithGSO() Option {
	return func(c *config) { c.gso = true }
}

// resolve fills in the defaults that depend on more than one option, once all
// of them have run. Doing it inside an option's own closure made the result
// depend on the order the caller listed them: WithGSO consulted frameSizeSet,
// so WithGSO() before WithFrameSize(1<<16) reserved 16 MiB and the same two
// the other way round reserved 256 MiB. Both are legal sizes, so nothing
// complained.
func (c *config) resolve() {
	if !c.gso {
		return
	}
	if !c.frameSizeSet && c.frameSize < 1<<16 {
		// A receiving device needs a frame big enough for a whole super-frame,
		// so that is the default here. A caller who asked for a size keeps it:
		// a transmit-only device may want small frames and chain them, which
		// is cheaper than any frame size for a forwarder whose bytes already
		// sit in small buffers.
		c.frameSize = 1 << 16
	}
	if !c.framesSet && c.frameSize >= 1<<16 {
		// Frames this big would reserve a quarter of a gigabyte at the
		// ordinary count, so scale it back to the same order of memory. Keyed
		// on the size that was actually settled on, not on how it got there:
		// a caller who kept small frames -- a forwarder chaining them --
		// needs the ordinary count, and cutting it to 256 would leave a pool
		// too small to hold one super-frame's chain.
		c.frames = 256
	}
}

// WithMultiBuffer lets a received packet span several frames.
//
// Without it a packet too big for one frame is counted oversize and dropped,
// because a caller who was handed the first frame of one and told nothing
// would forward a fragment. With it the packet is laid across as many frames
// as it takes, every one but the last marked [packetio.OptContinued], and
// [Capabilities.MultiBuffer] says so.
//
// It exists for a forwarder whose buffers are smaller than the packets it
// carries -- a segmentation-offloaded stream arrives in 64 KB super-frames --
// so the ring is copied straight into those buffers instead of into one large
// frame and out of it again. That copy is free while a core has time to spare
// and is the whole cost once it does not.
//
// Two things become the caller's business. A packet of n bytes needs
// ceil(n/[Device.MaxFrameSize]) descriptors, so the max passed to Receive must
// be at least that or the packet is dropped and counted -- 64 KB over 2 KB
// frames wants 33. And a partial checksum on a chained packet is left
// unfinished, because no one frame holds all the bytes it covers; the caller
// finishes it after reassembly, and nothing detects the omission.
func WithMultiBuffer() Option { return func(c *config) { c.multiBuffer = true } }

// WithPromiscuous puts the interface into promiscuous mode for as long as the
// device is open, so it takes packets not addressed to it. The kernel undoes
// this when the socket closes, including if the process dies.
func WithPromiscuous() Option { return func(c *config) { c.promiscuous = true } }
