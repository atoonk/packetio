//go:build linux && cgo && dpdk && (amd64 || arm64)

// Command l3fwd is an IPv4 router on one port, through DPDK: it takes the
// packets steered to it, looks each destination up in a forwarding table,
// rewrites the Ethernet header and the TTL, and sends the packet back out the
// same port.
//
// The forwarding itself -- the table, the parse and the rewrite -- is the same
// code the native mlx5 example runs (examples/internal/forward), so a
// comparison between the two measures the packet path and nothing else.
//
//	sudo l3fwd -i 0000:c1:00.1 -vlan 2053 -queues 4 \
//	    -route 10.0.0.0/8,7c:c2:55:be:f4:e1,2043
//
// Nothing is copied and nothing is allocated on the packet path: a frame that
// arrives is rewritten where it lies and handed to a transmit queue of the
// same device, then returned to the receive queue it came from.
//
// A ConnectX and other bifurcated cards need no setup at all -- no devbind, no
// IOMMU, not even hugepages. Any other PCI card must be handed over first, and
// wants an IOMMU and hugepages with it: see docs/dpdk.md. Run info to find out
// which kind you have.
package main

import (
	"context"
	"flag"
	"fmt"
	"math/bits"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/dpdk"
	"github.com/atoonk/packetio/examples/internal/affinity"
	"github.com/atoonk/packetio/examples/internal/forward"
)

type config struct {
	dev         string
	iface       string
	queues      int
	txPerWorker int
	rxDepth     int
	txDepth     int
	batch       int
	cpus        []int
	noAff       bool
	dur         time.Duration
	report      time.Duration
	devargs     string
	frameSize   int
	csum        bool
	routes      []route
}

// route is one -route flag: where to send what.
type route struct {
	prefix  netip.Prefix
	nextHop net.HardwareAddr
	vlan    uint16
}

type routeFlags []route

func (r *routeFlags) String() string { return fmt.Sprint(len(*r), " routes") }

func (r *routeFlags) Set(s string) error {
	parts := strings.Split(s, ",")
	if len(parts) < 2 || len(parts) > 3 {
		return fmt.Errorf("%q: want prefix,next-hop-mac[,vlan]", s)
	}
	p, err := netip.ParsePrefix(parts[0])
	if err != nil {
		return err
	}
	if !p.Addr().Is4() {
		return fmt.Errorf("%s: only IPv4 routes are forwarded here", parts[0])
	}
	hw, err := net.ParseMAC(parts[1])
	if err != nil {
		return err
	}
	if len(hw) != 6 {
		// ParseMAC also accepts 8- and 20-byte forms, which would be
		// truncated into the Ethernet header without a word.
		return fmt.Errorf("%s is a %d-byte address; an Ethernet next hop is six bytes",
			parts[1], len(hw))
	}
	rt := route{prefix: p.Masked(), nextHop: hw}
	if len(parts) == 3 {
		v, err := strconv.Atoi(parts[2])
		if err != nil {
			return fmt.Errorf("%s: %w", parts[2], err)
		}
		if v < 0 || v > 4094 {
			return fmt.Errorf("vlan %d in a route; it must be 0 (untagged) to 4094", v)
		}
		rt.vlan = uint16(v)
	}
	*r = append(*r, rt)
	return nil
}

func main() {
	var (
		dev     = flag.String("i", "", "interface, PCI address or vdev")
		iface   = flag.String("counters", "", "kernel interface whose port counters to read")
		queues  = flag.Int("queues", 1, "receive queues, one worker each")
		txPer   = flag.Int("tx-per-worker", 1, "transmit queues each worker drives")
		rxDepth = flag.Int("rx-depth", 1024, "receive queue depth")
		txDepth = flag.Int("tx-depth", 1024, "transmit queue depth")
		batch   = flag.Int("batch", 64, "packets per receive burst")
		cpus    = flag.String("cpus", "", "processors to pin the workers to")
		noAff   = flag.Bool("no-affinity", false, "leave the workers where the scheduler puts them")
		vlan    = flag.Int("vlan", 0, "steer this VLAN")
		dstPort = flag.Int("dst-port", 0, "steer this UDP destination port, or 0 for any")
		promisc = flag.Bool("promisc", false, "take every packet the port sees")

		duration  = flag.Duration("duration", 0, "how long to run; 0 until interrupted")
		report    = flag.Duration("report", time.Second, "how often to print a line")
		csum      = flag.Bool("csum", false, "let the NIC check the IPv4 header checksum on receive")
		noHuge    = flag.Int("no-huge", 0, "run on ordinary memory with this many megabytes, for a vdev")
		devargs   = flag.String("devargs", "", "extra driver arguments")
		frameSize = flag.Int("frame-size", 0, "bytes per frame; 0 is the backend's default. Some drivers need more room for a packet than that leaves")

		routes routeFlags
	)
	flag.Var(&routes, "route", "prefix,next-hop-mac[,vlan]: send packets for prefix to that "+
		"address, tagged with vlan (0 or absent for untagged); repeatable")
	flag.Parse()

	if *dev == "" || len(routes) == 0 {
		fmt.Fprintln(os.Stderr, "a device and at least one -route are needed")
		flag.Usage()
		os.Exit(2)
	}
	c := config{
		dev: *dev, iface: *iface, queues: *queues, txPerWorker: *txPer,
		rxDepth: *rxDepth, txDepth: *txDepth, batch: *batch, noAff: *noAff,
		dur: *duration, report: *report, devargs: *devargs, frameSize: *frameSize, csum: *csum,
		routes: routes,
	}
	if c.iface == "" && !strings.Contains(*dev, ":") && !strings.HasPrefix(*dev, "net_") {
		c.iface = *dev
	}
	switch {
	case c.queues <= 0:
		fmt.Fprintf(os.Stderr, "-queues must be at least 1, not %d\n", c.queues)
		os.Exit(2)
	case c.txPerWorker <= 0:
		fmt.Fprintf(os.Stderr, "-tx-per-worker must be at least 1, not %d; a worker with "+
			"no transmit queue cannot forward\n", c.txPerWorker)
		os.Exit(2)
	case c.batch <= 0:
		fmt.Fprintf(os.Stderr, "-batch must be at least 1, not %d\n", c.batch)
		os.Exit(2)
	case c.report <= 0:
		fmt.Fprintf(os.Stderr, "-report must be longer than zero, not %s\n", c.report)
		os.Exit(2)
	case *vlan < 0 || *vlan > 4094:
		fmt.Fprintf(os.Stderr, "-vlan must be 0 (no vlan match) to 4094, not %d\n", *vlan)
		os.Exit(2)
	}
	var err error
	if c.cpus, err = affinity.Parse(*cpus); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := run(c, *vlan, *dstPort, *promisc, *noHuge); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// forwarder is what every worker shares, and none of them writes to.
type forwarder struct {
	fib  *forward.V4Fib
	adjs []forward.Adjacency
}

// buildForwarder turns the routes into a table and the adjacencies they point
// at. Adjacency 0 is "no route", so the first real one is 1.
func buildForwarder(routes []route, src net.HardwareAddr) (*forwarder, error) {
	if len(src) != 6 {
		return nil, fmt.Errorf("the source address is %d bytes, not six", len(src))
	}
	var s [6]byte
	copy(s[:], src)

	fw := &forwarder{fib: forward.NewV4Fib(), adjs: make([]forward.Adjacency, 1, len(routes)+1)}
	for _, rt := range routes {
		var dst [6]byte
		copy(dst[:], rt.nextHop)
		fw.adjs = append(fw.adjs, forward.NewAdjacency(dst, s, rt.vlan))
		p := rt.prefix.Addr().As4()
		fw.fib.Insert(uint32(p[0])<<24|uint32(p[1])<<16|uint32(p[2])<<8|uint32(p[3]),
			uint8(rt.prefix.Bits()), uint32(len(fw.adjs)-1))
	}
	return fw, nil
}

func run(c config, vlan, dstPort int, promisc bool, noHuge int) error {
	txQueues := c.queues * c.txPerWorker
	frames := nextPow2(c.queues*c.rxDepth*2 + txQueues*c.txDepth*2)

	opts := []dpdk.Option{
		dpdk.WithRxQueues(c.queues), dpdk.WithTxQueues(txQueues),
		dpdk.WithRxDepth(c.rxDepth), dpdk.WithTxDepth(c.txDepth),
		dpdk.WithFrames(frames),
	}
	var m []packetio.Match
	if vlan > 0 {
		m = append(m, packetio.MatchVLAN(uint16(vlan)))
	}
	if dstPort > 0 {
		m = append(m, packetio.MatchDstPort(17, uint16(dstPort))...)
	}
	if len(m) > 0 || promisc {
		opts = append(opts, dpdk.WithSteering(packetio.SteeringFilter{
			Match: m, Promiscuous: promisc,
		}))
	}
	if promisc {
		opts = append(opts, dpdk.WithPromiscuous())
	}
	if c.csum {
		opts = append(opts, dpdk.WithChecksumOffload())
	}
	if c.devargs != "" {
		opts = append(opts, dpdk.WithDevArgs(c.devargs))
	}
	if c.frameSize > 0 {
		opts = append(opts, dpdk.WithFrameSize(c.frameSize))
	}
	if noHuge > 0 {
		opts = append(opts, dpdk.WithoutHugePages(noHuge))
	}
	if c.noAff {
		opts = append(opts, dpdk.WithoutAffinity())
	} else if len(c.cpus) > 0 {
		opts = append(opts, dpdk.WithAffinity(c.cpus...))
	}

	device, err := dpdk.Open(c.dev, opts...)
	if err != nil {
		return err
	}
	defer device.Close()

	src, err := sourceMAC(c.iface, device)
	if err != nil {
		return err
	}
	fw, err := buildForwarder(c.routes, src)
	if err != nil {
		return err
	}

	i := device.Info()
	fmt.Printf("%s on %s, port %d, link %s\n", i.Device, i.Driver, i.Port, upDown(i.LinkUp))
	fmt.Printf("%d receive queues of %d, %d transmit of %d, %d routes, source %s\n",
		i.RxQueues, i.RxDepth, i.TxQueues, i.TxDepth, fw.fib.Routes(), src)
	if i.Steering != "" {
		fmt.Printf("taking %s\n", i.Steering)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if c.dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.dur)
		defer cancel()
	}

	cnts := make([]counters, c.queues)
	var wg sync.WaitGroup
	errs := make(chan error, c.queues)
	for q := 0; q < c.queues; q++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := worker(ctx, device, q, c, fw, &cnts[q]); err != nil {
				errs <- err
			}
		}()
	}

	repErr := reportLoop(ctx, device, c, cnts)
	wg.Wait()
	close(errs)
	if repErr != nil {
		return repErr
	}
	for err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// counters is one worker's tally, padded so two workers never share a line.
type counters struct {
	rx    atomic.Uint64
	fwd   atomic.Uint64
	drops [forward.NumErrors]atomic.Uint64
	_     [64]byte
}

// worker drives one receive queue and the transmit queues that carry its
// frames. Everything a packet needs is done here, in one pass, on one core.
//
// The frames belong to the receive queue, so they go home through Reclaim and
// Recycle rather than Complete -- which would hand them to the transmit
// queue's own free list, where the receive queue could never find them again.
func worker(ctx context.Context, dev *dpdk.Device, index int, c config,
	fw *forwarder, cnt *counters) error {
	rx, ok := dev.RxQueue(index).(*dpdk.RxQueue)
	if !ok {
		return fmt.Errorf("receive queue %d is not a dpdk queue", index)
	}
	txs := make([]*dpdk.TxQueue, 0, c.txPerWorker)
	for i := index * c.txPerWorker; i < (index+1)*c.txPerWorker; i++ {
		q, ok := dev.TxQueue(i).(*dpdk.TxQueue)
		if !ok {
			return fmt.Errorf("transmit queue %d is not a dpdk queue", i)
		}
		txs = append(txs, q)
	}
	if _, err := rx.Pin(); err != nil {
		return err
	}

	// The region's bytes and frame size, taken once: going through the
	// interface for every packet costs a call the compiler cannot inline and a
	// divide to find the end of the frame, where this loop uses a shift.
	region := rx.Region()
	mem := region.Bytes()
	frameShift := uint(bits.TrailingZeros(uint(region.FrameSize())))

	out := make([]packetio.Desc, 0, c.batch)
	drop := make([]packetio.Desc, 0, c.batch)
	back := make([]packetio.Desc, 0, c.txDepth)

	// Nothing arrives until the NIC has been given somewhere to put it.
	rx.Fill(rx.NumFreeFillSlots())

	next := 0
	for ctx.Err() == nil {
		descs := rx.Receive(c.batch)
		if len(descs) == 0 {
			back = reclaim(txs, rx, back)
			rx.Fill(rx.NumFreeFillSlots())
			continue
		}

		var dropped [forward.NumErrors]uint64
		out, drop = out[:0], drop[:0]
		for _, d := range descs {
			end := (d.Addr>>frameShift + 1) << frameShift
			buf := mem[d.Addr:end:end]
			l3, dst, code := forward.Parse(buf[:d.Len], d.Options&packetio.OptL3ChecksumOK != 0)
			if code != forward.ErrNone {
				dropped[code]++
				drop = append(drop, d)
				continue
			}
			adj := fw.fib.Lookup(dst)
			if adj == 0 {
				dropped[forward.ErrNoRoute]++
				drop = append(drop, d)
				continue
			}
			n := forward.Rewrite(buf, l3, &fw.adjs[adj])
			if n == 0 {
				drop = append(drop, d)
				continue
			}
			d.Len = uint32(n)
			out = append(out, d)
		}

		sent := 0
		if len(out) > 0 {
			// Round-robin over this worker's transmit queues: one send queue
			// tops out below what a core can forward, so a worker above that
			// rate needs more than one.
			tx := txs[next]
			if next++; next == len(txs) {
				next = 0
			}
			sent = tx.Transmit(out)
			if sent < len(out) {
				// A full ring is a drop with its own count, not something to
				// wait for: a worker that spins on it looks busy for nothing.
				dropped[forward.ErrTxFull] += uint64(len(out) - sent)
				drop = append(drop, out[sent:]...)
			}
		}
		rx.Recycle(drop)
		back = reclaim(txs, rx, back)
		rx.Fill(rx.NumFreeFillSlots())

		cnt.rx.Add(uint64(len(descs)))
		cnt.fwd.Add(uint64(sent))
		for i, n := range dropped {
			if n != 0 {
				cnt.drops[i].Add(n)
			}
		}
	}
	return nil
}

// reclaim takes finished frames from the transmit queues and gives them back
// to the receive queue they came from. This is the whole reason the backend
// has to be able to name the frames it reclaimed.
func reclaim(txs []*dpdk.TxQueue, rx *dpdk.RxQueue, back []packetio.Desc) []packetio.Desc {
	for _, tx := range txs {
		back = tx.Reclaim(cap(back), back[:0])
		if len(back) > 0 {
			rx.Recycle(back)
		}
	}
	return back
}

// sourceMAC is the address to put in the rewritten frames: the kernel
// interface's own where there is one, and otherwise it must be given.
func sourceMAC(iface string, dev *dpdk.Device) (net.HardwareAddr, error) {
	if iface != "" {
		if ifc, err := net.InterfaceByName(iface); err == nil {
			return ifc.HardwareAddr, nil
		}
	}
	if mac := dev.Info().MAC; mac != ([6]byte{}) {
		return net.HardwareAddr(mac[:]), nil
	}
	return nil, fmt.Errorf("no source address: this device has no kernel interface, " +
		"so -counters must name one")
}

func upDown(b bool) string {
	if b {
		return "up"
	}
	return "down"
}

func nextPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}
