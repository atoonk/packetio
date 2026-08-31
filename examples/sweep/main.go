//go:build linux

// sweep drives every backend through the same three loops -- transmit,
// receive, forward -- so a comparison between them measures the backend and
// not four different programs.
//
//	sweep -io mlx5     -i eno2         -mode tx  -queues 4 -cpus 0-3
//	sweep -io dpdk     -i 0000:c1:00.1 -mode rx  -queues 8 -cpus 0-7
//	sweep -io afxdp    -i eno2         -mode fwd -queues 2 -cpus 0-1
//	sweep -io afpacket -i eno2         -mode tx  -queues 1
//
// It prints one RESULT line with what the application counted. The
// authoritative rate for a benchmark is the port's own counters, read by the
// harness around the measurement window; this count is the cross-check.
//
// mlx5 and dpdk need their build tags; without them those two backends are
// simply not registered:
//
//	go build -tags "mlx5 dpdk" ./examples/sweep
package main

import (
	"flag"
	"fmt"
	"math/bits"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"unsafe"

	xdp "github.com/atoonk/go-afxdp"
	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/afpacket"
	"github.com/atoonk/packetio/afxdp"
	"github.com/atoonk/packetio/examples/internal/affinity"
	"github.com/atoonk/packetio/examples/internal/forward"
	"github.com/atoonk/packetio/examples/internal/prefetch"
)

// opener builds a device for one backend. Backends behind a build tag
// register themselves from their own file, so this program compiles with
// whatever is available.
type opener func(cfg config) (packetio.Device, error)

var openers = map[string]opener{
	"afxdp":    openAFXDP,
	"afpacket": openAFPacket,
}

type config struct {
	io          string
	dev         string
	mode        string
	queues      int
	depth       int
	batch       int
	size        int
	vlan        int
	dstMAC      string
	srcMAC      string
	flows       int
	multiPacket bool
	rings       int
	perWorker   int
	prefetchOn  bool
	compEvery   int
	xdpStats    bool
	noTune      bool
	noMPW       bool
	devargs     string
	busyPoll    bool
	rxPoll      bool
	rxHeavy     bool
	flushUs     int
	cpus        []int
	warm        time.Duration
	window      time.Duration
}

func main() {
	var (
		io      = flag.String("io", "mlx5", "backend: mlx5, dpdk, afxdp or afpacket")
		dev     = flag.String("i", "", "interface name, or PCI address for dpdk")
		mode    = flag.String("mode", "tx", "tx, rx or fwd")
		queues  = flag.Int("queues", 1, "queues, one worker each")
		cpus    = flag.String("cpus", "", "processors to pin workers to, e.g. 0-3")
		batch   = flag.Int("batch", 64, "packets per call")
		depth   = flag.Int("depth", 1024, "queue depth")
		size    = flag.Int("size", 64, "frame size on the wire, including the 4-byte FCS")
		vlan    = flag.Int("vlan", 0, "vlan id to tag with")
		dstMAC  = flag.String("dst-mac", "", "destination mac (tx, fwd next hop)")
		srcMAC  = flag.String("src-mac", "", "source mac; defaults to the interface's")
		flows   = flag.Int("flows", 1, "source ports to cycle through")
		mpw     = flag.Bool("multi-packet", false, "mlx5: copy packets into descriptors instead of pointing at them")
		pf      = flag.Bool("prefetch", false, "fwd: hint each frame's first line into the cache before parsing the batch")
		compEv  = flag.Int("completion-every", 0, "mlx5: ask for a completion every N packets (0 = once per batch)")
		nrings  = flag.Int("rings", 0, "mlx5: hardware send queues behind each TxQueue; 0 lets the backend choose")
		qpw     = flag.Int("queues-per-worker", 1, "queues one worker drives, round-robin")
		warm    = flag.Duration("warm", 6*time.Second, "settle before measuring")
		window  = flag.Duration("window", 10*time.Second, "measurement window")
		xstats  = flag.Bool("xdp-stats", false, "afxdp: dump per-socket driver statistics on exit")
		notune  = flag.Bool("no-autotune", false, "afxdp: leave the driver's NAPI settings alone")
		nompw   = flag.Bool("no-mpwqe", false, "afxdp: leave the mlx5 xdp_tx_mpwqe flag alone")
		devargs = flag.String("devargs", "", "dpdk: extra device arguments for the PMD, e.g. txqs_min_inline=1")
		bpoll   = flag.Bool("busy-poll", false, "afxdp: XSK preferred busy polling, 10us budget 64")
		rxpoll  = flag.Bool("poll", false, "receive: block in Poll when the ring is empty instead of spinning")
		rxheavy = flag.Bool("receive-heavy", false, "afxdp: hand the transmit half of the frame pool to receive")
		flushus = flag.Int("napi-flush-us", 0, "afxdp: gro_flush_timeout in microseconds (0 = the library default of 20)")
	)
	flag.Parse()

	if *dev == "" {
		fmt.Fprintln(os.Stderr, "need -i")
		os.Exit(2)
	}
	list, err := parseCPUs(*cpus)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if len(list) > 0 && len(list) < *queues {
		fmt.Fprintf(os.Stderr, "%d queues but only %d processors\n", *queues, len(list))
		os.Exit(2)
	}
	c := config{io: *io, dev: *dev, mode: *mode, queues: *queues, depth: *depth,
		batch: *batch, size: *size, vlan: *vlan, dstMAC: *dstMAC, srcMAC: *srcMAC,
		flows: *flows, cpus: list, warm: *warm, window: *window, multiPacket: *mpw, rings: *nrings, perWorker: *qpw, prefetchOn: *pf, compEvery: *compEv, xdpStats: *xstats, noTune: *notune, noMPW: *nompw, devargs: *devargs, busyPoll: *bpoll, rxPoll: *rxpoll, rxHeavy: *rxheavy, flushUs: *flushus}

	open, ok := openers[c.io]
	if !ok {
		fmt.Fprintf(os.Stderr, "backend %q is not built in; rebuild with -tags %q\n", c.io, c.io)
		os.Exit(2)
	}
	d, err := open(c)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", c.io, err)
		os.Exit(1)
	}
	// Close on the way out however the exit happens. The harness kills this
	// program mid-window with SIGINT, and a signal skips deferred calls --
	// which leaks whatever the backend tuned on the host (NAPI settings, the
	// mlx5 xdp_tx_mpwqe flag) into every arm that follows. The workers must
	// stop BEFORE the device closes: DPDK wedges if the port is torn down
	// under goroutines still driving its queues. stopAll is filled in once
	// the worker plumbing below exists.
	var closeOnce sync.Once
	closeDev := func() { closeOnce.Do(func() { d.Close() }) }
	defer closeDev()
	var stopAll atomic.Pointer[func()]
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		if f := stopAll.Load(); f != nil {
			(*f)()
		}
		closeDev()
		os.Exit(0)
	}()

	caps := d.Capabilities()
	fmt.Fprintf(os.Stderr, "%s: %d tx, %d rx queues, frame %d, hands back frames %v, kernel keeps nic %v\n",
		caps.Backend, d.NumTxQueues(), d.NumRxQueues(), caps.MaxFrameSize,
		caps.HandsBackFrames, caps.KernelCoexistence)

	workers := c.queues
	n := c.queues
	switch c.mode {
	case "tx":
		n = min(n, d.NumTxQueues())
	case "rx":
		n = min(n, d.NumRxQueues())
	case "fwd":
		n = min(n, min(d.NumTxQueues(), d.NumRxQueues()))
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", c.mode)
		os.Exit(2)
	}
	if n == 0 {
		fmt.Fprintln(os.Stderr, "no queues for this mode")
		os.Exit(1)
	}

	if c.perWorker > 1 {
		// n counted queues; the workers are what cost cores.
		workers = n / c.perWorker
		if workers < 1 {
			workers = 1
		}
	} else {
		workers = n
	}

	pkt, err := buildFrame(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fw, err := buildForwarder(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var packets atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var stopOnce sync.Once
	stopFn := func() { stopOnce.Do(func() { close(stop) }); wg.Wait() }
	stopAll.Store(&stopFn)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if len(c.cpus) > 0 {
				affinity.Pin(c.cpus[idx%len(c.cpus)])
			}
			switch c.mode {
			case "tx":
				txLoop(d, idx, c, pkt, &packets, stop)
			case "rx":
				rxLoop(d, idx, c, &packets, stop)
			case "fwd":
				fwdLoop(d, idx, c, fw, &packets, stop)
			}
		}(i)
	}

	time.Sleep(c.warm)
	start := packets.Load()
	t0 := time.Now()
	time.Sleep(c.window)
	el := time.Since(t0)
	got := packets.Load() - start
	stopFn()

	if c.xdpStats {
		dumpXDPStats(d, n, c.mode)
	}
	mpps := float64(got) / el.Seconds() / 1e6
	// L1 rate: the frame plus preamble, start-of-frame delimiter and the gap.
	gbps := float64(got) * float64(c.size+20) * 8 / el.Seconds() / 1e9
	fmt.Printf("RESULT io=%s mode=%s queues=%d workers=%d packets=%d mpps=%.2f gbps=%.2f\n",
		c.io, c.mode, n, workers, got, mpps, gbps)
	if c.mode == "fwd" {
		// Whole-run totals, not window totals: what matters is the ratios --
		// where the packets that were taken but not forwarded actually died.
		b, t := diag.batches.Load(), diag.taken.Load()
		var avg float64
		if b > 0 {
			avg = float64(t) / float64(b)
		}
		fmt.Fprintf(os.Stderr, "DIAG taken=%d sent=%d parse_drop=%d tx_full=%d batches=%d avg_batch=%.1f\n",
			t, packets.Load(), diag.parsedrop.Load(), diag.txfull.Load(), b, avg)
	}
}

// dumpXDPStats prints go-afxdp's own per-socket counters, which say where
// packets die: the fill ring found empty (pool starvation), the rx ring full
// (slow reader), rx_dropped (XDP), or none of those while the NIC's
// rx_out_of_buffer climbs -- the parked-driver signature WakeupRx exists for.
func dumpXDPStats(d packetio.Device, n int, mode string) {
	for i := 0; i < n; i++ {
		var q interface{} = d.RxQueue(i)
		if mode == "tx" {
			q = d.TxQueue(i)
		}
		sk, ok := q.(interface{ Socket() *xdp.Socket })
		if !ok {
			return
		}
		sock := sk.Socket()
		st, err := sock.Stats()
		if err != nil {
			fmt.Fprintf(os.Stderr, "xdp-stats q%d: %v\n", i, err)
			continue
		}
		fmt.Fprintf(os.Stderr,
			"xdp-stats q%d: received=%d filled=%d polls=%d rx_kicks=%d num_filled=%d free_rx_frames=%d "+
				"ring_full=%d fill_ring_empty=%d rx_dropped=%d\n",
			i, st.Received, st.Filled, st.Polls, st.RxKicks,
			sock.NumFilled(), sock.FreeRxFrames(),
			st.KernelStats.Rx_ring_full, st.KernelStats.Rx_fill_ring_empty_descs,
			st.KernelStats.Rx_dropped)
	}
}

// ------------------------------------------------------------------ loops

func txLoop(d packetio.Device, idx int, c config, pkt []byte, packets *atomic.Uint64, stop <-chan struct{}) {
	// One worker may drive several queues: a single send queue tops out well
	// below what one core can produce, so reaching the card's limit from few
	// cores means giving each worker more than one.
	qs := make([]packetio.TxQueue, 0, c.perWorker)
	for k := 0; k < c.perWorker; k++ {
		if q := d.TxQueue(idx*c.perWorker + k); q != nil {
			qs = append(qs, q)
		}
	}
	if len(qs) == 0 {
		return
	}
	next := 0
	port := uint16(1024)
	build := func(_ int, b []byte) int {
		n := copy(b, pkt)
		if c.flows > 1 {
			// Vary the source port so the receiver's hash spreads the load.
			off := 34
			if c.vlan != 0 {
				off = 38
			}
			b[off] = byte(port >> 8)
			b[off+1] = byte(port)
			if port++; port >= 1024+uint16(c.flows) {
				port = 1024
			}
		}
		return n
	}
	for {
		select {
		case <-stop:
			return
		default:
		}
		q := qs[next]
		if next++; next == len(qs) {
			next = 0
		}
		sent, err := q.SendFunc(c.batch, build)
		if err != nil {
			fmt.Fprintf(os.Stderr, "queue %d: %v\n", idx, err)
			return
		}
		packets.Add(uint64(sent))
		q.Complete(c.batch)
	}
}

func rxLoop(d packetio.Device, idx int, c config, packets *atomic.Uint64, stop <-chan struct{}) {
	q := d.RxQueue(idx)
	// Block when the backend can really sleep, spin when it cannot.
	// Capabilities.BlockingPoll is the backend's own answer: AF_XDP's Poll
	// waits on the socket, mlx5's and DPDK's can only spin, and spinning
	// through a call that cannot sleep is worse than not calling it. On
	// AF_XDP this is worth about two of thirteen cores at line rate, because
	// a blocked worker lets the driver's NAPI batch instead of finding the
	// ring drained on every poll.
	blocking := d.Capabilities().BlockingPoll || c.rxPoll
	q.Fill(q.NumFreeFillSlots())
	for {
		select {
		case <-stop:
			return
		default:
		}
		descs := q.Receive(c.batch)
		if len(descs) == 0 {
			q.Fill(q.NumFreeFillSlots())
			if blocking {
				// Block rather than spin. Only AF_XDP can actually sleep
				// here (Capabilities.BlockingPoll); on the polling backends
				// this is a spin with extra steps, which is why it is off by
				// default and the cross-backend ladder does not use it.
				q.Poll(100 * time.Millisecond)
			}
			continue
		}
		packets.Add(uint64(len(descs)))
		q.Recycle(descs)
		q.Fill(q.NumFreeFillSlots())
	}
}

// diag says where forwarding lost what it lost: packets that failed to parse,
// and packets that parsed but found the transmit ring full.
var diag struct {
	batches, taken, parsedrop, txfull atomic.Uint64
}

// fwdLoop is the l3fwd cycle: take what arrived, find the IPv4 header, look
// the destination up, rewrite the packet in the frame it arrived in, and send
// that same frame back out. Nothing is copied and nothing is allocated.
//
// How the frames get home differs by backend, and Capabilities says which:
// where Reclaim can name the frames it reclaimed they go back to the receive
// pool by hand, and where it cannot (AF_XDP) the socket is opened so that a
// completed frame returns to the pool its address belongs to.
func fwdLoop(d packetio.Device, idx int, c config, fw *forwarder, packets *atomic.Uint64, stop <-chan struct{}) {
	rx, tx := d.RxQueue(idx), d.TxQueue(idx)
	handsBack := d.Capabilities().HandsBackFrames

	region := rx.Region()
	mem := region.Bytes()
	frameShift := uint(bits.TrailingZeros(uint(region.FrameSize())))

	out := make([]packetio.Desc, 0, c.batch)
	drop := make([]packetio.Desc, 0, c.batch)
	back := make([]packetio.Desc, 0, c.depth)

	rx.Fill(rx.NumFreeFillSlots())
	for {
		select {
		case <-stop:
			return
		default:
		}
		descs := rx.Receive(c.batch)
		if len(descs) == 0 {
			back = reclaim(tx, rx, back, handsBack, c.batch)
			rx.Fill(rx.NumFreeFillSlots())
			continue
		}
		diag.batches.Add(1)
		diag.taken.Add(uint64(len(descs)))
		if c.prefetchOn {
			// The frames were written by the NIC's DMA and are in no cache.
			// Hint a few ahead now, then keep an 8-packet lead inside the
			// loop below, so each miss overlaps the parses in front of it
			// instead of stalling Parse for the full memory latency.
			for i := 0; i < 8 && i < len(descs); i++ {
				prefetch.T0(unsafe.Pointer(&mem[descs[i].Addr]))
			}
		}
		out, drop = out[:0], drop[:0]
		for di, desc := range descs {
			if c.prefetchOn && di+8 < len(descs) {
				prefetch.T0(unsafe.Pointer(&mem[descs[di+8].Addr]))
			}
			end := (desc.Addr>>frameShift + 1) << frameShift
			if end > uint64(len(mem)) {
				end = uint64(len(mem))
			}
			buf := mem[desc.Addr:end:end]
			l3, dst, code := forward.Parse(buf[:desc.Len], desc.Options&packetio.OptL3ChecksumOK != 0)
			if code != forward.ErrNone {
				drop = append(drop, desc)
				continue
			}
			adj := fw.fib.Lookup(dst)
			if adj == 0 {
				drop = append(drop, desc)
				continue
			}
			n := forward.Rewrite(buf, l3, &fw.adjs[adj])
			if n == 0 {
				// The egress header no longer fits the frame; a zero-length
				// descriptor is a hardware error on mlx5, so drop instead.
				drop = append(drop, desc)
				continue
			}
			desc.Len = uint32(n)
			out = append(out, desc)
		}
		diag.parsedrop.Add(uint64(len(descs) - len(out)))
		sent := 0
		if len(out) > 0 {
			sent = tx.Transmit(out)
			if sent < len(out) {
				diag.txfull.Add(uint64(len(out) - sent))
				drop = append(drop, out[sent:]...)
			}
		}
		rx.Recycle(drop)
		back = reclaim(tx, rx, back, handsBack, c.batch)
		rx.Fill(rx.NumFreeFillSlots())
		packets.Add(uint64(sent))
	}
}

func reclaim(tx packetio.TxQueue, rx packetio.RxQueue, back []packetio.Desc, handsBack bool, max int) []packetio.Desc {
	if !handsBack {
		tx.Complete(max)
		return back
	}
	back = tx.Reclaim(max, back[:0])
	if len(back) > 0 {
		rx.Recycle(back)
	}
	return back
}

// ------------------------------------------------------------ the packet

type forwarder struct {
	fib  *forward.V4Fib
	adjs []forward.Adjacency
}

// buildForwarder installs one default route back to the peer, which is all a
// forwarding benchmark needs: every packet matches, and the lookup cost is the
// real one.
func buildForwarder(c config) (*forwarder, error) {
	if c.mode != "fwd" {
		return nil, nil
	}
	src, err := macOf(c.srcMAC, c.dev)
	if err != nil {
		return nil, err
	}
	if c.dstMAC == "" {
		return nil, fmt.Errorf("fwd needs -dst-mac, the next hop")
	}
	dst, err := net.ParseMAC(c.dstMAC)
	if err != nil {
		return nil, err
	}
	var s, dd [6]byte
	copy(s[:], src)
	copy(dd[:], dst)
	fw := &forwarder{fib: forward.NewV4Fib(), adjs: make([]forward.Adjacency, 1, 2)}
	fw.adjs = append(fw.adjs, forward.NewAdjacency(dd, s, uint16(c.vlan)))
	fw.fib.Insert(0, 0, 1) // 0.0.0.0/0
	return fw, nil
}

// buildFrame makes one Ethernet + optional 802.1Q + IPv4 + UDP packet.
func buildFrame(c config) ([]byte, error) {
	if c.mode != "tx" {
		return nil, nil
	}
	if c.dstMAC == "" {
		return nil, fmt.Errorf("tx needs -dst-mac")
	}
	dst, err := net.ParseMAC(c.dstMAC)
	if err != nil {
		return nil, err
	}
	src, err := macOf(c.srcMAC, c.dev)
	if err != nil {
		return nil, err
	}

	n := c.size - 4 // the NIC appends the frame check sequence
	p := make([]byte, n)
	copy(p[0:6], dst)
	copy(p[6:12], src)
	off := 12
	if c.vlan != 0 {
		be16(p[off:], 0x8100)
		be16(p[off+2:], uint16(c.vlan))
		off += 4
	}
	be16(p[off:], 0x0800)
	ip := p[off+2 : off+22]
	ip[0], ip[8], ip[9] = 0x45, 64, 17
	be16(ip[2:], uint16(n-(off+2)))
	copy(ip[12:16], []byte{10, 0, 0, 1})
	copy(ip[16:20], []byte{10, 0, 0, 2})
	be16(ip[10:], checksum(ip))
	udp := p[off+22 : off+30]
	be16(udp[0:], 1024)
	be16(udp[2:], 9000)
	be16(udp[4:], uint16(n-(off+22)))
	return p, nil
}

func be16(b []byte, v uint16) { b[0], b[1] = byte(v>>8), byte(v) }

func checksum(h []byte) uint16 {
	var s uint32
	for i := 0; i+1 < len(h); i += 2 {
		s += uint32(h[i])<<8 | uint32(h[i+1])
	}
	for s > 0xffff {
		s = s>>16 + s&0xffff
	}
	return ^uint16(s)
}

func macOf(want, iface string) (net.HardwareAddr, error) {
	if want != "" {
		return net.ParseMAC(want)
	}
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("no -src-mac and %s: %w", iface, err)
	}
	return ifi.HardwareAddr, nil
}

// --------------------------------------------------------------- backends

func openAFXDP(c config) (packetio.Device, error) {
	// The filter is required: without one every packet on the interface would
	// be redirected away from the kernel. Transmit takes nothing; receive and
	// forward take everything on the port, which is what the DPDK arm gets by
	// owning the device outright, so the arms are comparable. Safe here
	// because management is on another interface.
	filter := xdp.MatchNone()
	if c.mode != "tx" {
		filter = xdp.MatchAll()
	}
	opts := []xdp.Option{
		xdp.WithQueues(c.queues),
		// Each socket has its own UMEM, so these are per queue. The fill ring
		// has to be backed by frames that are not reserved for transmit, which
		// is what a too-small NumFrames fails on at bind.
		xdp.WithRingSize(c.depth),
		xdp.WithNumFrames(nextPow2(c.depth * 8)),
		xdp.WithFilter(filter),
		// Need-wakeup is the backend's own default now, so nothing is set
		// here: this measures what a caller gets.
	}
	if c.noMPW {
		opts = append(opts, xdp.WithoutMPWQETune())
	}
	if c.noTune {
		opts = append(opts, xdp.WithoutAutoTune())
	}
	if c.busyPoll {
		opts = append(opts, xdp.WithBusyPoll(10, 256))
	}
	if c.rxHeavy {
		// A receive-only socket never uses the transmit half of its frame
		// pool; handing it to receive gives the fill ring slack it otherwise
		// has none of (the default pool is exactly the fill ring's depth).
		opts = append(opts, xdp.WithReceiveHeavy())
	}
	if c.flushUs > 0 {
		opts = append(opts, xdp.WithNAPITuning(2, time.Duration(c.flushUs)*time.Microsecond))
	}
	if c.mode == "fwd" {
		// AF_XDP cannot name the frames it reclaimed, so a forwarded frame
		// has to find its own way home: this returns each completed frame to
		// the pool its address belongs to, instead of to the transmit pool.
		opts = append(opts, xdp.WithTxReuseRxFrames())
	}
	// Placement is left to go-afxdp, which seats each worker beside its
	// queue's interrupt -- that is what a caller gets out of the box, and it
	// is most of the difference between a good AF_XDP number and a poor one.
	// -cpus overrides it.
	if len(c.cpus) > 0 {
		opts = append(opts, xdp.WithAffinity(c.cpus...))
	}
	return afxdp.Open(c.dev, afxdp.WithXDP(opts...))
}

func openAFPacket(c config) (packetio.Device, error) {
	tx, rx := c.queues, c.queues
	switch c.mode {
	case "tx":
		rx = 0
	case "rx":
		tx = 0
	}
	return afpacket.Open(c.dev,
		afpacket.WithTxQueues(tx), afpacket.WithRxQueues(rx),
		afpacket.WithFrames(nextPow2((tx+rx)*c.depth*4)),
		afpacket.WithPromiscuous())
}

// ------------------------------------------------------------------ util

func parseCPUs(s string) ([]int, error) {
	if s == "" {
		return nil, nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			a, err := strconv.Atoi(strings.TrimSpace(lo))
			if err != nil {
				return nil, err
			}
			b, err := strconv.Atoi(strings.TrimSpace(hi))
			if err != nil {
				return nil, err
			}
			for i := a; i <= b; i++ {
				out = append(out, i)
			}
			continue
		}
		v, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func nextPow2(n int) int {
	if n < 1 {
		return 1
	}
	return 1 << bits.Len(uint(n-1))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
