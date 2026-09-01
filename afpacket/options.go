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
	// framesSet records that the caller chose the frame count, so WithGSO
	// knows whether it may lower the default.
	framesSet bool
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
	case c.gso && c.frameSize < 1<<16:
		// A super-frame is one buffer of up to 64 KB, so a region frame has to
		// hold one whole. Chaining it across small frames would defeat the
		// point, which is that one descriptor does the work of forty.
		//
		// The headroom comes off the top: a 65536-byte frame carries a
		// super-frame of 65472, which Capabilities.MaxFrameSize reports and
		// anything larger is counted oversize rather than truncated.
		return fmt.Errorf("afpacket: WithGSO needs a frame size of at least 65536, not %d "+
			"(a segmentation-offload super-frame is one buffer of up to 64 KB)", c.frameSize)
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
func WithFrameSize(n int) Option { return func(c *config) { c.frameSize = n } }

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
	return func(c *config) {
		c.gso = true
		if c.frameSize < 1<<16 {
			c.frameSize = 1 << 16
		}
		if !c.framesSet {
			// Frames are now 32 times bigger, so the ordinary default would
			// reserve a quarter of a gigabyte. Scale it back to the same
			// order of memory; WithFrames overrides either way.
			c.frames = 256
		}
	}
}

// WithPromiscuous puts the interface into promiscuous mode for as long as the
// device is open, so it takes packets not addressed to it. The kernel undoes
// this when the socket closes, including if the process dies.
func WithPromiscuous() Option { return func(c *config) { c.promiscuous = true } }
