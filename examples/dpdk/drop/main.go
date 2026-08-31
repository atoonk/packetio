//go:build linux && cgo && dpdk && amd64

// Command drop receives packets through DPDK and throws them away, reporting
// what arrived and what it cost.
//
// It is the receive half of the measurement pair. Dropping is the cheapest
// thing that can be done with a packet, so what this measures is the receive
// path itself: the driver's burst, the descriptor bookkeeping, and returning
// the frame to the free list.
//
//	sudo drop -i 0000:c1:00.1 -vlan 2043 -dst-port 9000 -queues 2
//
// On a device the kernel still owns, a steering filter is how anything arrives
// at all: without one the kernel keeps the traffic. On a device this process
// owns outright the port's own address filter decides, and -promisc widens it.
//
// A ConnectX and other bifurcated cards need no setup at all -- no devbind, no
// IOMMU, not even hugepages. Any other PCI card must be handed over first, and
// wants an IOMMU and hugepages with it: see docs/dpdk.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/dpdk"
	"github.com/atoonk/packetio/examples/internal/affinity"
	"github.com/atoonk/packetio/examples/internal/metrics"
)

type config struct {
	dev       string
	iface     string
	queues    int
	depth     int
	batch     int
	cpus      []int
	noAff     bool
	dur       time.Duration
	report    time.Duration
	ghz       float64
	csum      bool
	devargs   string
	frameSize int
	verify    bool
}

func main() {
	var (
		dev     = flag.String("i", "", "interface, PCI address or vdev")
		iface   = flag.String("counters", "", "kernel interface whose port counters to read; defaults to -i when it names one")
		queues  = flag.Int("queues", 1, "receive queues to use, one worker each")
		depth   = flag.Int("depth", 1024, "receive queue depth in packets")
		batch   = flag.Int("batch", 64, "packets per receive burst")
		cpus    = flag.String("cpus", "", "processors to pin the workers to")
		noAff   = flag.Bool("no-affinity", false, "leave the workers wherever the scheduler puts them")
		vlan    = flag.Int("vlan", 0, "steer this VLAN, or 0 for no VLAN match")
		dstPort = flag.Int("dst-port", 0, "steer this UDP destination port, or 0 for none")
		promisc = flag.Bool("promisc", false, "take every packet the port sees")

		duration  = flag.Duration("duration", 10*time.Second, "how long to run")
		report    = flag.Duration("report", time.Second, "how often to print a line")
		ghz       = flag.Float64("ghz", 0, "processor speed for the cycles-per-packet estimate")
		csum      = flag.Bool("csum", false, "ask the NIC to verify checksums and report what it found")
		verify    = flag.Bool("verify-checksums", false, "count how many packets the NIC said were correct; implies -csum")
		devargs   = flag.String("devargs", "", "extra driver arguments")
		frameSize = flag.Int("frame-size", 0, "bytes per frame; 0 is the backend's default. Some drivers need more room for a packet than that leaves")
		noHuge    = flag.Int("no-huge", 0, "run on ordinary memory with this many megabytes, for a vdev")
	)
	flag.Parse()

	if *dev == "" {
		fmt.Fprintln(os.Stderr, "no device given; try -i eno2")
		flag.Usage()
		os.Exit(2)
	}
	c := config{
		dev: *dev, iface: *iface, queues: *queues, depth: *depth, batch: *batch,
		noAff: *noAff, dur: *duration, report: *report, ghz: *ghz,
		csum: *csum || *verify, devargs: *devargs, frameSize: *frameSize, verify: *verify,
	}
	if c.iface == "" && !strings.Contains(*dev, ":") && !strings.HasPrefix(*dev, "net_") {
		c.iface = *dev
	}
	switch {
	case c.queues <= 0:
		fmt.Fprintf(os.Stderr, "-queues must be at least 1, not %d\n", c.queues)
		os.Exit(2)
	case c.batch <= 0:
		fmt.Fprintf(os.Stderr, "-batch must be at least 1, not %d; a batch of none "+
			"receives nothing and spins\n", c.batch)
		os.Exit(2)
	case c.report <= 0:
		fmt.Fprintf(os.Stderr, "-report must be longer than zero, not %s\n", c.report)
		os.Exit(2)
	case *vlan < 0 || *vlan > 4094:
		fmt.Fprintf(os.Stderr, "-vlan must be 0 (no vlan match) to 4094, not %d\n", *vlan)
		os.Exit(2)
	case *dstPort < 0 || *dstPort > 65535:
		fmt.Fprintf(os.Stderr, "-dst-port must be 0 to 65535, not %d\n", *dstPort)
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

func run(c config, vlan, dstPort int, promisc bool, noHuge int) error {
	opts := []dpdk.Option{
		dpdk.WithRxQueues(c.queues), dpdk.WithTxQueues(0),
		dpdk.WithRxDepth(c.depth),
		dpdk.WithFrames(nextPow2(c.queues * c.depth * 2)),
	}

	// A filter is compiled to hardware rules, and a match the device will not
	// take is refused at Open rather than installed as something wider.
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

	i := device.Info()
	fmt.Printf("%s on %s, port %d, link %s\n", i.Device, i.Driver, i.Port, upDown(i.LinkUp))
	fmt.Printf("%d receive queues of %d, %d frames of %d bytes\n",
		i.RxQueues, i.RxDepth, i.Frames, i.FrameSize)
	if i.Steering != "" {
		fmt.Printf("taking %s\n", i.Steering)
	}
	if i.Coexists && len(m) == 0 && !promisc {
		fmt.Println("note: the kernel still owns this device and no filter was given, so it " +
			"keeps the traffic and little will arrive here")
	}
	if c.verify && !device.Capabilities().RxChecksumFlags {
		fmt.Println("note: this device does not report checksum results, so none will be counted")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if c.dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.dur)
		defer cancel()
	}

	var wg sync.WaitGroup
	errs := make(chan error, c.queues)
	good := make([]counter, c.queues)
	for q := 0; q < c.queues; q++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := worker(ctx, device, q, c, &good[q]); err != nil {
				errs <- err
			}
		}()
	}

	repErr := reportLoop(ctx, device, c, good)
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

// counter is padded to a cache line so the workers do not share one. It is
// atomic because the reporting goroutine reads it while a worker writes it.
type counter struct {
	n atomic.Uint64
	_ [56]byte
}

func worker(ctx context.Context, dev *dpdk.Device, index int, c config, good *counter) error {
	q, ok := dev.RxQueue(index).(*dpdk.RxQueue)
	if !ok {
		return fmt.Errorf("receive queue %d is not a dpdk queue", index)
	}
	if _, err := q.Pin(); err != nil {
		return err
	}

	// The driver takes buffers in bulks it chooses and will not take a bulk it
	// cannot fill, so the supply is kept full rather than topped up a frame at
	// a time.
	q.Fill(q.NumFreeFillSlots())

	for n := 0; ; n++ {
		if n&0xff == 0 {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
		}
		descs := q.Receive(c.batch)
		if c.verify {
			var ok uint64
			for _, d := range descs {
				if d.Options&packetio.OptChecksumOK != 0 {
					ok++
				}
			}
			good.n.Add(ok)
		}
		q.Recycle(descs)
		q.Fill(q.NumFreeFillSlots())
	}
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

var _ = metrics.CPUHz
