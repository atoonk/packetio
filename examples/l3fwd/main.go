//go:build linux && cgo && mlx5 && (amd64 || arm64)

// l3fwd is an IPv4 router on one port: it takes the packets steered to it,
// looks each destination up, rewrites the Ethernet header for the next hop and
// sends the packet back out, in the frame it arrived in.
//
// It exists to measure what forwarding costs on this library, the way a
// native mlx5 path is measured: packets in and out of the same physical port,
// throughput read from the port's transmit counter, processor time counted
// whole. Routes are static and next hops are given by address, because
// resolving them is not what is being measured.
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/examples/internal/affinity"
	"github.com/atoonk/packetio/examples/internal/forward"
	"github.com/atoonk/packetio/mlx5"
)

func main() {
	var (
		iface   = flag.String("i", "", "network interface, for example eth0")
		vlan    = flag.Int("vlan", -1, "take packets on this VLAN only; -1 for any")
		mac     = flag.String("mac", "", "take packets to this address; defaults to the interface's own")
		promisc = flag.Bool("promisc", false, "take every packet the port sees")
		srcMAC  = flag.String("src-mac", "", "source address on forwarded packets; defaults to the interface's own")

		queues      = flag.Int("queues", 1, "receive queues, one worker each")
		txPerWorker = flag.Int("tx-per-worker", 1, "transmit queues per worker; one is enough since the backend rings its own fan-out behind each queue")
		cpus        = flag.String("cpus", "", "processors to pin the workers to, for example 4,6 or 8-11")
		batch       = flag.Int("batch", 64, "packets per batch")
		rxDepth     = flag.Int("rx-depth", 1024, "receive queue depth in packets")
		txDepth     = flag.Int("tx-depth", 1024, "transmit queue depth in packets")

		multi      = flag.String("multi-packet", "auto", "carry several packets per descriptor: auto, on or off")
		multiMax   = flag.Int("multi-packet-max", 192, "longest packet carried that way; longer ones get a descriptor of their own")
		inline     = flag.Int("inline", -1, "bytes of each packet to inline in its descriptor; -1 for whatever the device requires")
		completion = flag.Int("completion-every", 0, "ask for a completion every N packets; 0 means once per batch")
		hugePages  = flag.Bool("hugepages", false, "ask for huge pages for the frame region")

		duration   = flag.Duration("duration", 10*time.Second, "how long to run; 0 for until interrupted")
		report     = flag.Duration("report", time.Second, "how often to print a line")
		csvPath    = flag.String("csv", "", "write the samples to this file as well")
		ghz        = flag.Float64("ghz", 0, "processor speed, for the cycles-per-packet estimate; 0 reads it from the system")
		cpuProfile = flag.String("cpuprofile", "", "write a CPU profile here")
		sampleFlag = flag.Bool("sample", false, "print the first packet seen, before and after the rewrite")
	)
	var routes routeFlags
	flag.Var(&routes, "route", "prefix,next-hop-mac[,vlan]: send packets for prefix to that address, tagged with vlan (0 or absent for untagged); repeatable")
	flag.Parse()

	if *iface == "" || len(routes) == 0 {
		fmt.Fprintln(os.Stderr, "an interface and at least one -route are needed")
		flag.Usage()
		os.Exit(2)
	}

	c := config{
		iface: *iface, vlan: *vlan, mac: *mac, promisc: *promisc, srcMAC: *srcMAC,
		routes: routes,
		queues: *queues, txPerWorker: *txPerWorker, cpuList: *cpus, batch: *batch,
		rxDepth: *rxDepth, txDepth: *txDepth,
		multi: *multi, multiMax: *multiMax, inline: *inline, completion: *completion, hugePages: *hugePages,
		duration: *duration, report: *report, csvPath: *csvPath, ghz: *ghz, cpuProfile: *cpuProfile,
		sample: *sampleFlag,
	}
	if err := run(c); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type config struct {
	iface, mac, srcMAC string
	vlan               int
	promisc            bool
	routes             []route

	queues, txPerWorker int
	cpuList             string
	batch               int
	rxDepth, txDepth    int

	multi            string
	multiMax, inline int
	completion       int
	hugePages        bool
	duration, report time.Duration
	csvPath          string
	ghz              float64
	cpuProfile       string
	sample           bool
}

// route is one -route flag: where to send what.
type route struct {
	prefix  netip.Prefix
	nextHop net.HardwareAddr
	vlan    int
}

type routeFlags []route

func (r *routeFlags) String() string { return fmt.Sprint(len(*r), " routes") }

func (r *routeFlags) Set(s string) error {
	parts := strings.Split(s, ",")
	if len(parts) < 2 || len(parts) > 3 {
		return fmt.Errorf("want prefix,next-hop-mac[,vlan], got %q", s)
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
		return fmt.Errorf("next hop of %s: %w", parts[0], err)
	}
	rt := route{prefix: p.Masked(), nextHop: hw}
	if len(parts) == 3 {
		v, err := strconv.Atoi(parts[2])
		if err != nil || v < 0 || v > 4094 {
			return fmt.Errorf("vlan of %s: %q is not a VLAN id", parts[0], parts[2])
		}
		rt.vlan = v
	}
	*r = append(*r, rt)
	return nil
}

func run(c config) error {
	if c.queues < 1 || c.txPerWorker < 1 {
		return fmt.Errorf("at least one receive queue and one transmit queue per worker are needed")
	}
	if c.batch < 1 {
		return fmt.Errorf("a batch of %d packets", c.batch)
	}
	if c.report <= 0 {
		return fmt.Errorf("a report interval of %s", c.report)
	}
	cpus, err := affinity.Parse(c.cpuList)
	if err != nil {
		return err
	}
	if len(cpus) > 0 && len(cpus) < c.queues {
		return fmt.Errorf("%d workers but only %d processors to pin them to", c.queues, len(cpus))
	}

	src, err := sourceMAC(c.srcMAC, c.iface)
	if err != nil {
		return err
	}
	fw, tags, err := buildForwarder(c.routes, src)
	if err != nil {
		return err
	}

	// Frames are split evenly over every queue's pool, but a forwarder only
	// ever takes frames from the receive pools: a received frame goes out and
	// comes back to the receive queue, and the transmit pools sit unused. So
	// each receive pool must hold what its queue posts, what its transmit
	// queues can have in flight, and a batch or two in hand, and the whole is
	// that times the number of pools.
	pools := c.queues * (1 + c.txPerWorker)
	perPool := c.rxDepth + c.txPerWorker*c.txDepth + 2*c.batch
	frames := nextPow2(pools * perPool)

	opts := []mlx5.Option{
		mlx5.WithRxQueues(c.queues),
		mlx5.WithRxDepth(c.rxDepth),
		mlx5.WithTxQueues(c.queues * c.txPerWorker),
		mlx5.WithTxDepth(c.txDepth),
		mlx5.WithFrames(frames),
		mlx5.WithEthernetHeaderLen(mlx5.EthernetHeaderLen(tags)),
		mlx5.WithCompletionEvery(c.completion),
	}
	switch c.multi {
	case "on":
		opts = append(opts, mlx5.WithMultiPacket(c.multiMax))
	case "off":
		opts = append(opts, mlx5.WithoutMultiPacket())
	case "auto", "":
	default:
		return fmt.Errorf("-multi-packet must be auto, on or off")
	}
	if c.inline >= 0 {
		opts = append(opts, mlx5.WithInlineHeader(c.inline))
	}
	if c.hugePages {
		opts = append(opts, mlx5.WithHugePages())
	}
	var f packetio.SteeringFilter
	switch {
	case c.promisc:
		f.Promiscuous = true
	case c.mac != "":
		hw, err := net.ParseMAC(c.mac)
		if err != nil {
			return fmt.Errorf("-mac: %w", err)
		}
		if len(hw) != 6 {
			return fmt.Errorf("-mac: %s is not an Ethernet address", c.mac)
		}
		var m [6]byte
		copy(m[:], hw)
		f.Match = append(f.Match, packetio.MatchDstMAC(m))
	}
	if c.vlan >= 0 {
		f.Match = append(f.Match, packetio.MatchVLAN(uint16(c.vlan)))
	}
	if err := f.Validate(); err != nil {
		return err
	}
	if f.Promiscuous || len(f.Match) > 0 {
		opts = append(opts, mlx5.WithSteering(f))
	}

	dev, err := mlx5.Open(c.iface, opts...)
	if err != nil {
		return err
	}
	defer dev.Close()

	info := dev.Info()
	if !info.PortActive {
		return fmt.Errorf("%s: the link is down", c.iface)
	}
	fmt.Printf("%s: %s port %d, firmware %s\n", info.Interface, info.IBDev, info.Port, info.Firmware)
	fmt.Printf("taking %s\n", info.Steering)
	fmt.Printf("%d workers, each with a receive queue of %d and %d transmit queue(s) of %d; batch %d; %s\n",
		c.queues, info.RxDepth, c.txPerWorker, info.TxDepth, c.batch, describeDescriptors(info))
	fmt.Printf("%d frames of %d bytes, %d per pool%s\n", info.Frames, info.FrameSize, info.Frames/pools, hugeNote(info.HugePages))
	for i, r := range c.routes {
		fmt.Printf("route %-20s -> %s%s (adjacency %d)\n", r.prefix, r.nextHop, vlanNote(r.vlan), i+1)
	}
	fmt.Printf("forwarded packets are sent from %s\n", src)
	if len(cpus) > 0 {
		fmt.Printf("workers on processors %v (memory node %d; the card is on node %d)\n",
			cpus[:c.queues], affinity.NUMANodeOf(cpus[0]), affinity.NUMANodeOfInterface(c.iface))
	} else {
		fmt.Println("workers are not pinned; use -cpus to make the numbers repeatable")
	}
	fmt.Println()

	if c.cpuProfile != "" {
		f, err := os.Create(c.cpuProfile)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			return err
		}
		defer pprof.StopCPUProfile()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if c.duration > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, c.duration)
		defer stop()
	}

	counts := make([]counters, c.queues)
	var smp *sample
	if c.sample {
		smp = &sample{}
	}
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		workerErr error
	)
	for i := 0; i < c.queues; i++ {
		cpu := -1
		if len(cpus) > i {
			cpu = cpus[i]
		}
		var s *sample
		if i == 0 {
			s = smp
		}
		wg.Add(1)
		go func(i, cpu int, s *sample) {
			defer wg.Done()
			if err := worker(ctx, dev, i, cpu, c, fw, &counts[i], s); err != nil {
				fmt.Fprintf(os.Stderr, "worker %d: %v\n", i, err)
				mu.Lock()
				if workerErr == nil {
					workerErr = err
				}
				mu.Unlock()
				cancel()
			}
		}(i, cpu, s)
	}

	err = reportLoop(ctx, dev, c, cpus, counts)
	wg.Wait()
	if smp != nil {
		fmt.Printf("\nfirst packet in:  % x\nfirst packet out: % x\n", smp.before, smp.after)
	}
	if err == nil {
		err = workerErr
	}
	return err
}

// buildForwarder turns the routes into a table and its adjacencies, and says
// how many VLAN tags the longest egress header carries.
func buildForwarder(routes []route, src net.HardwareAddr) (*forwarder, int, error) {
	fw := &forwarder{fib: forward.NewV4Fib(), adjs: make([]forward.Adjacency, 1, len(routes)+1)}
	var s [6]byte
	copy(s[:], src)
	tags := 0
	for _, r := range routes {
		var d [6]byte
		copy(d[:], r.nextHop)
		fw.adjs = append(fw.adjs, forward.NewAdjacency(d, s, uint16(r.vlan)))
		a := r.prefix.Addr().As4()
		prefix := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
		fw.fib.Insert(prefix, uint8(r.prefix.Bits()), uint32(len(fw.adjs)-1))
		if r.vlan != 0 {
			tags = 1
		}
	}
	return fw, tags, nil
}

func sourceMAC(want, iface string) (net.HardwareAddr, error) {
	if want != "" {
		mac, err := net.ParseMAC(want)
		if err != nil {
			return nil, fmt.Errorf("source address: %w", err)
		}
		return mac, nil
	}
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("looking up %s: %w", iface, err)
	}
	if len(ifi.HardwareAddr) != 6 {
		return nil, fmt.Errorf("%s has no Ethernet address; give one with -src-mac", iface)
	}
	return ifi.HardwareAddr, nil
}

func nextPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

func describeDescriptors(i mlx5.Info) string {
	if i.MultiPacket {
		return fmt.Sprintf("several packets per descriptor up to %d bytes", i.MultiPacketMaxLen)
	}
	if i.InlineHeader > 0 {
		return fmt.Sprintf("one packet per descriptor, %d inline bytes", i.InlineHeader)
	}
	return "one packet per descriptor, pointed at"
}

func hugeNote(b bool) string {
	if b {
		return ", huge pages"
	}
	return ""
}

func vlanNote(vlan int) string {
	if vlan != 0 {
		return fmt.Sprintf(" on vlan %d", vlan)
	}
	return " untagged"
}

func si(v float64) string {
	switch {
	case v >= 1e9:
		return strconv.FormatFloat(v/1e9, 'f', 2, 64) + "G"
	case v >= 1e6:
		return strconv.FormatFloat(v/1e6, 'f', 2, 64) + "M"
	case v >= 1e3:
		return strconv.FormatFloat(v/1e3, 'f', 2, 64) + "k"
	}
	return strconv.FormatFloat(v, 'f', 0, 64)
}

// csvWriter writes one row per sample, for a benchmark that will be read by
// something other than a person.
type csvWriter struct {
	f *os.File
	w *csv.Writer
}

func newCSV(path string) (*csvWriter, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	c := &csvWriter{f: f, w: csv.NewWriter(f)}
	_ = c.w.Write([]string{
		"elapsed_s", "kind", "received", "forwarded", "dropped", "tx_full",
		"nic_rx_packets", "nic_tx_packets", "nic_rx_out_of_buffer", "nic_rx_discards", "nic_tx_errors",
		"cores", "process_cores", "cycles_per_packet",
	})
	return c, nil
}

func (c *csvWriter) write(rec []string) {
	if c == nil {
		return
	}
	_ = c.w.Write(rec)
}

func (c *csvWriter) close() {
	if c == nil {
		return
	}
	c.w.Flush()
	_ = c.f.Close()
}
