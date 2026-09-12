//go:build linux && cgo && dpdk && (amd64 || arm64)

package dpdk

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/dpdk/internal/eal"
	"github.com/atoonk/packetio/dpdk/internal/mbuf"
	"github.com/atoonk/packetio/dpdk/internal/queue"
	"github.com/atoonk/packetio/internal/affinity"
)

// Device is an open NIC: one region of frame memory and the queues that move
// packets through it.
type Device struct {
	port   eal.Port
	handle eal.DeviceHandle
	region *eal.Region
	layout mbuf.Layout
	info   Info
	cfg    config
	place  *affinity.Placement

	// What the device took at configure time, as opposed to what was asked
	// for. Capabilities and the queues are built from these.
	rxChecksums bool
	txChecksums bool
	tso         bool
	maxQueues   int

	tx    []*TxQueue
	rx    []*RxQueue
	pools []*eal.Mempool

	closeMu sync.Mutex
	closed  bool
	// How far Open got, which is how far Close has to go back. A port whose
	// queues were never all set up is left probed, its isolation and
	// promiscuous mode undone: see Close.
	isolated bool
	ready    bool
	started  bool
}

// Info describes an open device.
type Info struct {
	// Device is what was opened, and Port the DPDK port it became.
	Device string
	Port   int

	// Driver is the poll-mode driver's own name, such as mlx5_pci or
	// net_af_packet, and Socket the memory node the device is on.
	Driver string
	Socket int

	// Coexists reports whether the kernel still has a network interface for
	// this device.
	//
	// It decides what a steering filter can promise. Where it is true the port
	// is flow-isolated and traffic the filter does not match still reaches the
	// host, so taking a few UDP ports from a live machine is safe. Where it is
	// false this process owns the device outright: nothing else is receiving
	// from it, and a filter only chooses which queue a packet lands on.
	Coexists bool

	// MAC is the port's own Ethernet address.
	MAC [6]byte

	// FrameSize, Frames, and the queue counts and depths are what was opened;
	// MaxPacket is how much of a frame a packet may use.
	FrameSize int
	Frames    int
	MaxPacket int
	TxQueues  int
	TxDepth   int
	RxQueues  int
	RxDepth   int

	// RingsPerQueue is how many hardware send queues sit behind each
	// TxQueue; Transmit deals batches over them round-robin. More than one
	// on a Mellanox PMD opened transmit-only with several queues, where the
	// card's per-send-queue drain rate is the limit.
	RingsPerQueue int

	// Steering describes what the receive queues were told to take, and
	// Promiscuous whether the port was put in promiscuous mode to do it.
	//
	// Promiscuous can be false after asking for it. A driver with no such
	// mode -- the ENA on EC2 is one -- is accepted when this process owns the
	// device outright, because the port's own address filter is then the only
	// thing between the wire and the queues and there is nothing to widen.
	Steering    string
	Promiscuous bool

	// Placement describes where this device's workers will be put.
	Placement string

	// LinkUp reports whether the port had carrier by the end of Open, and
	// SpeedMbps how fast the link negotiated. A port opened while its link is
	// still negotiating transmits nothing until it comes up.
	LinkUp    bool
	SpeedMbps uint32

	// RegionVA is the address this process knows the frame memory by: every
	// descriptor is an offset from it. IOVAMode is how the NIC addresses the
	// same memory -- "virtual" behind an IOMMU or on a bifurcated card,
	// "physical" where there is no IOMMU, as on EC2. Both are the first things
	// to check when a frame does not arrive.
	RegionVA uint64
	IOVAMode string
}

// Open opens a device and creates its queues.
//
// The name is a network interface (eth0), a PCI address (0000:c1:00.1), or a
// virtual device (net_null0, or net_af_packet0,iface=veth1). An interface name
// is resolved to the PCI address behind it.
func Open(name string, opts ...Option) (d *Device, err error) {
	cfg := defaults()
	for _, o := range opts {
		o(&cfg)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	devargs, err := resolve(name)
	if err != nil {
		return nil, err
	}
	// mlx5's vectorised receive assumes a mempool laid out by
	// rte_pktmbuf_pool_create. This backend's pool is custom ops over the frame
	// region, which that path does not understand: it posts descriptors, then
	// mishandles the replenish and stalls after the first ring while the NIC
	// drops everything else. Measured on a ConnectX-6 Dx -- 256 of 3,000,000
	// received with vector rx on, 3,000,000 of 3,000,000 with it off. The
	// scalar path is a little slower per core and correct, so it is the default
	// on a Mellanox card. A caller who has made the pool vector-compatible can
	// override it through WithDevArgs.
	extra := cfg.devargs
	if isMellanox(devargs) && !strings.Contains(extra, "rx_vec_en") {
		if extra == "" {
			extra = "rx_vec_en=0"
		} else {
			extra = "rx_vec_en=0," + extra
		}
	}
	if extra != "" {
		devargs += "," + extra
	}

	// One transmit queue may be backed by several hardware send queues; see
	// TxQueue. On a Mellanox PMD the ceiling that matters at high rate is per
	// send queue -- the driver switches to copied descriptors at eight
	// queues, and the card drains each at ~17 Mpps, a quarter of what one
	// core produces -- so a transmit-only device opened with two or more
	// queues gets four rings behind each. That is both the arrangement that
	// reaches 64-byte line rate and the one that spends the fewest cores
	// doing it: measured, 148.8 Mpps on four cores. Other PMDs have shown no
	// per-queue ceiling worth paying extra rings for, and keep one.
	// WithRingsPerQueue overrides either way.
	rings := cfg.ringsPerQueue
	if rings == 0 {
		rings = 1
		if cfg.rxQueues == 0 && cfg.txQueues >= 2 && isMellanox(devargs) {
			rings = ringsPerCopiedQueue
		}
	}
	if cfg.txQueues*rings > maxQueues {
		return nil, fmt.Errorf("dpdk: %d transmit queues of %d rings each is %d send queues, "+
			"and %d is the most this backend opens", cfg.txQueues, rings, cfg.txQueues*rings, maxQueues)
	}

	// The frames have to cover every queue's ring twice over, or a queue can
	// never reach the depth it was given.
	queues := cfg.txQueues*rings + cfg.rxQueues
	need := cfg.txQueues*rings*cfg.txDepth + cfg.rxQueues*cfg.rxDepth
	if cfg.framesSet {
		// The caller chose the count; refuse one the queues cannot work in
		// rather than silently multiplying a bound they set -- the same
		// contract as mlx5's WithFrames.
		if cfg.frames < need {
			return nil, fmt.Errorf("dpdk: %d frames is not enough for %d transmit rings of %d "+
				"and %d receive queues of %d; at least %d are needed",
				cfg.frames, cfg.txQueues*rings, cfg.txDepth, cfg.rxQueues, cfg.rxDepth, need)
		}
	} else if cfg.frames < 2*need {
		cfg.frames = nextPow2(2 * need)
	}
	if cfg.frames > maxFrames {
		return nil, fmt.Errorf("dpdk: %d transmit queues of %d and %d receive queues of %d "+
			"need %d frames, more than this backend reserves",
			cfg.txQueues, cfg.txDepth, cfg.rxQueues, cfg.rxDepth, 2*need)
	}

	if err := eal.Init(eal.Args{
		Devices:  []string{devargs},
		NoHuge:   cfg.noHuge,
		Memory:   cfg.memoryMB,
		LogLevel: cfg.logLevel,
		Extra:    cfg.eal,
	}); err != nil {
		return nil, err
	}

	port, err := open(devargs)
	if err != nil {
		return nil, err
	}

	// The device handle is taken now, while the port still exists: closing the
	// port releases its id and with it the only route back to the device.
	d = &Device{port: port, handle: eal.Handle(port), cfg: cfg}
	// Anything that fails from here leaves nothing behind that the next Open
	// cannot take over: the memzone and mempools are freed, and the port is
	// left as Close describes. The cleanup holds its own reference, because
	// returning "nil, err" clears the named result before the deferred
	// function runs.
	half := d
	defer func() {
		if err != nil {
			half.Close()
			d = nil
		}
	}()

	di, err := eal.Info(port)
	if err != nil {
		return nil, err
	}
	if cfg.txQueues*rings > di.MaxTxQueues || cfg.rxQueues > di.MaxRxQueues {
		return nil, fmt.Errorf("dpdk: %s allows %d receive and %d transmit queues, "+
			"and %d and %d were asked for",
			di.Driver, di.MaxRxQueues, di.MaxTxQueues, cfg.rxQueues, cfg.txQueues*rings)
	}
	// What Capabilities.MaxQueues reports: the device's own ceiling where it
	// is lower than this backend's, because the contract is what this device
	// can open, not what packetio would allow some other device.
	d.maxQueues = min(maxQueues, di.MaxRxQueues, di.MaxTxQueues)

	d.layout = mbuf.Layout{
		FrameSize: cfg.frameSize,
		ObjHeader: mempoolHeader,
		MbufSize:  mbuf.Size,
		Headroom:  mbuf.Headroom,
	}
	if err := d.layout.Validate(); err != nil {
		return nil, err
	}
	// A packet must fit in one frame: this backend never chains.
	if want := cfg.mtu + maxEthernetHeader; want > d.layout.MaxPacket() {
		return nil, fmt.Errorf("dpdk: an MTU of %d needs %d bytes and a %d-byte frame leaves "+
			"only %d after the mbuf and headroom; raise WithFrameSize",
			cfg.mtu, want, cfg.frameSize, d.layout.MaxPacket())
	}

	d.info = Info{
		Device: name, Port: int(port), Driver: di.Driver, Socket: di.Socket,
		RingsPerQueue: rings,
		Coexists:      di.Bifurcated, MAC: di.MAC,
		FrameSize: cfg.frameSize, Frames: cfg.frames, MaxPacket: d.layout.MaxPacket(),
		TxQueues: cfg.txQueues, TxDepth: cfg.txDepth,
		RxQueues: cfg.rxQueues, RxDepth: cfg.rxDepth,
		IOVAMode: iovaName(eal.IOVAMode()),
	}

	// The memory comes before the port is touched: it needs nothing from the
	// port but its memory node, and everything that can go wrong with it --
	// too few hugepages, above all -- then goes wrong while the port is still
	// only probed and there is nothing to undo.
	if d.region, err = eal.ReserveRegion(regionName(port), cfg.frames*cfg.frameSize,
		cfg.frameSize, di.Socket); err != nil {
		return nil, err
	}
	d.info.RegionVA = d.region.VA

	if !cfg.noAffinity {
		if d.place, err = affinity.New(name, cfg.affinity); err != nil {
			return nil, fmt.Errorf("dpdk: %w", err)
		}
	}
	d.info.Placement = d.place.String()

	// Isolated mode has to be asked for before the port is configured, and it
	// is what keeps the kernel's traffic reaching the kernel.
	steer, err := d.steeringRules(name)
	if err != nil {
		return nil, err
	}
	if len(steer) > 0 && di.Bifurcated {
		if err := eal.Isolate(port, true); err != nil {
			return nil, fmt.Errorf("%w; without isolated mode this device cannot take traffic "+
				"from the kernel safely", err)
		}
		d.isolated = true
	}

	var offloads uint32
	if cfg.checksums {
		offloads = eal.RxChecksum | eal.TxChecksum
	}
	if cfg.tso {
		// Segmentation needs the checksums it implies: a NIC cutting a
		// super-frame up writes a new checksum into every segment.
		offloads |= eal.TxTSO | eal.TxChecksum
	}
	applied, err := eal.Configure(port, cfg.rxQueues, cfg.txQueues*rings, cfg.mtu, offloads)
	if err != nil {
		return nil, err
	}
	// What the device took, not what was asked for. A PMD that does not offer
	// an offload has it dropped rather than failing configure, so asking is not
	// evidence of having.
	d.rxChecksums = applied&eal.RxChecksum != 0
	d.txChecksums = applied&eal.TxChecksum != 0
	d.tso = applied&eal.TxTSO != 0
	if cfg.tso && !d.tso {
		return nil, fmt.Errorf("dpdk: %w: this device does not offer TCP segmentation",
			packetio.ErrUnsupported)
	}

	// Each queue owns a disjoint run of frames and a mempool over exactly
	// those, so no two free lists can ever name the same frame. The division
	// is rarely exact, so the first frames%queues queues take one frame more:
	// every frame in the region has exactly one owner, with none left at the
	// end belonging to nobody.
	perQueue := cfg.frames / max(queues, 1)
	leftover := cfg.frames % max(queues, 1)
	take := func() int {
		if leftover > 0 {
			leftover--
			return perQueue + 1
		}
		return perQueue
	}
	first := 0
	for i := 0; i < cfg.txQueues; i++ {
		starts := make([]int, rings)
		counts := make([]int, rings)
		for r := 0; r < rings; r++ {
			counts[r] = take()
			starts[r] = first
			first += counts[r]
		}
		q, err := d.newTxQueue(i, starts, counts)
		if err != nil {
			return nil, fmt.Errorf("dpdk: transmit queue %d: %w", i, err)
		}
		d.tx = append(d.tx, q)
	}
	for i := 0; i < cfg.rxQueues; i++ {
		n := take()
		q, err := d.newRxQueue(i, first, n)
		if err != nil {
			return nil, fmt.Errorf("dpdk: receive queue %d: %w", i, err)
		}
		d.rx = append(d.rx, q)
		first += n
	}

	// Fill everything the queue owns, not just the ring depth: the driver
	// takes a full ring's worth the moment the port starts, and a supply
	// pre-filled with exactly that comes up empty -- every packet then races
	// the application's next Fill for a buffer, which the port counts as
	// rx_out_of_buffer while half the queue's frames idle in the free list.
	for _, q := range d.rx {
		q.q.Fill(q.q.NumFreeFillSlots())
	}
	// Every queue the port was configured for now exists, which is what makes
	// the port safe to close.
	d.ready = true

	if err := eal.Start(port); err != nil {
		return nil, err
	}
	d.started = true

	// Wait for carrier before handing the device over. A copper port
	// negotiates for seconds after it starts, and a caller that transmits into
	// a link that is not up yet fills the ring and spins on it -- measured on
	// an X550 at 10GBASE-T as tens of millions of empty batches before the
	// first packet moved. Waiting costs nothing on a link that is already up.
	d.info.LinkUp, d.info.SpeedMbps = eal.WaitLink(port, linkWait)

	// Rules go in last: rte_flow_create refuses on a stopped port, and nothing
	// should arrive before there is somewhere to put it.
	if len(steer) > 0 {
		qs := make([]uint16, len(d.rx))
		for i := range d.rx {
			qs[i] = uint16(i)
		}
		for _, m := range steer {
			if err := eal.Flow(port, m, qs, false); err != nil {
				return nil, fmt.Errorf("%w: %w", packetio.ErrUnsupported, err)
			}
		}
	}
	return d, nil
}

// mempoolHeader is what the mempool library puts in front of every object.
//
// Read from the installed DPDK headers rather than hardcoded, because it is
// cache-line aligned: 64 on x86-64 and 128 on aarch64. It is still checked
// against the mempool actually built rather than trusted.
var mempoolHeader = eal.Headers().ObjHeader

// linkWait is how long Open waits for carrier. 10GBASE-T autonegotiation is
// the slow case and takes a few seconds; a device whose link never comes up is
// still returned, with Info.LinkUp false, because a caller may be opening it to
// receive on a link the far end has not brought up yet.
const linkWait = 9 * time.Second

// maxEthernetHeader is a header with two VLAN tags, the most this backend
// expects in front of an MTU-sized payload.
const maxEthernetHeader = 22

// newTxQueue builds one logical transmit queue over one or more hardware
// send queues. Ring r of logical queue i is PMD queue i*rings+r, and each
// ring has its own mempool over its own slice of the region, so the driver
// frees every mbuf back to the ring that sent it and frame ownership stays
// provable per ring.
func (d *Device) newTxQueue(index int, starts, counts []int) (*TxQueue, error) {
	tq := &TxQueue{dev: d, index: index}
	for r := range starts {
		pmdIdx := index*len(starts) + r
		mp, err := eal.NewMempool(fmt.Sprintf("piotx%d_%d_%d", d.port, index, r), d.region,
			starts[r], counts[r], d.cfg.frameSize, ringSize(counts[r]), d.info.Socket)
		if err != nil {
			return nil, err
		}
		d.pools = append(d.pools, mp)
		if err := d.checkLayout(mp); err != nil {
			return nil, err
		}
		if err := eal.SetupTx(d.port, pmdIdx, d.cfg.txDepth, d.info.Socket); err != nil {
			return nil, err
		}
		q, err := queue.NewTx(queue.Config{
			Region: d.region.Bytes, RegionVA: d.region.VA, Layout: d.layout,
			PoolVA: mp.VA, FirstFrame: starts[r], Frames: counts[r], Depth: d.cfg.txDepth,
			Returned: mp.Returned, PMD: eal.Queue{Port: d.port, Index: uint16(pmdIdx)},
			Checksums: d.txChecksums, TSO: d.tso,
		})
		if err != nil {
			return nil, err
		}
		tq.qs = append(tq.qs, q)
		tq.pools = append(tq.pools, mp)
	}
	return tq, nil
}

func (d *Device) newRxQueue(index, firstFrame, frames int) (*RxQueue, error) {
	mp, err := eal.NewMempool(fmt.Sprintf("piorx%d_%d", d.port, index), d.region,
		firstFrame, frames, d.cfg.frameSize, ringSize(frames), d.info.Socket)
	if err != nil {
		return nil, err
	}
	d.pools = append(d.pools, mp)
	if err := d.checkLayout(mp); err != nil {
		return nil, err
	}
	if err := eal.SetupRx(d.port, index, d.cfg.rxDepth, d.info.Socket, mp); err != nil {
		return nil, err
	}
	q, err := queue.NewRx(queue.Config{
		Region: d.region.Bytes, RegionVA: d.region.VA, Layout: d.layout,
		PoolVA: mp.VA, FirstFrame: firstFrame, Frames: frames, Depth: d.cfg.rxDepth,
		Supply: mp.Supply, Returned: mp.Returned,
		PMD: eal.Queue{Port: d.port, Index: uint16(index)},
	})
	if err != nil {
		return nil, err
	}
	return &RxQueue{q: q, dev: d, pool: mp, index: index}, nil
}

// checkLayout compares what the mempool library built against what the frame
// arithmetic assumes.
//
// It is not defensive programming for its own sake. DPDK pads between objects
// to stripe them across memory channels unless it is told not to, and an object
// that is not exactly one frame moves every mbuf a little further along than
// the layout expects -- which would be read as silent nonsense, not as an error.
func (d *Device) checkLayout(mp *eal.Mempool) error {
	header, elt, trailer := mp.Layout()
	if header != d.layout.ObjHeader || trailer != 0 || header+elt+trailer != d.cfg.frameSize {
		return fmt.Errorf("dpdk: this DPDK lays out a mempool object as %d + %d + %d bytes, "+
			"and this backend's frame arithmetic needs %d + %d + 0 = %d",
			header, elt, trailer, d.layout.ObjHeader, d.cfg.frameSize-d.layout.ObjHeader,
			d.cfg.frameSize)
	}
	return nil
}

// ringSize is how many addresses a queue's rings must hold, rounded up to a
// power of two.
//
// It is sized from the queue's whole frame count rather than from its ring
// depth, because populating the mempool hands every object to the returned ring
// at once. Sized from the depth it overflowed on every Open -- 2048 drops for a
// 4096-frame queue -- and a drop there is a frame lost for good. The counter
// said so plainly, which is why it exists.
func ringSize(frames int) int { return nextPow2(frames) }

// Info describes the open device.
func (d *Device) Info() Info { return d.info }

// Capabilities reports what this backend and this device can do.
func (d *Device) Capabilities() packetio.Capabilities {
	c := packetio.Capabilities{
		Backend: "dpdk",
		// Every physical PMD DMAs into the region. net_af_packet, the vdev
		// the tests run on, is the one supported driver that does not: it
		// copies through an AF_PACKET socket, and saying otherwise here
		// would be exactly the untruthful capability this field forbids.
		ZeroCopy: d.info.Driver != "net_af_packet",
		// Whether the kernel kept a netdev is a property of the opened
		// device, not of this backend: true behind a bifurcated ConnectX or
		// the af_packet vdev, false on anything bound to vfio-pci.
		KernelCoexistence: d.info.Coexists,
		MultiBuffer:       false, // one frame per packet; the MTU is kept inside one
		RSS:               len(d.rx) > 1,
		BlockingPoll:      false, // polling only: no interrupt is armed
		SharedRegion:      true,
		// The driver frees each mbuf into the pool the mbuf names, and this
		// backend stamps that at Transmit, so a reclaimed frame can always be
		// named and handed back.
		HandsBackFrames: true,
		MaxFrameSize:    d.layout.MaxPacket(),
		MaxQueues:       d.maxQueues,
	}
	// Reported from what the device took at configure time. Offload stays
	// false: it means per-frame metadata in both directions, and this backend
	// carries it only outbound -- a receive queue has no LRO to report, so
	// there is no OffloadReceiver to assert.
	c.TxChecksumOffload = d.txChecksums
	c.RxChecksumFlags = d.rxChecksums
	return c
}

// NumTxQueues and NumRxQueues are how many queues were opened.
func (d *Device) NumTxQueues() int { return len(d.tx) }

// NumRxQueues is how many receive queues were opened.
func (d *Device) NumRxQueues() int { return len(d.rx) }

// TxQueue returns transmit queue i, or nil if there is no such queue.
func (d *Device) TxQueue(i int) packetio.TxQueue {
	if i < 0 || i >= len(d.tx) {
		return nil
	}
	return d.tx[i]
}

// RxQueue returns receive queue i, or nil if there is no such queue.
func (d *Device) RxQueue(i int) packetio.RxQueue {
	if i < 0 || i >= len(d.rx) {
		return nil
	}
	return d.rx[i]
}

// Tx and Rx return a queue as its concrete type, for what is specific here.
func (d *Device) Tx(i int) *TxQueue {
	if i < 0 || i >= len(d.tx) {
		return nil
	}
	return d.tx[i]
}

// Rx returns the concrete receive queue i, or nil when i is out of range.
func (d *Device) Rx(i int) *RxQueue {
	if i < 0 || i >= len(d.rx) {
		return nil
	}
	return d.rx[i]
}

// Region is the frame memory every queue of this device draws on.
func (d *Device) Region() packetio.Region { return &region{d: d} }

// PortStats are the device's own counters, which are the ones to believe about
// what reached the wire.
func (d *Device) PortStats() (eal.Stats, error) { return eal.PortStats(d.port) }

// PortXStats are the driver's own named counters, in the order it lists them.
//
// What is in the set is entirely the driver's business: most PMDs publish
// per-queue totals, and some publish things no generic counter could carry. On
// EC2 this is the only way to read ENA's shaping counters --
// pps_allowance_exceeded, bw_in_allowance_exceeded, conntrack_allowance_exceeded
// and the rest -- from a process that owns the device, since the kernel driver
// that answers ethtool -S is not attached to it. A driver with nothing to add
// returns no stats and no error.
//
// It reads the whole set every call and allocates to do it, so it belongs
// beside PortStats in a monitoring goroutine, not on the packet path.
func (d *Device) PortXStats() ([]eal.XStat, error) { return eal.PortXStats(d.port) }

// PortXStatsMap is PortXStats keyed by name, for a caller that wants to look a
// counter up rather than walk the set. A driver that publishes the same name
// twice keeps its last value, which no driver does.
func (d *Device) PortXStatsMap() (map[string]uint64, error) {
	xs, err := d.PortXStats()
	if err != nil {
		return nil, err
	}
	m := make(map[string]uint64, len(xs))
	for _, x := range xs {
		m[x.Name] = x.Value
	}
	return m, nil
}

// Close shuts down every queue and releases the device.
//
// The order is not negotiable: the queues are told first so that a goroutine in
// Poll stops touching the driver, then the port is stopped and closed, then the
// mempools and the region go, and finally the device is removed from the
// environment so the same one can be opened again.
//
// A device that Open gave up on part-way is treated differently: the port is
// left probed, and configured if it got that far, with its isolation and
// promiscuous mode undone and its memory released -- the PMD keeps a stale
// mempool pointer in any queue it did set up, which the next Open's queue
// setup replaces before anything reads it. Closing a port whose queues were
// never all set up is a fault in some drivers -- ENA walks the queue table
// and dereferences the gaps -- and
// there is nothing in such a port worth closing. The next Open finds it again
// and configures it afresh.
//
// It is safe to call twice, and safe to call while another goroutine sits in
// Poll -- that goroutine returns ErrClosed. It is not safe to call while one is
// part-way through Receive or Transmit.
func (d *Device) Close() error {
	if d == nil {
		return nil
	}
	d.closeMu.Lock()
	defer d.closeMu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true

	var errs []error
	// Tell the queues before anything is taken away. Each receive queue's
	// close also waits for a poller to leave the driver.
	for _, q := range d.tx {
		for _, t := range q.qs {
			t.Close()
		}
		q.closed.Store(true)
	}
	for _, q := range d.rx {
		q.shut()
	}
	if d.started {
		if err := eal.Stop(d.port); err != nil {
			errs = append(errs, err)
		}
		d.started = false
	}
	if d.ready {
		if err := eal.Close(d.port); err != nil {
			errs = append(errs, err)
		}
	} else {
		// Isolation and promiscuous mode are both asked for before the port
		// is configured and neither is reset by anything short of closing
		// it, so a half-open Close takes them back by hand. A bifurcated
		// port left isolated receives nothing for the kernel; a port left
		// promiscuous is replayed as promiscuous by every start after it,
		// and would take everything while reporting that it takes nothing.
		if d.isolated {
			if err := eal.Isolate(d.port, false); err != nil {
				errs = append(errs, err)
			}
			d.isolated = false
		}
		if d.info.Promiscuous {
			// A driver can grant the mode without having it -- the null
			// vdev is born promiscuous, so enabling returns early and
			// disabling reaches a missing op -- which is not a failure.
			if err := eal.Promiscuous(d.port, false); err != nil && !errors.Is(err, eal.ErrNoPromiscuous) {
				errs = append(errs, err)
			}
			d.info.Promiscuous = false
		}
	}
	for _, mp := range d.pools {
		mp.Free()
	}
	d.pools = nil
	if err := d.region.Free(); err != nil {
		errs = append(errs, err)
	}
	// Removing the device is what lets the same one be opened again. Without
	// it a program that opens and closes in a loop runs out of ports. A port
	// left half-open is not removed: removing it closes it, and Open finds a
	// port that is still there before probing for a new one.
	if d.ready {
		if err := eal.Remove(d.handle); err != nil {
			errs = append(errs, err)
		}
	}
	d.handle = nil
	return errors.Join(errs...)
}

func nextPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func iovaName(m int) string {
	switch m {
	case 1:
		return "physical"
	case 2:
		return "virtual"
	}
	return "unknown"
}

func regionName(p eal.Port) string { return fmt.Sprintf("pio_region_%d", p) }

var _ packetio.Device = (*Device)(nil)
