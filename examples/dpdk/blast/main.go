//go:build linux && cgo && dpdk && amd64

// Command blast transmits UDP frames as fast as it can through DPDK, and says
// what that cost.
//
// It is the transmit half of the measurement pair: rates come from the port's
// own counters where they can be read, cost from whole-machine processor time,
// and cycles per packet from both together. What is being measured is the
// packet path, so the frame is built once and copied.
//
//	sudo blast -i 0000:c1:00.1 -dst-mac 7c:c2:55:be:f4:e1 -vlan 2043 -queues 4
//
// On a device the kernel still owns -- a ConnectX under mlx5_core -- the port
// is shared and nothing needs unbinding. On one this process must own outright
// the device has to be on vfio-pci first; info will say which of the two it is.
//
// A ConnectX and other bifurcated cards need no setup at all -- no devbind, no
// IOMMU, not even hugepages. Any other PCI card must be handed over first, and
// wants an IOMMU and hugepages with it: see docs/dpdk.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/dpdk"
	"github.com/atoonk/packetio/examples/internal/affinity"
	"github.com/atoonk/packetio/examples/internal/frame"
)

type config struct {
	dev      string
	iface    string
	queues   int
	perWkr   int
	batch    int
	depth    int
	frames   int
	cpus     []int
	noAff    bool
	dur      time.Duration
	report   time.Duration
	ghz      float64
	pps      float64
	prebuilt bool
	csum     bool
	devargs  string
	size     int
	frameSz  int
	mtu      int
}

func main() {
	var (
		dev    = flag.String("i", "", "interface, PCI address or vdev, for example 0000:c1:00.1")
		iface  = flag.String("counters", "", "kernel interface whose port counters to read; defaults to -i when it names one")
		dstMAC = flag.String("dst-mac", "", "destination Ethernet address (required)")
		srcMAC = flag.String("src-mac", "", "source Ethernet address (required for a PCI address)")
		vlan   = flag.Int("vlan", 0, "VLAN id to tag with, or 0 for untagged")
		outer  = flag.Int("outer-vlan", 0, "outer VLAN id, for double-tagged frames")
		size   = flag.Int("size", 64, "frame size on the wire including the 4-byte check sequence the NIC adds")

		queues  = flag.Int("queues", 1, "transmit queues to use")
		perWkr  = flag.Int("queues-per-worker", 1, "queues one worker drives, round-robin")
		batch   = flag.Int("batch", 256, "packets per transmit batch; the cgo crossing is per batch, not per packet")
		depth   = flag.Int("depth", 1024, "transmit queue depth in packets")
		frames  = flag.Int("frames", 0, "frames in the region; 0 sizes it from the queues")
		frameSz = flag.Int("frame-size", 0, "bytes per frame; 0 is the backend's default. A jumbo -size needs a frame that holds it")
		mtu     = flag.Int("mtu", 0, "MTU to configure the port for; 0 is the backend's default")
		cpus    = flag.String("cpus", "", "processors to pin the workers to, for example 4,6 or 8-11")
		noAff   = flag.Bool("no-affinity", false, "leave the workers wherever the scheduler puts them")

		duration = flag.Duration("duration", 10*time.Second, "how long to run")
		report   = flag.Duration("report", time.Second, "how often to print a line")
		ghz      = flag.Float64("ghz", 0, "processor speed for the cycles-per-packet estimate; 0 reads it from the system")
		prebuilt = flag.Bool("prebuilt", false, "write the frame into every buffer once at startup and only stamp what varies per packet, instead of rewriting the whole frame each time")
		pps      = flag.Float64("pps", 0, "packets per second in total, or 0 for as fast as possible. Pace the sender when measuring a receiver: a line-rate micro-burst overflows any ring and looks like a steering failure")

		srcIP   = flag.String("src-ip", "10.0.0.1", "source IPv4 address")
		dstIP   = flag.String("dst-ip", "10.0.0.2", "destination IPv4 address")
		dstPort = flag.Int("dst-port", 9, "UDP destination port")
		flows   = flag.Int("flows", 1, "source ports to cycle through, to spread the receiver's load")

		csum    = flag.Bool("csum", false, "have the NIC compute the checksums")
		devargs = flag.String("devargs", "", "extra driver arguments, key=value,key=value")
		noHuge  = flag.Int("no-huge", 0, "run on ordinary memory with this many megabytes, for a vdev")
	)
	flag.Parse()

	if *dev == "" || *dstMAC == "" {
		fmt.Fprintln(os.Stderr, "need at least -i and -dst-mac")
		flag.Usage()
		os.Exit(2)
	}

	c := config{
		dev: *dev, iface: *iface, queues: *queues, perWkr: *perWkr,
		batch: *batch, depth: *depth, frames: *frames, noAff: *noAff,
		dur: *duration, report: *report, ghz: *ghz, pps: *pps, prebuilt: *prebuilt, csum: *csum,
		devargs: *devargs, size: *size, frameSz: *frameSz, mtu: *mtu,
	}
	if c.iface == "" && !strings.Contains(*dev, ":") && !strings.HasPrefix(*dev, "net_") {
		c.iface = *dev
	}
	switch {
	case c.queues <= 0:
		fmt.Fprintf(os.Stderr, "-queues must be at least 1, not %d\n", c.queues)
		os.Exit(2)
	case c.perWkr <= 0:
		fmt.Fprintf(os.Stderr, "-queues-per-worker must be at least 1, not %d\n", c.perWkr)
		os.Exit(2)
	case c.batch <= 0:
		fmt.Fprintf(os.Stderr, "-batch must be at least 1, not %d\n", c.batch)
		os.Exit(2)
	case c.report <= 0:
		fmt.Fprintf(os.Stderr, "-report must be longer than zero, not %s\n", c.report)
		os.Exit(2)
	case *vlan < 0 || *vlan > 4094 || *outer < 0 || *outer > 4094:
		fmt.Fprintf(os.Stderr, "a vlan id must be 0 (untagged) to 4094\n")
		os.Exit(2)
	}
	var err error
	if c.cpus, err = affinity.Parse(*cpus); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := run(c, *dstMAC, *srcMAC, *vlan, *outer, *srcIP, *dstIP, *dstPort, *flows, *noHuge); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(c config, dm, sm string, vlan, outer int, si, di string, dport, flows, noHuge int) error {
	dstMAC, err := net.ParseMAC(dm)
	if err != nil {
		return fmt.Errorf("-dst-mac: %w", err)
	}
	srcMAC, err := sourceMAC(sm, c.iface)
	if err != nil {
		return err
	}
	srcIP, err := netip.ParseAddr(si)
	if err != nil {
		return fmt.Errorf("-src-ip: %w", err)
	}
	dstIP, err := netip.ParseAddr(di)
	if err != nil {
		return fmt.Errorf("-dst-ip: %w", err)
	}

	// The wire size includes the check sequence the NIC appends; what is
	// handed to the device is four bytes shorter.
	tmpl, err := frame.Build(frame.Spec{
		SrcMAC: srcMAC, DstMAC: dstMAC, VLAN: vlan, OuterVLAN: outer,
		SrcIP: srcIP, DstIP: dstIP, SrcPort: 1024, DstPort: uint16(dport),
		Size: c.size - frame.FCS,
	})
	if err != nil {
		return err
	}

	frames := c.frames
	if frames == 0 {
		frames = nextPow2(c.queues * c.depth * 2)
	}
	opts := []dpdk.Option{
		dpdk.WithTxQueues(c.queues), dpdk.WithRxQueues(0),
		dpdk.WithTxDepth(c.depth), dpdk.WithFrames(frames),
	}
	if c.frameSz > 0 {
		opts = append(opts, dpdk.WithFrameSize(c.frameSz))
	}
	if c.mtu > 0 {
		opts = append(opts, dpdk.WithMTU(c.mtu))
	}
	if c.csum {
		opts = append(opts, dpdk.WithChecksumOffload())
	}
	if c.devargs != "" {
		opts = append(opts, dpdk.WithDevArgs(c.devargs))
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

	i := device.Info()
	if max := device.Capabilities().MaxFrameSize; len(tmpl.Bytes) > max {
		// copy() would silently shorten the frame while the IPv4 header kept
		// claiming the full length, putting malformed packets on the wire.
		return fmt.Errorf("a %d-byte frame does not fit: this device leaves %d bytes for a "+
			"packet in a %d-byte frame. Ask for a smaller -size, or open the device with "+
			"a larger frame", len(tmpl.Bytes), max, i.FrameSize)
	}
	fmt.Printf("%s on %s, port %d, link %s\n", i.Device, i.Driver, i.Port, upDown(i.LinkUp))
	fmt.Printf("%d transmit queues of %d, %d frames of %d bytes, batch %d\n",
		i.TxQueues, i.TxDepth, i.Frames, i.FrameSize, c.batch)
	if c.prebuilt && flows > 1 {
		fmt.Println("note: -prebuilt writes every frame once at startup, so -flows is not " +
			"applied and the run uses a single source port")
	}
	if !i.LinkUp {
		fmt.Println("note: the link is down, so the ring will fill and nothing will leave the port")
	}

	workers := (c.queues + c.perWkr - 1) / c.perWkr
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if c.dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.dur)
		defer cancel()
	}

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		first := w * c.perWkr
		last := min(first+c.perWkr, c.queues)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := worker(ctx, device, first, last, workers, c, tmpl, flows); err != nil {
				errs <- err
			}
		}()
	}

	repErr := reportLoop(ctx, device, c)
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

// worker drives one or more transmit queues from a single goroutine, which is
// pinned to a processor of its own by the backend on first use.
func worker(ctx context.Context, dev *dpdk.Device, first, last, workers int, c config, tmpl *frame.Frame, flows int) error {
	queues := make([]*dpdk.TxQueue, 0, last-first)
	for i := first; i < last; i++ {
		q, ok := dev.TxQueue(i).(*dpdk.TxQueue)
		if !ok {
			return fmt.Errorf("transmit queue %d is not a dpdk queue", i)
		}
		queues = append(queues, q)
	}
	if len(queues) == 0 {
		return nil
	}
	if _, err := queues[0].Pin(); err != nil {
		return err
	}

	pkt := tmpl.Bytes
	if c.prebuilt {
		// Every frame of every queue this worker drives, written once. The
		// frames cycle through their pool and nothing else writes to them, so
		// the packet stays there for the life of the run.
		for _, q := range queues {
			r := q.Region()
			var held []packetio.Desc
			for {
				descs := q.Alloc(c.batch)
				if len(descs) == 0 {
					break
				}
				for _, d := range descs {
					copy(r.Writable(d), pkt)
				}
				held = append(held, descs...)
			}
			q.Free(held)
		}
	}
	port := uint16(1024)
	build := func(_ int, b []byte) int { return copy(b, pkt) }
	if flows > 1 {
		build = func(_ int, b []byte) int {
			n := copy(b, pkt)
			// Vary the source port so a receiver spreads the load. The UDP
			// checksum is left zero, which is legal for IPv4 and is what
			// every generator does.
			b[tmpl.SrcPortOff] = byte(port >> 8)
			b[tmpl.SrcPortOff+1] = byte(port)
			port++
			if port >= 1024+uint16(flows) {
				port = 1024
			}
			return n
		}
	}

	// Pacing is per worker: each takes an equal share of the asked-for rate,
	// measured against its own start rather than a shared clock, so the
	// workers do not have to agree on anything.
	share := 0.0
	if c.pps > 0 {
		share = c.pps / float64(workers)
	}
	start, sent := time.Now(), 0

	for qi := 0; ; qi = (qi + 1) % len(queues) {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if share > 0 {
			if want := time.Since(start).Seconds() * share; float64(sent) > want {
				time.Sleep(100 * time.Microsecond)
				continue
			}
		}
		q := queues[qi]
		var n int
		if c.prebuilt {
			// The frames already hold the packet, written once at startup, so
			// there is nothing to build: take them, say how long they are, and
			// send. What this leaves out is one 60-byte copy per packet, which
			// at these rates is not a rounding error.
			descs := q.Alloc(c.batch)
			for i := range descs {
				descs[i].Len = uint32(len(pkt))
			}
			n = q.Transmit(descs)
			q.Free(descs[n:])
		} else {
			var err error
			if n, err = q.SendFunc(c.batch, build); err != nil {
				return err
			}
		}
		sent += n
		// Completions are the driver's business and it looks at them while
		// sending, so this only drains what the burst already freed.
		q.Complete(c.batch)
	}
}

func sourceMAC(given, iface string) (net.HardwareAddr, error) {
	if given != "" {
		m, err := net.ParseMAC(given)
		if err != nil {
			return nil, fmt.Errorf("-src-mac: %w", err)
		}
		return m, nil
	}
	if iface == "" {
		return nil, fmt.Errorf("no -src-mac, and -i does not name a kernel interface to take one from")
	}
	ifc, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("-src-mac not given and %s: %w", iface, err)
	}
	return ifc.HardwareAddr, nil
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
