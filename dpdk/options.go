//go:build linux && cgo && dpdk && (amd64 || arm64)

package dpdk

import (
	"fmt"

	"github.com/atoonk/packetio"
)

// An Option changes how a device is opened.
type Option func(*config)

type config struct {
	txQueues  int
	rxQueues  int
	txDepth   int
	rxDepth   int
	frames    int
	frameSize int
	mtu       int

	steering  packetio.SteeringFilter
	promisc   bool
	checksums bool
	tso       bool

	noHuge   bool
	memoryMB int
	logLevel string
	eal      []string
	devargs  string
	// framesSet records that the caller chose the frame count, so Open must
	// refuse a count the queues cannot work in rather than silently raising it.
	framesSet bool

	affinity   []int
	noAffinity bool

	// ringsPerQueue is how many hardware send queues back one TxQueue; 0
	// means the device chooses from the driver and the queue count.
	ringsPerQueue int
}

func defaults() config {
	return config{
		txQueues:  1,
		txDepth:   1024,
		rxDepth:   1024,
		frameSize: 2048,
		// Enough to fill every queue twice over, so a slow completion never
		// starves the packet builder. Open raises it if the queues need more.
		frames: 4096,
		mtu:    1500,
	}
}

// maxQueues is how many queues of either direction this backend opens. The
// ceiling is this backend's, not the hardware's: more queues than cores has
// only ever cost throughput on the cards measured here.
const maxQueues = 64

// ringsPerCopiedQueue is how many hardware send queues back one TxQueue on a
// Mellanox PMD opened transmit-only with several queues. Measured on a
// ConnectX-6 Dx: the PMD's copied descriptors drain at ~17 Mpps per send
// queue, one core produces ~52, and the fifth ring adds nothing a core can
// use.
const ringsPerCopiedQueue = 4

// maxFrames bounds the region, so a mistyped frame count is refused rather than
// turning into a multi-gigabyte hugepage reservation that fails halfway.
const maxFrames = 1 << 21

// maxFrameSize is what fits the mbuf's sixteen-bit data_off and data_len.
const maxFrameSize = 1 << 16

func (c *config) validate() error {
	switch {
	case c.txQueues < 0 || c.rxQueues < 0:
		return fmt.Errorf("dpdk: %d transmit and %d receive queues", c.txQueues, c.rxQueues)
	case c.txQueues == 0 && c.rxQueues == 0:
		return fmt.Errorf("dpdk: no queues asked for")
	case c.ringsPerQueue < 0 || c.ringsPerQueue > 16:
		return fmt.Errorf("dpdk: %d rings behind each transmit queue; between 1 and 16, or 0 to let the device choose", c.ringsPerQueue)
	case c.txQueues > maxQueues || c.rxQueues > maxQueues:
		return fmt.Errorf("dpdk: %d transmit and %d receive queues, and %d each way is the "+
			"most this backend opens", c.txQueues, c.rxQueues, maxQueues)
	case c.txDepth <= 0 || c.txDepth&(c.txDepth-1) != 0:
		return fmt.Errorf("dpdk: a transmit queue of %d, which must be a power of two", c.txDepth)
	case c.rxDepth <= 0 || c.rxDepth&(c.rxDepth-1) != 0:
		return fmt.Errorf("dpdk: a receive queue of %d, which must be a power of two", c.rxDepth)
	case c.frameSize > maxFrameSize:
		// Above this the per-packet lengths the driver is given -- data_off
		// and data_len -- no longer fit the sixteen bits the mbuf has for
		// them, and a truncated length is bytes from the wrong place going on
		// the wire with nothing reported.
		return fmt.Errorf("dpdk: a frame size of %d, and %d is the largest a frame may be",
			c.frameSize, maxFrameSize)
	case c.frameSize < 512 || c.frameSize&(c.frameSize-1) != 0:
		// Below 512 the mempool header, the mbuf and the headroom leave almost
		// nothing for a packet.
		return fmt.Errorf("dpdk: a frame size of %d, which must be a power of two of at least 512",
			c.frameSize)
	case c.frames <= 0:
		return fmt.Errorf("dpdk: %d frames", c.frames)
	case c.frames > maxFrames:
		return fmt.Errorf("dpdk: %d frames of %d bytes is %d MiB, and %d MiB is the most this "+
			"backend reserves", c.frames, c.frameSize,
			int64(c.frames)*int64(c.frameSize)>>20, int64(maxFrames)*int64(c.frameSize)>>20)
	case c.mtu <= 0:
		return fmt.Errorf("dpdk: an MTU of %d", c.mtu)
	case c.affinity != nil && c.noAffinity:
		return fmt.Errorf("dpdk: WithAffinity and WithoutAffinity contradict each other")
	}
	if err := c.steering.Validate(); err != nil {
		return fmt.Errorf("dpdk: %w", err)
	}
	if c.steering.Promiscuous || len(c.steering.Match) > 0 {
		if c.rxQueues == 0 {
			return fmt.Errorf("dpdk: a steering filter with no receive queues; it says what to "+
				"receive, and this device receives nothing (%s)", c.steering)
		}
		if _, err := c.steering.Rules(); err != nil {
			return fmt.Errorf("dpdk: %w", err)
		}
	}
	return nil
}

// WithTxQueues sets how many transmit queues to open, each driven by one
// goroutine.
func WithTxQueues(n int) Option { return func(c *config) { c.txQueues = n } }

// WithRxQueues sets how many receive queues to open.
//
// A queue receives nothing until the device is told what to take. On a device
// the kernel still owns, that is [WithSteering]; on one this process owns
// outright, the port's own address filter decides and [WithPromiscuous] widens
// it.
func WithRxQueues(n int) Option { return func(c *config) { c.rxQueues = n } }

// WithQueues sets both directions at once.
func WithQueues(n int) Option { return func(c *config) { c.txQueues, c.rxQueues = n, n } }

// WithRingsPerQueue sets how many hardware send queues sit behind each
// TxQueue, which Transmit deals batches over round-robin.
//
// Left alone, the device chooses what measured fastest: four on a Mellanox
// PMD opened transmit-only with two or more queues -- where the card drains
// each send queue at about a quarter of what one core produces -- and one
// everywhere else. This option exists for measuring one arrangement against
// another, and for hardware where the trade lands somewhere else.
func WithRingsPerQueue(n int) Option { return func(c *config) { c.ringsPerQueue = n } }

// WithTxDepth and WithRxDepth set the driver's descriptor ring depths, in
// packets. Both must be powers of two.
func WithTxDepth(n int) Option { return func(c *config) { c.txDepth = n } }

// WithRxDepth sets how many buffers a receive queue may have outstanding.
func WithRxDepth(n int) Option { return func(c *config) { c.rxDepth = n } }

// WithFrames sets how many frames the region holds, shared out between the
// queues.
// Open refuses a count the queues cannot work in rather than silently
// raising it.
func WithFrames(n int) Option {
	return func(c *config) { c.frames, c.framesSet = n, true }
}

// WithFrameSize sets the size of one frame. It must be a power of two.
//
// A frame holds more than the packet: the mempool's object header, the mbuf and
// the headroom come first, which is 320 bytes on every platform this runs on.
// [Device.Capabilities] reports what is left as MaxFrameSize.
func WithFrameSize(n int) Option { return func(c *config) { c.frameSize = n } }

// WithMTU sets the MTU the device is configured for. It must fit in a frame
// with the headroom, or Open refuses: this backend never chains a packet across
// frames, so a packet that does not fit is one the driver could not deliver.
func WithMTU(n int) Option { return func(c *config) { c.mtu = n } }

// WithSteering says which packets the receive queues should be given.
//
// What it means depends on who owns the device, and the difference matters.
// Where the kernel still has a network interface for it -- a ConnectX under
// mlx5_core, say -- the port is put in flow-isolated mode and these rules divert
// exactly what they match, while everything else still reaches the host, so the
// machine stays usable. Where this process owns the device outright there is no
// host to leave anything to, and the filter only decides which queue a packet
// lands on. [Info.Coexists] says which of the two you have.
//
// A match the device will not take is refused at Open with
// [packetio.ErrUnsupported] rather than installed as something wider.
func WithSteering(f packetio.SteeringFilter) Option {
	return func(c *config) { c.steering = f }
}

// WithPromiscuous asks the port to take every packet it sees.
//
// Whether it did is [Info.Promiscuous]. A driver with no promiscuous mode at
// all is accepted on a device this process owns outright -- the ENA on EC2 --
// since the port's own address filter is then the only thing in the way and
// nothing else was going to arrive; on a device shared with the kernel it is
// [packetio.ErrUnsupported].
func WithPromiscuous() Option { return func(c *config) { c.promisc = true } }

// WithChecksumOffload asks the NIC to verify checksums on receive and compute
// them on transmit, where it can. What it actually took is in
// [packetio.Capabilities].
func WithChecksumOffload() Option { return func(c *config) { c.checksums = true } }

// WithTSO asks the NIC to cut super-frames into MTU-sized segments, so one
// descriptor does the work of many.
//
// It implies [WithChecksumOffload], because a NIC that segments a packet writes
// a fresh checksum into every segment it makes. Open refuses with
// [packetio.ErrUnsupported] where the device does not offer segmentation,
// rather than opening a device that would put a 64 KB frame on the wire.
//
// Use it through [packetio.OffloadTransmitter]: a plain Transmit never
// segments, because a frame carries no segment size of its own.
func WithTSO() Option { return func(c *config) { c.tso = true; c.checksums = true } }

// WithoutHugePages runs the environment on ordinary memory.
//
// It is for a virtual device on a machine with no hugepages reserved -- a test
// over a veth pair, say. A real NIC wants hugepages: ordinary memory is neither
// pinned nor mapped for a device to reach, and where the device addresses
// memory physically -- no IOMMU, as on EC2 -- the environment refuses to start
// without them.
func WithoutHugePages(megabytes int) Option {
	return func(c *config) { c.noHuge, c.memoryMB = true, megabytes }
}

// WithEALArgs passes extra arguments to the environment, for the things this
// package does not name. They are used only if this is the call that starts the
// environment, since it starts once per process.
func WithEALArgs(args ...string) Option {
	return func(c *config) { c.eal = append(c.eal, args...) }
}

// WithDevArgs passes driver arguments for the device, in DPDK's own
// "key=value,key=value" form, for what this package does not name.
//
// The one worth knowing on a ConnectX is dv_flow_en=0, which makes the driver
// install steering rules through verbs -- the same mechanism the native mlx5
// backend uses -- rather than writing them into the card's steering memory
// itself.
func WithDevArgs(args string) Option { return func(c *config) { c.devargs = args } }

// WithLogLevel sets the environment's verbosity, as DPDK spells it, for example
// "lib.eal:debug".
func WithLogLevel(s string) Option { return func(c *config) { c.logLevel = s } }

// WithAffinity places the workers on these processors, in this order, instead
// of the ones this backend would choose.
func WithAffinity(cpus ...int) Option {
	return func(c *config) { c.affinity = append([]int(nil), cpus...) }
}

// WithoutAffinity leaves the workers wherever the scheduler puts them. See
// internal/affinity for why the default is not that.
func WithoutAffinity() Option { return func(c *config) { c.noAffinity = true } }
