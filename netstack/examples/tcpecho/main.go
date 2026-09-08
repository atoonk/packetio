//go:build linux

// Command tcpecho is a TCP echo server -- and, with -client, a client -- on
// gVisor's TCP/IP stack over any packetio backend. It is the smallest program
// that shows the whole idea: packets come off the wire through packetio, a
// userspace stack turns them into net.Conn, and the kernel is nowhere on the
// path.
//
//	# an echo server on 10.0.0.2, taking the address from nobody else
//	sudo go run . -backend afpacket -i eth1 -ip 10.0.0.2/24
//	sudo go run . -backend afxdp    -i eth1 -ip 10.0.0.2/24
//	sudo go run -tags dpdk . -backend dpdk -i 0000:00:06.0 -ip 10.0.2.132/24 -gw 10.0.2.1
//	sudo go run -tags mlx5 . -backend mlx5 -i eno2 -vlan 2043 -ip 192.168.43.9/24
//
//	# from another machine
//	nc 10.0.0.2 8080
//
//	# or the same program as a client, on the same stack, sending -bytes and
//	# checking the echo
//	sudo go run . -backend afpacket -i eth1 -ip 10.0.0.3/24 -client 10.0.0.2:8080
//
// Which address to give the stack depends on the backend; see the netstack
// package documentation. In short: on afpacket, an address the kernel does
// not have (the program refuses one it has, unless -force); on afxdp a
// spare one -- this program steers ARP to the stack along with the port, so
// the kernel no longer sees ARP on that interface: not one you manage the
// machine over; on mlx5 the interface's own or a spare one; on dpdk with
// the device taken from the kernel, the device's address.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	xdp "github.com/atoonk/go-afxdp"
	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/afpacket"
	"github.com/atoonk/packetio/afxdp"
	"github.com/atoonk/packetio/netstack"
)

// opener opens one backend. Backends behind a build tag register themselves
// from their own file, so this program compiles with whatever is available.
// The MAC it returns is the one the stack sends from: the interface's where
// there is one, the device's own where the kernel has no interface.
type opener func(c config) (packetio.Device, net.HardwareAddr, error)

var openers = map[string]opener{
	"afpacket": openAFPacket,
	"afxdp":    openAFXDP,
}

// config is the command line. Backends behind a build tag read the fields
// they need from their own file.
type config struct {
	backend   string
	iface     string
	ip        string
	addr      netip.Prefix
	macStr    string
	mac       net.HardwareAddr
	vlan      int
	port      int
	gwStr     string
	gw        netip.Addr
	mtu       int
	queues    int
	client    string
	peer      int // the port replies come from, for a client's steering
	nbytes    int
	generic   bool
	frames    int
	devargs   string
	noHuge    int
	stats     time.Duration
	busy      bool
	noGRO     bool
	mode      string
	direct    bool
	force     bool
	neighbor  string
	neighbors map[netip.Addr]net.HardwareAddr
}

func main() {
	var c config
	flag.StringVar(&c.backend, "backend", "afpacket", "afpacket, afxdp, dpdk or mlx5 (the last two need -tags)")
	flag.StringVar(&c.iface, "i", "", "interface; a PCI address or vdev for dpdk")
	flag.StringVar(&c.ip, "ip", "", "the stack's address with its prefix, for example 10.0.0.2/24 (required)")
	flag.StringVar(&c.macStr, "mac", "", "the stack's MAC; defaults to the interface's, or the device's")
	flag.IntVar(&c.vlan, "vlan", 0, "802.1Q tag every frame carries, or 0")
	flag.IntVar(&c.port, "port", 8080, "port to serve on")
	flag.StringVar(&c.gwStr, "gw", "", "gateway for destinations off the prefix")
	flag.IntVar(&c.mtu, "mtu", 0, "IP MTU; 0 is 1500 or what the frame allows")
	flag.IntVar(&c.queues, "queues", 1, "queues to open (afxdp opens every queue regardless)")
	flag.StringVar(&c.client, "client", "", "host:port to connect to instead of serving; sends -bytes and checks the echo")
	flag.IntVar(&c.nbytes, "bytes", 16<<20, "bytes a client sends")
	flag.BoolVar(&c.generic, "generic", false, "afxdp: generic XDP, for a NIC or veth without native XDP support (strips VLAN tags: -vlan will not work)")
	flag.IntVar(&c.frames, "frames", 0, "frames in the region; 0 is the backend's default")
	flag.StringVar(&c.devargs, "devargs", "", "dpdk: driver arguments")
	flag.IntVar(&c.noHuge, "no-hugepages", 0, "dpdk: run on ordinary memory with this many megabytes, for a vdev")
	flag.DurationVar(&c.stats, "stats", 0, "print counters this often, or never")
	flag.BoolVar(&c.busy, "busypoll", false, "keep polling when idle on a backend that cannot sleep (dpdk, mlx5): lowest latency, a core per queue")
	flag.BoolVar(&c.noGRO, "no-gro", false, "do not merge bursts of received segments before the stack sees them")
	flag.StringVar(&c.mode, "mode", "echo", "what the server does with a connection: echo, sink (read and discard) or source (write until the peer hangs up)")
	flag.BoolVar(&c.direct, "direct", false, "transmit from the goroutine that produced a packet instead of a transmit goroutine per queue")
	flag.StringVar(&c.neighbor, "neighbor", "", "a peer's address and MAC, ip=mac, pinned in the MAC table; the gateway needs this where the kernel answers ARP")
	flag.BoolVar(&c.force, "force", false, "run even on an address the kernel owns on the interface")
	flag.Parse()
	if c.iface == "" || c.ip == "" {
		fmt.Fprintln(os.Stderr, "need -i and -ip; try -i eth1 -ip 10.0.0.2/24")
		flag.Usage()
		os.Exit(2)
	}
	if err := c.parse(); err != nil {
		log.Fatal(err)
	}
	if err := run(c); err != nil {
		log.Fatal(err)
	}
}

// parse turns the string flags into addresses and checks the ports.
func (c *config) parse() error {
	var err error
	if c.addr, err = netip.ParsePrefix(c.ip); err != nil {
		return fmt.Errorf("-ip: %w", err)
	}
	if c.macStr != "" {
		if c.mac, err = net.ParseMAC(c.macStr); err != nil {
			return fmt.Errorf("-mac: %w", err)
		}
	}
	if c.gwStr != "" {
		if c.gw, err = netip.ParseAddr(c.gwStr); err != nil {
			return fmt.Errorf("-gw: %w", err)
		}
	}
	if c.port < 1 || c.port > 65535 {
		return fmt.Errorf("-port %d: want 1 to 65535", c.port)
	}
	if c.vlan < 0 || c.vlan > 4094 {
		return fmt.Errorf("-vlan %d: want 0 to 4094", c.vlan)
	}
	if c.neighbor != "" {
		ipStr, macStr, ok := strings.Cut(c.neighbor, "=")
		if !ok {
			return fmt.Errorf("-neighbor %q: want ip=mac", c.neighbor)
		}
		ip, err := netip.ParseAddr(ipStr)
		if err != nil {
			return fmt.Errorf("-neighbor: %w", err)
		}
		mac, err := net.ParseMAC(macStr)
		if err != nil {
			return fmt.Errorf("-neighbor: %w", err)
		}
		c.neighbors = map[netip.Addr]net.HardwareAddr{ip: mac}
	}
	if c.client != "" {
		_, p, err := net.SplitHostPort(c.client)
		if err != nil {
			return fmt.Errorf("-client: %w", err)
		}
		if c.peer, err = strconv.Atoi(p); err != nil || c.peer < 1 || c.peer > 65535 {
			return fmt.Errorf("-client: port %q: want a number from 1 to 65535", p)
		}
	}
	switch c.mode {
	case "echo", "sink", "source":
	default:
		return fmt.Errorf("-mode %q: want echo, sink or source", c.mode)
	}
	return nil
}

// run opens the device, brings the stack up on it and serves, or dials,
// until the context ends. Everything opened is closed on the way out, in
// order: the stack first, so nothing is inside a queue when the device goes.
func run(c config) error {
	open, ok := openers[c.backend]
	if !ok {
		return fmt.Errorf("backend %q is not built in; rebuild with -tags %s", c.backend, c.backend)
	}
	if err := checkKernelOwns(c); err != nil {
		return err
	}

	dev, devMAC, err := open(c)
	if err != nil {
		return fmt.Errorf("open %s: %w", c.backend, err)
	}
	defer dev.Close()
	if c.mac == nil {
		c.mac = devMAC
	}
	st, err := netstack.New(dev, netstack.Config{
		Addr: c.addr, MAC: c.mac, VLAN: uint16(c.vlan), MTU: c.mtu, Gateway: c.gw,
		Neighbors: c.neighbors,
		BusyPoll:  c.busy, NoGRO: c.noGRO, DirectTx: c.direct,
	})
	if err != nil {
		return err
	}
	defer st.Close()
	caps := dev.Capabilities()
	log.Printf("%s on %s: %s, MAC %s, zero-copy %v, frames of %d bytes",
		caps.Backend, c.iface, c.addr, c.mac, caps.ZeroCopy, caps.MaxFrameSize)

	// Ctrl-C ends the context; a device failure does too, since the stack
	// cannot do anything useful after one.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-st.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	if c.stats > 0 {
		go report(ctx, st, c.stats)
	}
	if c.client != "" {
		err = runClient(ctx, st, c.client, c.nbytes)
	} else {
		err = serve(ctx, st, c.port, c.mode)
	}
	if derr := st.Err(); derr != nil {
		return derr
	}
	return err
}

// serve handles every connection until the context ends: echoing, or
// draining, or filling it, by mode. When the context ends the listener and
// every open connection are closed, so the peers hear about it.
func serve(ctx context.Context, st *netstack.Stack, port int, mode string) error {
	var handle func(c net.Conn) (int64, error)
	switch mode {
	case "echo":
		handle = func(c net.Conn) (int64, error) { return io.Copy(c, c) }
	case "sink":
		handle = func(c net.Conn) (int64, error) { return io.Copy(io.Discard, c) }
	case "source":
		src := make([]byte, 64<<10)
		handle = func(c net.Conn) (int64, error) {
			var n int64
			for {
				m, err := c.Write(src)
				n += int64(m)
				if err != nil {
					return n, err
				}
			}
		}
	}
	ln, err := st.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	defer ln.Close()
	log.Printf("%s: serving on port %d", mode, port)

	var conns sync.Map // open connections, closed when the context ends
	go func() {
		<-ctx.Done()
		ln.Close()
		conns.Range(func(k, _ any) bool { k.(net.Conn).Close(); return true })
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		conns.Store(c, struct{}{})
		go func() {
			defer conns.Delete(c)
			defer c.Close()
			peer := c.RemoteAddr() // read now: it is gone once the peer hangs up
			n, _ := handle(c)
			log.Printf("%v: %s %d bytes", peer, mode, n)
		}()
	}
}

// runClient connects, sends n bytes and reads them back, and reports the
// rate. The context ending closes the connection, which ends the read.
func runClient(ctx context.Context, st *netstack.Stack, addr string, n int) error {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := st.Dial(dctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer c.Close()
	go func() {
		<-ctx.Done()
		c.Close()
	}()
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*13 + i>>10)
	}
	start := time.Now()
	werr := make(chan error, 1)
	go func() {
		_, err := c.Write(out)
		werr <- err
	}()
	in := make([]byte, n)
	if _, err := io.ReadFull(c, in); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("read: %w", err)
	}
	if err := <-werr; err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if !bytes.Equal(in, out) {
		return fmt.Errorf("the echo differs from what was sent")
	}
	d := time.Since(start)
	log.Printf("echoed %d bytes in %v: %.2f Gbit/s each way", n, d.Round(time.Millisecond),
		float64(n)*8/d.Seconds()/1e9)
	return nil
}

// report prints the counters: the endpoint's, with every receive-side drop
// reason so a stack that hears nothing says why, and TCP's.
func report(ctx context.Context, st *netstack.Stack, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s := st.Stats()
			tcp := st.TCPIP().Stats().TCP
			log.Printf("rx %d tx %d gso %d/%d gro %d/%d | dropped: ring %d qdisc %d device %d/%d waited %d unknown-mac %d | rx drops: vlan %d notip %d csum %d frag %d runt %d chained %d | tcp established %d retransmits %d timeouts %d",
				s.Rx, s.Tx, s.GSOPackets, s.GSOFrames, s.GROFrames, s.GROPackets,
				s.TxDropped, s.QDiscDropped, s.DeviceRxDropped, s.DeviceTxDropped, s.TxWaited, s.UnknownMAC,
				s.WrongVLAN, s.NotIP, s.RxBadCsum, s.Fragments, s.Runt, s.Chained,
				tcp.CurrentEstablished.Value(), tcp.Retransmits.Value(), tcp.Timeouts.Value())
		}
	}
}

// checkKernelOwns refuses an address the kernel has on the interface, on a
// backend the kernel shares: the kernel then answers every SYN with a reset
// before the stack sees it. dpdk's af_packet vdev is such a backend too,
// but names the interface in its devargs; it is not checked. -force runs
// anyway, for the experiment.
func checkKernelOwns(c config) error {
	if c.backend != "afpacket" || c.force {
		return nil
	}
	ifi, err := net.InterfaceByName(c.iface)
	if err != nil {
		return nil // not a kernel interface; the open will say
	}
	addrs, _ := ifi.Addrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(c.addr.Addr().AsSlice()) {
			return fmt.Errorf("%s is the kernel's own address on %s; on %s the kernel resets every connection to it. Use an address the kernel does not have, or -force",
				c.addr.Addr(), c.iface, c.backend)
		}
	}
	return nil
}

// ifaceMAC is the kernel interface's MAC, for the backends that keep one.
func ifaceMAC(name string) (net.HardwareAddr, error) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	return ifi.HardwareAddr, nil
}

func openAFPacket(c config) (packetio.Device, net.HardwareAddr, error) {
	mac, err := ifaceMAC(c.iface)
	if err != nil {
		return nil, nil, err
	}
	opts := []afpacket.Option{afpacket.WithQueues(c.queues)}
	if c.frames > 0 {
		opts = append(opts, afpacket.WithFrames(c.frames))
	}
	dev, err := afpacket.Open(c.iface, opts...)
	return dev, mac, err
}

// openAFXDP steers ARP and the stack's TCP traffic to the socket; the kernel
// keeps everything else. A server takes its port, a client the replies from
// the peer's.
func openAFXDP(c config) (packetio.Device, net.HardwareAddr, error) {
	mac, err := ifaceMAC(c.iface)
	if err != nil {
		return nil, nil, err
	}
	tcpMatch := xdp.MatchTCPPort(uint16(c.port))
	if c.client != "" {
		tcpMatch = xdp.MatchTCPSrcPort(uint16(c.peer))
	}
	xopts := []xdp.Option{xdp.WithFilter(xdp.MatchEtherType(0x0806), tcpMatch)}
	if c.generic {
		xopts = append(xopts, xdp.WithGenericMode())
	}
	opts := []afxdp.Option{afxdp.WithXDP(xopts...)}
	if c.frames > 0 {
		opts = append(opts, afxdp.WithFrames(c.frames))
	}
	dev, err := afxdp.Open(c.iface, opts...)
	return dev, mac, err
}
