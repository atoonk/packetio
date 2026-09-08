//go:build linux

package netstack

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/header"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/stack"
)

var (
	fakeAddrA = netip.MustParsePrefix("10.99.0.1/24")
	fakeAddrB = netip.MustParsePrefix("10.99.0.2/24")
	fakeMACA  = net.HardwareAddr{2, 0, 0, 0, 0, 1}
	fakeMACB  = net.HardwareAddr{2, 0, 0, 0, 0, 2}
)

// fakePair is two stacks on two fake devices wired back to back.
func fakePair(t *testing.T, cfgA, cfgB Config) (*Stack, *Stack, *fakeDevice, *fakeDevice) {
	t.Helper()
	a, b := newFakeDevice(), newFakeDevice()
	link(a, b)
	cfgA.Addr, cfgA.MAC, cfgA.Logf = fakeAddrA, fakeMACA, t.Logf
	cfgB.Addr, cfgB.MAC, cfgB.Logf = fakeAddrB, fakeMACB, t.Logf
	sa, err := New(a, cfgA)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := New(b, cfgB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sa.Close(); sb.Close() })
	return sa, sb, a, b
}

// TestFakeEcho is the two-stack wire working at all: A dials B, B echoes,
// 1 MiB each way through GSO and GRO, with the stacks the only thing between.
func TestFakeEcho(t *testing.T) {
	sa, sb, _, _ := fakePair(t, Config{}, Config{})
	ln, err := sb.Listen("tcp", ":80")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := sa.Dial(ctx, "tcp", "10.99.0.2:80")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	bulkEcho(t, c, 1<<20)
	for _, st := range []*Stack{sa, sb} {
		if s := st.Stats(); s.TxDropped != 0 || s.RxBadCsum != 0 || s.Fragments != 0 || s.UnknownMAC != 0 {
			t.Errorf("stats: %+v", s)
		}
	}
}

// TestCloseResetsConnections: closing a stack with a connection open tells
// the peer, which sees the connection end at once rather than hang.
func TestCloseResetsConnections(t *testing.T) {
	sa, sb, _, _ := fakePair(t, Config{}, Config{})
	ln, err := sb.Listen("tcp", ":80")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := sa.Dial(ctx, "tcp", "10.99.0.2:80")
	if err != nil {
		t.Fatal(err)
	}
	peer := <-accepted
	defer peer.Close()

	sa.Close() // with c still open
	peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = peer.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("the peer read data from a connection whose stack closed")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("the peer was not told: read timed out (%v)", err)
	}
	t.Logf("peer saw: %v", err)
	if _, err := c.Write([]byte("x")); err == nil {
		t.Fatal("write on a closed stack's connection succeeded")
	}
}

// TestNewTwice: a device takes one stack; a second is refused until the
// first closes.
var errBrokenTx = errors.New("the transmit queue broke")

func TestNewTwice(t *testing.T) {
	d := newFakeDevice()
	cfg := Config{Addr: fakeAddrA, MAC: fakeMACA, Logf: t.Logf}
	st, err := New(d, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(d, cfg); err == nil || !strings.Contains(err.Error(), "already has a stack") {
		t.Fatalf("second New: %v", err)
	}
	st.Close()
	st2, err := New(d, cfg)
	if err != nil {
		t.Fatalf("New after Close: %v", err)
	}
	st2.Close()
}

// TestDeviceFailure: a queue that fails is reported through Err and Done
// rather than looking like a quiet link.
func TestDeviceFailure(t *testing.T) {
	sa, _, a, _ := fakePair(t, Config{}, Config{})
	if err := sa.Err(); err != nil {
		t.Fatal(err)
	}
	broken := errors.New("the link went away")
	a.rx[0].fail(broken)
	select {
	case <-sa.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done not closed after the receive queue failed")
	}
	if err := sa.Err(); !errors.Is(err, broken) {
		t.Fatalf("Err = %v", err)
	}
}

// TestTxDroppedIsBackpressureNotFailure: a transmit queue that takes
// nothing is a full link -- AF_PACKET reports a full device queue exactly
// that way -- so the packets are dropped and counted, once each, and the
// stack goes on running. Only an error from the queue is a failure.
func TestTxDroppedIsBackpressureNotFailure(t *testing.T) {
	sa, _, a, _ := fakePair(t, Config{DirectTx: true}, Config{})
	a.tx[0].dead.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := sa.Dial(ctx, "tcp", "10.99.0.2:80"); err == nil {
		t.Fatal("dial succeeded over a queue taking nothing")
	}
	if err := sa.Err(); err != nil {
		t.Fatalf("a full queue was reported as a device failure: %v", err)
	}
	// One ARP request went nowhere: the dial waits on the resolution and
	// never sends a SYN, so exactly one packet is counted, not one per
	// attempt to hand it over.
	if s := sa.Stats(); s.TxDropped != 1 {
		t.Fatalf("TxDropped = %d, want 1: %+v", s.TxDropped, s)
	}
	a.tx[0].dead.Store(false)

	// An error, though, is a failure.
	sb, _, b, _ := fakePair(t, Config{DirectTx: true}, Config{})
	b.tx[0].err.Store(&errBrokenTx)
	bctx, bcancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer bcancel()
	sb.Dial(bctx, "tcp", "10.99.0.2:80")
	if err := sb.Err(); err == nil {
		t.Fatal("an error from the transmit queue was not reported")
	}
}

// TestChainedFramesSkipped: a packet spanning several frames is not
// accepted, and neither is its last piece, which carries no flag.
func TestChainedFramesSkipped(t *testing.T) {
	sa, _, a, _ := fakePair(t, Config{}, Config{})
	f := wireSegment(1, make([]byte, 100), true) // any well-formed frame will do as the pieces
	a.rx[0].inject(f, packetio.OptContinued)
	a.rx[0].inject(f, packetio.OptContinued)
	a.rx[0].inject(f, 0) // the tail: not a frame, must not be parsed as one
	a.rx[0].inject(f, 0) // an ordinary frame after the chain, delivered (and dropped for its VLAN)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s := sa.Stats(); s.Chained == 3 && s.WrongVLAN == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("stats: %+v, want Chained 3 and WrongVLAN 1", sa.Stats())
}

// TestConfigDefaults is what withDefaults accepts and refuses.
func TestConfigDefaults(t *testing.T) {
	good := Config{Addr: fakeAddrA, MAC: fakeMACA}
	c, err := good.withDefaults()
	if err != nil || c.TxQueueLen != defaultTxQueueLen || c.SendBufferSize != defaultBuffer || c.ReceiveBufferSize != defaultBuffer {
		t.Fatalf("defaults: %+v, %v", c, err)
	}
	bad := map[string]Config{
		"no address":       {MAC: fakeMACA},
		"ipv6":             {Addr: netip.MustParsePrefix("fd00::1/64"), MAC: fakeMACA},
		"no MAC":           {Addr: fakeAddrA},
		"short MAC":        {Addr: fakeAddrA, MAC: net.HardwareAddr{1, 2}},
		"vlan":             {Addr: fakeAddrA, MAC: fakeMACA, VLAN: 4095},
		"negative MTU":     {Addr: fakeAddrA, MAC: fakeMACA, MTU: -1},
		"gateway off-link": {Addr: fakeAddrA, MAC: fakeMACA, Gateway: netip.MustParseAddr("10.98.0.1")},
		"negative queue":   {Addr: fakeAddrA, MAC: fakeMACA, TxQueueLen: -5},
		"negative buffer":  {Addr: fakeAddrA, MAC: fakeMACA, ReceiveBufferSize: -1},
		"bad neighbor":     {Addr: fakeAddrA, MAC: fakeMACA, Neighbors: map[netip.Addr]net.HardwareAddr{netip.MustParseAddr("10.99.0.9"): {1}}},
	}
	for name, cfg := range bad {
		if _, err := cfg.withDefaults(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	d := newFakeDevice()
	if _, err := New(d, Config{Addr: fakeAddrA, MAC: fakeMACA, MTU: fakeFrameSize}); err == nil {
		t.Error("an MTU the frame cannot carry was accepted")
	}
}

// TestPartialBatchDoesNotDuplicate: a device takes the frames it has room
// for, and the count it was asked for is not a packet boundary. A packet cut
// that way is already partly on the wire, so it counts as sent and the batch
// moves past it; rebuilding it would put those frames there twice and the
// batch would make no progress at all. TCP retransmits the missing tail.
func TestPartialBatchDoesNotDuplicate(t *testing.T) {
	d := newFakeDevice()
	e, err := newEndpoint(d, mustDefaults(t, Config{Addr: fakeAddrA, MAC: fakeMACA, VLAN: testVLAN}))
	if err != nil {
		t.Fatal(err)
	}
	// Two packets of four segments each. The queue takes six frames: all of
	// the first packet and half of the second.
	var list stack.PacketBufferList
	for i := 0; i < 2; i++ {
		pkt := gsoPacket(t, make([]byte, 4*testMSS), header.TCPFlagAck, true)
		defer pkt.DecRef()
		list.PushBack(pkt)
	}
	d.tx[0].accept.Store(6)

	sent, terr := e.WritePackets(list)
	if terr != nil {
		t.Fatalf("WritePackets: %v", terr)
	}
	if sent != 2 {
		t.Fatalf("WritePackets returned %d, want both packets: the cut one is on the wire and must not be rebuilt", sent)
	}
	if got := d.tx[0].sent.Load(); got != 6 {
		t.Fatalf("the queue was given %d frames, want 6", got)
	}
	if st := e.Stats(); st.Tx != 6 {
		t.Fatalf("stats: %+v, want Tx 6", st)
	}

	// Offered again, the queue must be given the packets that follow, not
	// the frames it already has.
	d.tx[0].accept.Store(-1)
	var next stack.PacketBufferList
	pkt := gsoPacket(t, make([]byte, testMSS), header.TCPFlagAck, true)
	defer pkt.DecRef()
	next.PushBack(pkt)
	if sent, terr := e.WritePackets(next); terr != nil || sent != 1 {
		t.Fatalf("WritePackets after a cut batch: %d, %v", sent, terr)
	}
	if got := d.tx[0].sent.Load(); got != 7 {
		t.Fatalf("the queue has been given %d frames in total, want 7", got)
	}
}

// mustDefaults is Config.withDefaults for a test that needs an endpoint
// without a stack over it.
func mustDefaults(t *testing.T, c Config) Config {
	t.Helper()
	c, err := c.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestManyQueues is the rule packetio makes and this package must keep: a
// queue is driven by one goroutine at a time. The receive goroutines and
// gVisor's fifo discipline are what keep it, by agreeing on the same
// queue index for a packet, so it is worth a test on a device with several.
func TestManyQueues(t *testing.T) {
	const queues = 4
	a, b := newFakeDeviceQueues(queues), newFakeDeviceQueues(queues)
	link(a, b)
	sa, err := New(a, Config{Addr: fakeAddrA, MAC: fakeMACA, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer sa.Close()
	sb, err := New(b, Config{Addr: fakeAddrB, MAC: fakeMACB, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	if n := sa.ep.NumTxQueues(); n != queues {
		t.Fatalf("the endpoint drives %d transmit queues, want %d", n, queues)
	}

	ln, err := sb.Listen("tcp", ":80")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()

	// Several connections at once, so more than one queue is in play.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			c, err := sa.Dial(ctx, "tcp", "10.99.0.2:80")
			if err != nil {
				t.Errorf("dial: %v", err)
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(10 * time.Second))
			bulkEcho(t, c, 64<<10)
		}()
	}
	wg.Wait()

	for _, d := range []*fakeDevice{a, b} {
		for i, q := range d.tx {
			if n := q.races.Load(); n != 0 {
				t.Errorf("transmit queue %d had %d overlapping goroutines", i, n)
			}
		}
	}
	used := 0
	for _, q := range a.tx {
		if q.sent.Load() > 0 {
			used++
		}
	}
	t.Logf("%d of %d transmit queues carried traffic", used, queues)
	for _, st := range []*Stack{sa, sb} {
		if s := st.Stats(); s.TxDropped != 0 || s.RxBadCsum != 0 || s.UnknownMAC != 0 {
			t.Errorf("stats: %+v", s)
		}
	}
}

// TestVLAN is a tagged link end to end, which is how the mlx5 rig runs and
// what only the frame-level tests covered before: both stacks tag every
// frame, and an untagged frame injected into a tagged endpoint is dropped.
func TestVLAN(t *testing.T) {
	sa, sb, a, _ := fakePair(t, Config{VLAN: 100}, Config{VLAN: 100})
	ln, err := sb.Listen("tcp", ":80")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := sa.Dial(ctx, "tcp", "10.99.0.2:80")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	bulkEcho(t, c, 256<<10)

	// Every frame on the wire carries the tag.
	if s := sa.Stats(); s.WrongVLAN != 0 || s.Rx == 0 {
		t.Fatalf("stats: %+v", s)
	}
	// An untagged frame, and one tagged for another VLAN, are not ours.
	before := sa.Stats().WrongVLAN
	a.rx[0].inject(wireFrame(1, make([]byte, 10), true, true)[4:], 0) // the tag cut out
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && sa.Stats().WrongVLAN == before {
		time.Sleep(5 * time.Millisecond)
	}
	if sa.Stats().WrongVLAN == before {
		t.Fatal("an untagged frame was accepted by a tagged endpoint")
	}
}

// TestConfigKnobs runs a bulk echo with each of the switches that change the
// data path, so none of them is only ever exercised on real hardware.
func TestConfigKnobs(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  Config
	}{
		{"no-gso", Config{NoGSO: true}},
		{"no-gro", Config{NoGRO: true}},
		{"direct-tx", Config{DirectTx: true}},
		{"neither-offload", Config{NoGSO: true, NoGRO: true}},
		{"small-mtu", Config{MTU: 576}},
	} {
		t.Run(c.name, func(t *testing.T) {
			sa, sb, _, _ := fakePair(t, c.cfg, c.cfg)
			ln, err := sb.Listen("tcp", ":80")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				io.Copy(conn, conn)
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			conn, err := sa.Dial(ctx, "tcp", "10.99.0.2:80")
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(20 * time.Second))
			bulkEcho(t, conn, 256<<10)
			s := sa.Stats()
			switch {
			case c.cfg.NoGSO && s.GSOPackets != 0:
				t.Errorf("GSO is off but %d packets were segmented", s.GSOPackets)
			case !c.cfg.NoGSO && s.GSOPackets == 0:
				t.Errorf("nothing was segmented: %+v", s)
			}
			switch {
			case c.cfg.NoGRO && s.GROFrames != 0:
				t.Errorf("GRO is off but %d frames went through it", s.GROFrames)
			case !c.cfg.NoGRO && s.GROFrames == 0:
				t.Errorf("nothing went through GRO: %+v", s)
			}
			if s.TxDropped != 0 || s.RxBadCsum != 0 || s.UnknownMAC != 0 {
				t.Errorf("stats: %+v", s)
			}
		})
	}
}

// TestClosedStack: what the API does after Close is part of the contract,
// and it says so in the way the net package does.
func TestClosedStack(t *testing.T) {
	sa, _, _, _ := fakePair(t, Config{}, Config{})
	ln, err := sa.Listen("tcp", ":80")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan error, 1)
	go func() { _, err := ln.Accept(); accepted <- err }()
	sa.Close()

	select {
	case err := <-accepted:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("Accept after Close: %v, want net.ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Accept did not return after Close")
	}
	if _, err := sa.Listen("tcp", ":81"); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Listen after Close: %v, want net.ErrClosed", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := sa.Dial(ctx, "tcp", "10.99.0.2:80"); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Dial after Close: %v, want net.ErrClosed", err)
	}
	if err := sa.Close(); err != nil {
		t.Errorf("Close twice: %v", err)
	}
}

// TestDeadline: a read that times out matches os.ErrDeadlineExceeded, as
// the net package's does, and reports itself as a timeout.
func TestDeadline(t *testing.T) {
	sa, sb, _, _ := fakePair(t, Config{}, Config{})
	ln, err := sb.Listen("tcp", ":80")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			time.Sleep(3 * time.Second)
			c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := sa.Dial(ctx, "tcp", "10.99.0.2:80")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, err = c.Read(make([]byte, 1))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("read past the deadline: %v, want os.ErrDeadlineExceeded", err)
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Errorf("read error does not report itself as a timeout: %v", err)
	}
}
