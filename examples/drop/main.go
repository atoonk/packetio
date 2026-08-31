//go:build linux && cgo && mlx5 && (amd64 || arm64)

// Command drop receives packets and throws them away, as fast as the NIC will
// hand them over.
//
// It exists to measure the receive path with nothing on top of it: take the
// packets, give the buffers straight back, count what happened. Anything more
// would be measuring the application.
//
//	sudo drop -i eth0 -vlan 2053 -queues 2 -cpus 4,6 -duration 20s
//
// By default it takes packets addressed to the interface's own address. With
// -promisc it takes everything the port sees, which is what a benchmark sink
// wants and what you should not point at a machine's only interface: traffic
// the kernel would have handled arrives here instead.
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/examples/internal/affinity"
	"github.com/atoonk/packetio/mlx5"
)

func main() {
	var (
		iface     = flag.String("i", "", "network interface, for example eth0")
		queues    = flag.Int("queues", 1, "how many receive queues to use")
		cpus      = flag.String("cpus", "", "processors to pin the workers to, for example 4,6")
		vlan      = flag.Int("vlan", -1, "take only packets carrying this VLAN id; -1 takes any")
		mac       = flag.String("mac", "", "take packets addressed here instead of to the interface")
		promisc   = flag.Bool("promisc", false, "take every packet the port sees")
		batch     = flag.Int("batch", 64, "packets to take per call")
		perWorker = flag.Int("queues-per-worker", 1, "how many queues one worker drains, round-robin")
		depth     = flag.Int("depth", 1024, "receive queue depth in buffers")

		duration = flag.Duration("duration", 10*time.Second, "how long to run")
		report   = flag.Duration("report", time.Second, "how often to print a line")
		check    = flag.Bool("check", false, "check that every packet carries the expected VLAN and count what does not")
		sample   = flag.Bool("sample", false, "print one packet a second")
		ghz      = flag.Float64("ghz", 0, "processor speed for the cycles-per-packet estimate; 0 reads it from the system")
	)
	flag.Parse()

	if *iface == "" {
		fmt.Fprintln(os.Stderr, "no interface given; try -i eth0")
		flag.Usage()
		os.Exit(2)
	}
	if err := run(cfg{
		iface: *iface, queues: *queues, cpuList: *cpus, vlan: *vlan, mac: *mac,
		promisc: *promisc, batch: *batch, depth: *depth, perWorker: *perWorker,
		duration: *duration, report: *report, check: *check, sample: *sample, ghz: *ghz,
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type cfg struct {
	iface, cpuList, mac string
	queues, vlan        int
	promisc             bool
	batch, depth        int
	perWorker           int
	duration, report    time.Duration
	check, sample       bool
	ghz                 float64
}

func run(c cfg) error {
	if c.batch < 1 {
		return fmt.Errorf("a batch of %d packets", c.batch)
	}
	if c.report <= 0 {
		return fmt.Errorf("a report interval of %s", c.report)
	}
	if c.queues < 1 {
		return fmt.Errorf("at least one queue is needed")
	}
	cpus, err := affinity.Parse(c.cpuList)
	if err != nil {
		return err
	}
	perWorker := c.perWorker
	if perWorker < 1 {
		perWorker = 1
	}
	workers := (c.queues + perWorker - 1) / perWorker
	if len(cpus) > 0 && len(cpus) < workers {
		return fmt.Errorf("%d workers but only %d processors to pin them to", workers, len(cpus))
	}

	opts := []mlx5.Option{
		mlx5.WithTxQueues(0),
		mlx5.WithRxQueues(c.queues),
		mlx5.WithRxDepth(c.depth),
		mlx5.WithFrames(nextPow2(c.queues * c.depth * 2)),
	}
	// One filter says everything the queues should take. Asking for
	// promiscuous together with a match is refused rather than quietly
	// widened to match-everything.
	var f packetio.SteeringFilter
	f.Promiscuous = c.promisc
	if c.vlan >= 0 {
		f.Match = append(f.Match, packetio.MatchVLAN(uint16(c.vlan)))
	}
	if c.mac != "" {
		hw, err := net.ParseMAC(c.mac)
		if err != nil {
			return fmt.Errorf("address to receive on: %w", err)
		}
		if len(hw) != 6 {
			return fmt.Errorf("address to receive on: %s is not an Ethernet address", c.mac)
		}
		var a [6]byte
		copy(a[:], hw)
		f.Match = append(f.Match, packetio.MatchDstMAC(a))
	}
	if err := f.Validate(); err != nil {
		return err
	}
	if f.Promiscuous || len(f.Match) > 0 {
		opts = append(opts, mlx5.WithSteering(f))
	}

	if len(cpus) > 0 {
		opts = append(opts, mlx5.WithAffinity(cpus...))
	}
	dev, err := mlx5.Open(c.iface, opts...)
	if err != nil {
		return err
	}
	defer dev.Close()

	info := dev.Info()
	fmt.Printf("%s: %s port %d, firmware %s, link %s\n",
		info.Interface, info.IBDev, info.Port, info.Firmware, upDown(info.PortActive))
	fmt.Printf("%d queues of %d buffers, batch %d, frames %d x %d\n",
		info.RxQueues, info.RxDepth, c.batch, info.Frames, info.FrameSize)
	fmt.Printf("taking %s\n", info.Steering)
	if len(cpus) > 0 {
		fmt.Printf("%d workers on processors %v, %d queue(s) each\n", workers, cpus[:workers], perWorker)
	} else {
		fmt.Println("workers are not pinned; use -cpus to make the numbers repeatable")
	}
	fmt.Println()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if c.duration > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, c.duration)
		defer stop()
	}

	var (
		wg       sync.WaitGroup
		wrongTag [16]counter // padded so workers do not share a cache line
	)
	// One goroutine can drain several queues, which matters because a queue
	// runs out of packets long before the core draining it runs out of time.
	for first := 0; first < c.queues; first += perWorker {
		last := first + perWorker
		if last > c.queues {
			last = c.queues
		}
		cpu := -1
		if n := first / perWorker; len(cpus) > n {
			cpu = cpus[n]
		}
		wg.Add(1)
		go func(first, last, cpu int) {
			defer wg.Done()
			if err := worker(ctx, dev, first, last, cpu, c, &wrongTag[(first/perWorker)%len(wrongTag)]); err != nil {
				fmt.Fprintf(os.Stderr, "queues %d-%d: %v\n", first, last-1, err)
				cancel()
			}
		}(first, last, cpu)
	}

	err = reportLoop(ctx, dev, c, cpus, &wrongTag)
	wg.Wait()
	return err
}

// counter is padded to a cache line so that two workers updating their own
// counts do not slow each other down.
type counter struct {
	n   uint64
	_   [56]byte
	seq []byte
}

// worker drives one queue: keep it full of buffers, take what arrived, give the
// buffers straight back.
func worker(ctx context.Context, dev *mlx5.Device, first, last, cpu int, c cfg, wrong *counter) error {
	// Placement is the library's: the first queue this goroutine drives puts
	// it on a processor, and the rest of its queues leave it there.
	queues := make([]*mlx5.RxQueue, 0, last-first)
	for i := first; i < last; i++ {
		queues = append(queues, dev.Rx(i))
	}
	region := queues[0].Region()

	// Nothing arrives until the NIC has been given somewhere to put it.
	for _, q := range queues {
		q.Fill(q.NumFreeFillSlots())
	}

	next := 0
	for ctx.Err() == nil {
		q := queues[next]
		if next++; next == len(queues) {
			next = 0
		}

		descs := q.Receive(c.batch)
		if len(descs) == 0 {
			// Nothing waiting. Top the queue up: a receiver that stops filling
			// stops receiving, and the packets it misses are counted by the
			// port, not by it.
			q.Fill(q.NumFreeFillSlots())
			continue
		}

		if c.check && c.vlan >= 0 {
			for _, d := range descs {
				if !hasVLAN(region.Frame(d), c.vlan) {
					wrong.n++
				}
			}
		}
		if c.sample && len(wrong.seq) == 0 && len(descs) > 0 {
			wrong.seq = append(wrong.seq[:0], region.Frame(descs[0])...)
		}

		q.Recycle(descs)
		q.Fill(q.NumFreeFillSlots())
	}
	return nil
}

// hasVLAN reports whether a frame carries the given tag identifier, outermost
// first.
func hasVLAN(frame []byte, id int) bool {
	if len(frame) < 16 {
		return false
	}
	switch binary.BigEndian.Uint16(frame[12:]) {
	case 0x8100, 0x88a8:
		return int(binary.BigEndian.Uint16(frame[14:])&0x0fff) == id
	}
	return false
}

func nextPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

func upDown(b bool) string {
	if b {
		return "up"
	}
	return "down"
}

var _ packetio.RxQueue = (*mlx5.RxQueue)(nil)
