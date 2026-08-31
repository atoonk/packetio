//go:build linux && cgo && mlx5 && (amd64 || arm64)

// Command blast is a packet generator: it transmits as fast as it can, or at a
// rate given, and reports what that cost.
//
// It is deliberately tiny. The point is to measure the packet path, so the
// application on top of it does as little as possible: one frame is built at
// startup and each packet is a copy of it with one field changed. Anything more
// would be measuring the generator.
//
//	sudo blast -i eth0 -dst-mac 02:00:00:00:00:02 -vlan 2053 -size 64 \
//	     -queues 2 -cpus 4,6 -batch 64 -duration 20s
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
	"sync"
	"syscall"
	"time"

	"github.com/atoonk/packetio/examples/internal/affinity"
	"github.com/atoonk/packetio/examples/internal/frame"
	"github.com/atoonk/packetio/mlx5"
)

func main() {
	var (
		iface  = flag.String("i", "", "network interface, for example eth0")
		dstMAC = flag.String("dst-mac", "", "destination Ethernet address (required)")
		srcMAC = flag.String("src-mac", "", "source Ethernet address; defaults to the interface's own")
		vlan   = flag.Int("vlan", 0, "VLAN id to tag with, or 0 for untagged")
		outer  = flag.Int("outer-vlan", 0, "outer VLAN id, for double-tagged frames")
		size   = flag.Int("size", 64, "frame size on the wire in bytes, including the 4-byte frame check sequence the NIC adds; 64 is the Ethernet minimum")

		queues = flag.Int("queues", 1, "how many transmit queues to use")
		cpus   = flag.String("cpus", "", "processors to pin the workers to, for example 4,6 or 8-11")
		batch  = flag.Int("batch", 256, "packets per transmit batch; 256 is what it takes to reach line rate at 68 bytes on four cores, and it costs nothing per core")
		depth  = flag.Int("depth", 1024, "transmit queue depth in packets")

		duration = flag.Duration("duration", 10*time.Second, "how long to run")
		pps      = flag.Float64("pps", 0, "packets per second in total, or 0 for as fast as possible")
		report   = flag.Duration("report", time.Second, "how often to print a line")
		csvPath  = flag.String("csv", "", "write the samples to this file as well")

		srcIP   = flag.String("src-ip", "10.0.0.1", "source IPv4 address")
		dstIP   = flag.String("dst-ip", "10.0.0.2", "destination IPv4 address")
		dstPort = flag.Int("dst-port", 9, "UDP destination port")
		flows   = flag.Int("flows", 1, "how many source ports to cycle through, to spread the receiver's load")

		inline     = flag.Int("inline", -1, "bytes of each packet to inline in its descriptor; -1 for whatever the device requires")
		multi      = flag.String("multi-packet", "auto", "carry several packets per descriptor: auto, on or off")
		multiMax   = flag.Int("multi-packet-max", 192, "longest packet carried that way, NOT counting the frame check sequence; longer ones get a descriptor of their own. 0 means -multi-packet off")
		completion = flag.Int("completion-every", 0, "ask for a completion every N packets; 0 means once per batch")
		hugePages  = flag.Bool("hugepages", false, "ask for huge pages for the frame region")
		ghz        = flag.Float64("ghz", 0, "processor speed, for the cycles-per-packet estimate; 0 reads it from the system")
		prebuilt   = flag.Bool("prebuilt", false, "write the frame into every buffer once at startup and only stamp what varies per packet, instead of rewriting the whole frame each time")
		cpuProfile = flag.String("cpuprofile", "", "write a CPU profile here")
		minBatch   = flag.Int("min-batch", 0, "wait until this many descriptor slots are free before sending, so each doorbell carries more packets")
		perWorker  = flag.Int("queues-per-worker", 1, "how many queues one worker drives, round-robin")
		noAff      = flag.Bool("no-affinity", false, "leave the workers wherever the scheduler puts them, instead of packing them into as few cache complexes as possible")
	)
	flag.Parse()

	if *iface == "" || *dstMAC == "" {
		fmt.Fprintln(os.Stderr, "an interface and a destination address are needed")
		flag.Usage()
		os.Exit(2)
	}

	cfg := config{
		iface: *iface, dstMAC: *dstMAC, srcMAC: *srcMAC,
		vlan: *vlan, outer: *outer, size: *size,
		queues: *queues, cpuList: *cpus, batch: *batch, depth: *depth,
		duration: *duration, pps: *pps, report: *report, csvPath: *csvPath,
		srcIP: *srcIP, dstIP: *dstIP, dstPort: uint16(*dstPort), flows: *flows,
		inline: *inline, completion: *completion, hugePages: *hugePages, ghz: *ghz,
		multi: *multi, multiMax: *multiMax, prebuilt: *prebuilt, cpuProfile: *cpuProfile,
		minBatch: *minBatch, perWorker: *perWorker, noAffinity: *noAff,
	}
	if *noAff && *cpus != "" {
		fmt.Fprintln(os.Stderr, "-no-affinity and -cpus contradict each other: pick one")
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// A wait shorter than minSleep is skipped rather than slept through, because
// the scheduler cannot deliver it accurately; spinTail is how much of a longer
// wait is spun out at the end so the rate does not drift late.
const (
	minSleep = 50 * time.Microsecond
	spinTail = 20 * time.Microsecond
)

type config struct {
	iface, dstMAC, srcMAC string
	vlan, outer, size     int
	queues, batch, depth  int
	cpuList               string
	duration, report      time.Duration
	pps                   float64
	csvPath               string
	srcIP, dstIP          string
	dstPort               uint16
	flows                 int
	inline, completion    int
	multi                 string
	multiMax              int
	prebuilt              bool
	cpuProfile            string
	minBatch              int
	perWorker             int
	noAffinity            bool
	hugePages             bool
	ghz                   float64
}

func run(cfg config) error {
	if cfg.queues < 1 {
		return fmt.Errorf("at least one queue is needed")
	}
	if cfg.batch < 1 {
		return fmt.Errorf("a batch of %d packets", cfg.batch)
	}
	if cfg.report <= 0 {
		return fmt.Errorf("a report interval of %s", cfg.report)
	}

	cpus, err := affinity.Parse(cfg.cpuList)
	if err != nil {
		return err
	}
	perWorker := cfg.perWorker
	if perWorker < 1 {
		perWorker = 1
	}
	workers := (cfg.queues + perWorker - 1) / perWorker
	if len(cpus) > 0 && len(cpus) < workers {
		return fmt.Errorf("%d workers but only %d processors to pin them to", workers, len(cpus))
	}

	// The frame every worker sends. The size on the wire includes the frame
	// check sequence the NIC appends, which is not part of what is handed to
	// it, so a "64-byte packet" is 60 bytes here.
	dst, err := net.ParseMAC(cfg.dstMAC)
	if err != nil {
		return fmt.Errorf("destination address: %w", err)
	}
	src, err := sourceMAC(cfg.srcMAC, cfg.iface)
	if err != nil {
		return err
	}
	srcAddr, err := netip.ParseAddr(cfg.srcIP)
	if err != nil {
		return fmt.Errorf("source address: %w", err)
	}
	dstAddr, err := netip.ParseAddr(cfg.dstIP)
	if err != nil {
		return fmt.Errorf("destination address: %w", err)
	}
	tmpl, err := frame.Build(frame.Spec{
		SrcMAC: src, DstMAC: dst,
		VLAN: cfg.vlan, OuterVLAN: cfg.outer,
		SrcIP: srcAddr, DstIP: dstAddr,
		SrcPort: 1024, DstPort: cfg.dstPort,
		Size: cfg.size - frame.FCS,
	})
	if err != nil {
		return err
	}

	opts := []mlx5.Option{
		mlx5.WithTxQueues(cfg.queues),
		mlx5.WithTxDepth(cfg.depth),
		mlx5.WithEthernetHeaderLen(mlx5.EthernetHeaderLen(tmpl.VLANTags)),
		mlx5.WithCompletionEvery(cfg.completion),
	}
	switch {
	case cfg.noAffinity:
		opts = append(opts, mlx5.WithoutAffinity())
	case len(cpus) > 0:
		opts = append(opts, mlx5.WithAffinity(cpus...))
	}
	switch cfg.multi {
	case "on":
		if cfg.multiMax == 0 {
			// Zero here used to reach the library as "carry packets of up to
			// zero bytes", which it refuses in a message about frame sizes that
			// says nothing about the flag that caused it.
			opts = append(opts, mlx5.WithoutMultiPacket())
			break
		}
		opts = append(opts, mlx5.WithMultiPacket(cfg.multiMax))
	case "off":
		opts = append(opts, mlx5.WithoutMultiPacket())
	case "auto", "":
	default:
		return fmt.Errorf("-multi-packet must be auto, on or off")
	}
	if cfg.inline >= 0 {
		opts = append(opts, mlx5.WithInlineHeader(cfg.inline))
	}
	if cfg.hugePages {
		opts = append(opts, mlx5.WithHugePages())
	}

	dev, err := mlx5.Open(cfg.iface, opts...)
	if err != nil {
		return err
	}
	// The frame is the ceiling, and only the open device knows how big it is.
	// Without this the packet is silently truncated by the copy into it and the
	// startup line still announces the size that was asked for -- a benchmark
	// misreporting what it put on the wire, which is worse than refusing.
	if fs := dev.TxQueue(0).Region().FrameSize(); len(tmpl.Bytes) > fs {
		dev.Close()
		return fmt.Errorf("a %d-byte frame does not fit this device's %d-byte buffers; "+
			"lower -size", len(tmpl.Bytes), fs)
	}
	defer dev.Close()

	info := dev.Info()
	if !info.PortActive {
		return fmt.Errorf("%s: the link is down", cfg.iface)
	}
	fmt.Printf("%s: %s port %d, firmware %s\n", info.Interface, info.IBDev, info.Port, info.Firmware)
	fmt.Printf("%d queues of %d, batch %d, %s, frames %d x %d%s\n",
		info.TxQueues, info.TxDepth, cfg.batch, describeDescriptors(info, cfg.size), info.Frames, info.FrameSize,
		hugeNote(info.HugePages))
	fmt.Printf("%d-byte frames on the wire, %s -> %s%s, %d flows\n",
		cfg.size, src, dst, vlanNote(cfg.vlan, cfg.outer), cfg.flows)
	fmt.Printf("%d workers, %d queue(s) each; placement %s\n", workers, perWorker, info.Placement)
	if cfg.pps > 0 {
		fmt.Printf("rate limited to %s packets per second in total\n", si(cfg.pps))
	}
	fmt.Println()

	if cfg.cpuProfile != "" {
		f, err := os.Create(cfg.cpuProfile)
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
	if cfg.duration > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, cfg.duration)
		defer stop()
	}

	// A queue is filled by one goroutine, but one goroutine can fill several
	// queues. That matters because a queue runs out of room long before the
	// core filling it runs out of time.
	var wg sync.WaitGroup
	for first := 0; first < cfg.queues; first += perWorker {
		last := first + perWorker
		if last > cfg.queues {
			last = cfg.queues
		}
		cpu := -1
		if n := first / perWorker; len(cpus) > n {
			cpu = cpus[n]
		}
		wg.Add(1)
		go func(first, last, cpu int) {
			defer wg.Done()
			if err := worker(ctx, dev, first, last, cpu, cfg, tmpl); err != nil {
				fmt.Fprintf(os.Stderr, "queues %d-%d: %v\n", first, last-1, err)
				cancel()
			}
		}(first, last, cpu)
	}

	err = reportLoop(ctx, dev, cfg, cpus)
	wg.Wait()
	return err
}

// worker drives one queue. Everything it needs is on its own stack or owned by
// its queue, so no two workers touch the same memory.
func worker(ctx context.Context, dev *mlx5.Device, first, last, cpu int, cfg config, tmpl *frame.Frame) error {
	// Placement is the library's: the first queue this goroutine drives puts
	// it on a processor, and the rest of its queues leave it there.
	queues := make([]*mlx5.TxQueue, 0, last-first)
	for i := first; i < last; i++ {
		queues = append(queues, dev.Tx(i))
	}
	q := queues[0]

	// Each worker starts at its own source port so that the flows a receiver
	// sees are spread across its queues rather than piled on one.
	firstPort := uint16(1024 + first*cfg.flows)
	port := firstPort
	body := tmpl.Bytes
	srcPortOff := tmpl.SrcPortOff

	// The rate limiter, if there is one, is per worker: a share of the total.
	// The share is per worker, not per queue, since one worker may drive
	// several queues.
	var (
		workers   = (cfg.queues + queuesPerWorker(cfg) - 1) / queuesPerWorker(cfg)
		perWorker = cfg.pps / float64(workers)
		start     = time.Now()
		sent      uint64
	)

	build := func(i int, b []byte) int {
		n := copy(b, body)
		// One field changed per packet, in place, with no checksum to fix: the
		// UDP checksum is left at zero, which IPv4 allows.
		b[srcPortOff] = byte(port >> 8)
		b[srcPortOff+1] = byte(port)
		if port++; port >= firstPort+uint16(cfg.flows) {
			port = firstPort
		}
		return n
	}
	if cfg.prebuilt {
		// The frame is already in the buffer from the last time it was used,
		// so there is nothing to write but the field that changes. Rewriting
		// the whole frame every time is what a generator does when it has no
		// reason not to; here it is the difference being measured.
		region := dev.TxQueue(0).Region()
		whole := region.Bytes()
		frameSize := region.FrameSize()
		for off := 0; off+len(body) <= len(whole); off += frameSize {
			copy(whole[off:], body)
		}
		length := len(body)
		build = func(i int, b []byte) int {
			b[srcPortOff] = byte(port >> 8)
			b[srcPortOff+1] = byte(port)
			if port++; port >= firstPort+uint16(cfg.flows) {
				port = firstPort
			}
			return length
		}
	}

	next := 0
	for ctx.Err() == nil {
		// Round-robin over this worker's queues: while one is full, the others
		// have room, and the core has time to fill them.
		if len(queues) > 1 {
			q = queues[next]
			if next++; next == len(queues) {
				next = 0
			}
		}

		// Waiting for room before sending makes each doorbell carry more
		// packets. One doorbell per handful of packets is a great many
		// eight-byte writes to a device register.
		if cfg.minBatch > 0 && q.NumFreeSlots() < cfg.minBatch {
			q.Complete(cfg.depth)
			continue
		}
		n, err := q.SendFunc(cfg.batch, build)
		if err != nil {
			return err
		}
		sent += uint64(n)

		if perWorker > 0 {
			// The rate is held against the whole run rather than the last
			// batch, so a batch that ran late is made up by the next ones
			// rather than lost.
			//
			// Waiting is done by sleeping, not by spinning. A worker that
			// spins to pass the time looks fully busy whatever rate it is
			// holding, which would make the cost of a packet unmeasurable at
			// exactly the rates worth measuring. A wait too short to sleep
			// through is not waited for at all: the deficit accumulates over
			// the next few batches into one worth sleeping.
			due := start.Add(time.Duration(float64(sent) / perWorker * float64(time.Second)))
			if d := time.Until(due); d > minSleep {
				time.Sleep(d - spinTail)
				for time.Now().Before(due) {
				}
			}
		}
	}

	// Let the hardware finish with what it has before the device is closed
	// underneath it.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		busy := false
		for _, qq := range queues {
			if qq.Complete(cfg.depth) != 0 || qq.NumInFlight() != 0 {
				busy = true
			}
		}
		if !busy {
			break
		}
	}
	for _, qq := range queues {
		if err := qq.Err(); err != nil {
			return err
		}
	}
	return nil
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

// queuesPerWorker is how many queues one worker drives, at least one.
func queuesPerWorker(cfg config) int {
	if cfg.perWorker < 1 {
		return 1
	}
	return cfg.perWorker
}

func nextPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// describeDescriptors says how packets are being handed to the NIC, which is
// the single biggest thing determining the rate.
func describeDescriptors(i mlx5.Info, wireSize int) string {
	if i.MultiPacketPointer {
		return "several packets per descriptor, pointed at (up to 58 in one)"
	}
	if i.MultiPacket {
		return fmt.Sprintf("several packets per descriptor up to %d bytes copied in (%d of these frames fit)",
			i.MultiPacketMaxLen, packetsPerDescriptor(wireSize))
	}
	if i.InlineHeader > 0 {
		return fmt.Sprintf("one packet per descriptor, %d inline bytes", i.InlineHeader)
	}
	return "one packet per descriptor, pointed at"
}

// packetsPerDescriptor is how many frames of this size share one multi-packet
// descriptor.
//
// This is worth printing because it steps rather than sliding, and the steps
// are large. A packet is copied in behind a four-byte length and the whole
// thing is rounded up to a sixteen-byte octoword, so 60 bytes of frame takes
// four octowords and 64 takes five -- and fourteen frames per descriptor
// becomes eleven. Asking for 68-byte frames on the wire rather than 64 costs
// about a sixth of the transmit rate for that reason alone, which is not
// something anyone would guess from the flag.
func packetsPerDescriptor(wireSize int) int {
	const octoword, usable = 16, 58 // an entry less its control and Ethernet segments
	toNIC := wireSize - frame.FCS   // the NIC appends the check sequence itself
	if toNIC <= 0 {
		return 0
	}
	per := (4 + toNIC + octoword - 1) / octoword
	return usable / per
}

func hugeNote(b bool) string {
	if b {
		return ", huge pages"
	}
	return ""
}

func vlanNote(vlan, outer int) string {
	switch {
	case outer != 0:
		return fmt.Sprintf(", vlan %d inside %d", vlan, outer)
	case vlan != 0:
		return fmt.Sprintf(", vlan %d", vlan)
	}
	return ", untagged"
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
		"elapsed_s", "kind", "packets", "bytes", "pps", "l1_bps", "l2_bps",
		"nic_tx_packets", "nic_tx_errors", "nic_tx_discards",
		"cores", "process_cores", "softirq_s", "cycles_per_packet",
		"batches", "completions", "ring_full", "errors", "in_flight",
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
