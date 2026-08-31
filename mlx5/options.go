//go:build linux && cgo && mlx5 && (amd64 || arm64)

package mlx5

import (
	"fmt"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/mlx5/internal/wqe"
)

// An Option changes how a device is opened. The defaults are meant to be right
// for a packet generator or a forwarder on a modern ConnectX; the options are
// for the cases where they are not.
type Option func(*config)

type config struct {
	txQueues    int
	txDepth     int
	rxQueues    int
	rxDepth     int
	steering    packetio.SteeringFilter
	frameSize   int
	affinity    []int
	affinitySet bool
	noAffinity  bool
	frames      int
	inlineLen   int
	inlineSet   bool
	ethHdrLen   int
	hugePages   bool
	checksums   bool
	signalEvery int
	multiPacket bool
	mpwSet      bool
	mpwMaxLen   int
	// ringsPerQueue is how many hardware send queues back one TxQueue; 0
	// means the device chooses from the descriptor mode.
	ringsPerQueue int
	// framesSet records that the caller chose the frame count, so Open must
	// honour it rather than sizing the region itself.
	framesSet bool
}

func defaults() config {
	return config{
		txQueues:  1,
		txDepth:   1024,
		rxDepth:   1024,
		frameSize: 2048,
		// Enough frames to keep every queue full twice over, so that a slow
		// completion never starves the packet builder.
		frames:    4096,
		inlineLen: -1, // decided from what the device requires
		mpwMaxLen: 192,
	}
}

func (c *config) validate(dev string) error {
	switch {
	case c.txQueues < 0 || c.rxQueues < 0:
		return fmt.Errorf("mlx5: %d transmit and %d receive queues", c.txQueues, c.rxQueues)
	case c.txQueues == 0 && c.rxQueues == 0:
		return fmt.Errorf("mlx5: no queues asked for")
	case c.txQueues > maxQueues || c.rxQueues > maxQueues:
		// Capabilities has always reported this ceiling and nothing enforced
		// it. Asking for 999 queues opened 999 of them and sized the region
		// from the total, so a typo became a four-gigabyte allocation and a
		// thousand busy-polling workers -- which looks like a hang, not a
		// mistake.
		return fmt.Errorf("mlx5: %d transmit and %d receive queues, and %d each way is the most "+
			"this backend opens; more queues than cores has only ever cost throughput",
			c.txQueues, c.rxQueues, maxQueues)
	case c.txDepth <= 0 || c.txDepth&(c.txDepth-1) != 0:
		return fmt.Errorf("mlx5: a transmit queue of %d, which must be a power of two", c.txDepth)
	case c.rxDepth <= 0 || c.rxDepth&(c.rxDepth-1) != 0:
		return fmt.Errorf("mlx5: a receive queue of %d, which must be a power of two", c.rxDepth)
	case c.frameSize < 64 || c.frameSize&(c.frameSize-1) != 0:
		return fmt.Errorf("mlx5: a frame size of %d, which must be a power of two of at least 64", c.frameSize)
	case c.frames <= 0:
		return fmt.Errorf("mlx5: %d frames", c.frames)
	case c.frames > maxFrames:
		return fmt.Errorf("mlx5: %d frames of %d bytes is %d MiB of pinned memory, and %d MiB "+
			"is the most this backend registers; ask for fewer frames or smaller ones",
			c.frames, c.frameSize, int64(c.frames)*int64(c.frameSize)>>20,
			int64(maxFrames)*int64(c.frameSize)>>20)
	case c.inlineSet && (c.inlineLen < 0 || c.inlineLen > wqe.MaxDS*wqe.Octoword):
		return fmt.Errorf("mlx5: an inline header of %d bytes", c.inlineLen)
	case c.signalEvery < 0:
		return fmt.Errorf("mlx5: a completion every %d packets", c.signalEvery)
	case c.ethHdrLen < 0 || c.ethHdrLen > wqe.MaxDS*wqe.Octoword:
		return fmt.Errorf("mlx5: an Ethernet header of %d bytes", c.ethHdrLen)
	case c.multiPacket && (c.mpwMaxLen <= 0 || c.mpwMaxLen > c.frameSize):
		return fmt.Errorf("mlx5: carrying packets of up to %d bytes in a descriptor, in %d-byte frames", c.mpwMaxLen, c.frameSize)
	case c.ringsPerQueue < 0 || c.ringsPerQueue > 16:
		return fmt.Errorf("mlx5: %d rings behind each transmit queue; between 1 and 16, or 0 to let the device choose", c.ringsPerQueue)
	}
	// The filter is checked here, with the rest of the options, so that a
	// contradictory one is reported as what it is rather than surfacing later
	// as whatever the hardware said first.
	if err := c.steering.Validate(); err != nil {
		return fmt.Errorf("mlx5: %w", err)
	}
	if c.steering.Promiscuous || len(c.steering.Match) > 0 {
		if c.rxQueues == 0 {
			return fmt.Errorf("mlx5: a steering filter with no receive queues; "+
				"it says what to receive, and this device receives nothing (%s)", c.steering)
		}
		if _, err := c.steering.Rules(); err != nil {
			return fmt.Errorf("mlx5: %w", err)
		}
	}
	// Every queue needs enough frames to fill it, or it can never reach the
	// depth it was given and the extra descriptors are a waste. This check
	// only binds a caller who chose the count: without WithFrames, Open sizes
	// the region from the queues once it knows how many rings each one gets.
	if c.framesSet {
		need := c.txQueues*max(c.ringsPerQueue, 1)*c.txDepth + c.rxQueues*c.rxDepth
		if c.frames < need {
			return fmt.Errorf("mlx5: %d frames is not enough for %d transmit queues of %d and %d receive queues of %d; %s needs at least %d",
				c.frames, c.txQueues, c.txDepth, c.rxQueues, c.rxDepth, dev, need)
		}
	}
	return nil
}

// WithTxQueues sets how many transmit queues to open. Each is driven by one
// goroutine and is independent of the others.
func WithTxQueues(n int) Option { return func(c *config) { c.txQueues = n } }

// WithTxDepth sets how many packets a transmit queue may have in flight. It
// must be a power of two. Deeper queues ride out longer pauses in the sender at
// the cost of frames sitting in them.
func WithTxDepth(n int) Option { return func(c *config) { c.txDepth = n } }

// WithRxQueues sets how many receive queues to open. Each is driven by one
// goroutine.
//
// A queue receives nothing until it is told what to take. By default that is
// the interface's own address; [WithSteering] changes it. Everything not taken
// still reaches the kernel.
func WithRxQueues(n int) Option { return func(c *config) { c.rxQueues = n } }

// WithQueues sets both directions at once.
func WithQueues(n int) Option { return func(c *config) { c.txQueues, c.rxQueues = n, n } }

// WithRxDepth sets how many buffers a receive queue may have posted at once. It
// must be a power of two. A deeper queue rides out longer pauses in the
// receiver before the NIC starts dropping.
func WithRxDepth(n int) Option { return func(c *config) { c.rxDepth = n } }

// WithSteering says which packets the receive queues should be given.
//
// The card matches these in hardware at no cost per packet, and anything the
// filter does not match still goes to the kernel, so the interface keeps
// working -- SSH, ARP and the rest carry on while this program takes the
// traffic it asked for.
//
// A match this card cannot express is refused at Open with
// [packetio.ErrUnsupported] rather than installed as a wider rule. The card
// matches destination address, VLAN, EtherType, IPv4 source and destination
// prefixes, IP protocol, and TCP or UDP ports.
//
// This is the only way to say what a queue takes. There were once separate
// options for a MAC, a VLAN and promiscuous mode, and having two routes to one
// setting meant they could disagree: the code read as though the option won and
// the filter actually did. A filter says all of it in one place, and
// SteeringFilter.Validate refuses the combinations that used to be silently
// resolved -- promiscuous together with a match, most of all, which quietly
// widened to match-everything.
//
//	// every packet the port sees
//	mlx5.WithSteering(packetio.SteeringFilter{Promiscuous: true})
//
//	// one VLAN, two UDP ports
//	mlx5.WithSteering(packetio.SteeringFilter{Match: append(
//	    []packetio.Match{packetio.MatchVLAN(100)},
//	    append(packetio.MatchDstPort(packetio.IPProtoUDP, 9000),
//	           packetio.MatchDstPort(packetio.IPProtoUDP, 9001)...)...)})
func WithSteering(f packetio.SteeringFilter) Option {
	return func(c *config) { c.steering = f }
}

// WithFrameSize sets the size of one frame, which is the largest packet that
// fits in one. It must be a power of two.
func WithFrameSize(n int) Option { return func(c *config) { c.frameSize = n } }

// WithFrames sets how many frames the region holds, across every queue.
//
// Without it the region is sized from the queues: enough frames to cover
// every ring twice over, so a slow completion never starves the packet
// builder. Set it only to bound pinned memory, and know that Open refuses a
// count the queues cannot work in.
func WithFrames(n int) Option { return func(c *config) { c.frames = n; c.framesSet = true } }

// WithRingsPerQueue sets how many hardware send queues sit behind each
// TxQueue, which Transmit deals batches over round-robin.
//
// Left alone, the device chooses what measured fastest: one ring in pointer
// mode, where a single ring outruns the core driving it, and four in copied
// multi-packet mode, where the card drains each send queue at about a quarter
// of what one core produces. This option exists for measuring one arrangement
// against another, and for hardware where the trade lands somewhere else.
func WithRingsPerQueue(n int) Option { return func(c *config) { c.ringsPerQueue = n } }

// WithInlineHeader sets how many bytes of each packet are copied into its work
// queue entry rather than pointed at.
//
// By default this is whatever the device requires, which is either nothing or
// the 18 bytes of an Ethernet header with one VLAN tag. Raising it copies more
// and points at less, which is how small packets are made to cost the NIC one
// memory read instead of two; lowering it below what the device requires is
// refused.
func WithInlineHeader(n int) Option {
	return func(c *config) { c.inlineLen, c.inlineSet = n, true }
}

// WithEthernetHeaderLen says how long the Ethernet headers of the frames to be
// transmitted are: 14 untagged, 18 with a VLAN tag, 22 with two.
//
// It matters on a device that requires an inline header, because that header
// must cover the whole Ethernet header. On a device that requires none this
// changes nothing, which is why it is stated as a property of the traffic
// rather than as a length to inline: the right number depends on both.
func WithEthernetHeaderLen(n int) Option { return func(c *config) { c.ethHdrLen = n } }

// WithAffinity places the workers on these processors, in this order, instead
// of the ones this backend would choose. The first goroutine to drive a queue
// takes the first processor, the second the second, and so on.
func WithAffinity(cpus ...int) Option {
	return func(c *config) { c.affinity = append([]int(nil), cpus...); c.affinitySet = true }
}

// WithoutAffinity leaves the workers wherever the scheduler puts them.
//
// The default is to place them, because where they run changes the rate by
// about a third: the workers of one device share the write-combining page
// their doorbells go through, and a page written from two last-level caches is
// dearer than one written from a single cache. Turn it off for a program that
// does its own placement, or one sharing the machine with something else that
// matters more.
func WithoutAffinity() Option { return func(c *config) { c.noAffinity = true } }

// WithHugePages asks for the frame region to be backed by huge pages, so the
// NIC has fewer address translations to cache. It falls back to ordinary pages
// if none are reserved.
func WithHugePages() Option { return func(c *config) { c.hugePages = true } }

// WithChecksumOffload asks the NIC to compute IPv4 header and transport
// checksums on transmit, so the packet may be built with those fields left
// zero.
func WithChecksumOffload() Option { return func(c *config) { c.checksums = true } }

// WithMultiPacket carries several packets in one descriptor, copied in rather
// than pointed at.
//
// The two descriptor forms are two regimes, not better and worse. Pointer
// entries cost a third of the instructions and one ring outruns the core
// driving it -- 66-67 Mpps -- but every pointer ring on the device shares one
// ~76 Mpps ceiling, the card's per-packet gather limit. Copied entries cost
// more per packet, but their ceiling is per send queue and multiplies: that
// is the only route past 76 Mpps and to 64-byte line rate on the cards
// measured here. A queue opened in this mode brings its own rings -- four
// hardware send queues behind each TxQueue, dealt to round-robin -- so
// asking for it is the whole answer, with nothing further to configure.
//
// A device opened with two or more transmit queues chooses this mode by
// itself where the card supports it, on the grounds that more than one queue
// means throughput matters. Use this option to get it at one queue, or
// [WithoutMultiPacket] to refuse it.
//
// It needs a device that reports it can do this and that requires no inline
// Ethernet header; Open fails if the device does not. Packets longer than the
// threshold go in ordinary descriptors, where the copy would cost more than the
// fetch it saves.
func WithMultiPacket(maxLen int) Option {
	return func(c *config) { c.multiPacket, c.mpwSet = true, true; c.mpwMaxLen = maxLen }
}

// WithoutMultiPacket keeps ordinary one-packet descriptors even where the
// device could do better, which is what to reach for when comparing the two.
func WithoutMultiPacket() Option {
	return func(c *config) { c.multiPacket, c.mpwSet = false, true }
}

// WithCompletionEvery asks for a completion at least every n packets rather
// than once per batch. Frames come back sooner, at the cost of a completion
// queue entry each time. Zero, the default, is once per batch.
func WithCompletionEvery(n int) Option { return func(c *config) { c.signalEvery = n } }
