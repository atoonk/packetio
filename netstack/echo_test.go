//go:build linux

package netstack

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atoonk/packetio"
)

// backends is the devices the end-to-end tests run on, by name. Each backend
// file registers its opener from init(); the ones behind a build tag are
// only there when built with it.
var backends = map[string]func(t *testing.T, l *lab) packetio.Device{}

func backendNames() []string {
	names := make([]string, 0, len(backends))
	for n := range backends {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// required turns "cannot open" from a skip into a failure, the way the rest
// of the repository's conformance tests do.
func required() bool { return os.Getenv("PACKETIO_CONFORM_REQUIRED") != "" }

// openStack opens the backend on the near end of the lab and brings a stack
// up on it.
func openStack(t *testing.T, l *lab, open func(*testing.T, *lab) packetio.Device, cfg Config) *Stack {
	t.Helper()
	dev := open(t, l)
	if dev == nil {
		if required() {
			t.Fatal("cannot open the device, and PACKETIO_CONFORM_REQUIRED is set")
		}
		t.Skip("cannot open the device here")
	}
	cfg.Addr = netip.MustParsePrefix(labStackPfx)
	cfg.MAC = l.mac
	st, err := New(dev, cfg)
	if err != nil {
		dev.Close()
		t.Fatalf("netstack.New: %v", err)
	}
	t.Cleanup(func() {
		st.Close()
		dev.Close()
	})
	return st
}

// echoServer echoes every connection it accepts, and counts.
type echoServer struct {
	accepted, closed atomic.Int64
	stopping         atomic.Bool
}

func (s *echoServer) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		s.accepted.Add(1)
		go func() {
			defer s.closed.Add(1)
			defer c.Close()
			io.Copy(c, c)
		}()
	}
}

// TestEcho is the stack as a server on every backend: a kernel client in the
// peer namespace connects, 200 small messages go round byte-exact, then 4 MiB
// both ways at once, so the stack sends packets far larger than one MSS and
// the endpoint has to segment them.
func TestEcho(t *testing.T) {
	for _, name := range backendNames() {
		t.Run(name, func(t *testing.T) { testEcho(t, backends[name], Config{}) })
		t.Run(name+"/direct", func(t *testing.T) { testEcho(t, backends[name], Config{DirectTx: true}) })
	}
}

func testEcho(t *testing.T, open func(*testing.T, *lab) packetio.Device, cfg Config) {
	l := newLab(t)
	st := openStack(t, l, open, cfg)

	ln, err := st.Listen("tcp", ":8080")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	var srv echoServer
	go srv.serve(ln)

	c := l.dial(t, labStackIP+":8080")
	defer c.Close()

	// Small messages, well under one segment: request/response, before
	// the bulk transfer below exercises segmentation.
	buf := make([]byte, 1000)
	for i := 0; i < 200; i++ {
		msg := []byte(fmt.Sprintf("hello over netstack %04d ", i))
		msg = append(msg, bytes.Repeat([]byte{byte(i)}, 500)...)
		if _, err := c.Write(msg); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if _, err := io.ReadFull(c, buf[:len(msg)]); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if !bytes.Equal(buf[:len(msg)], msg) {
			t.Fatalf("echo %d differs", i)
		}
	}
	bulkEcho(t, c, 4<<20)
	if es := st.Stats(); es.GSOPackets == 0 {
		t.Errorf("bulk echo did not exercise GSO: %+v", es)
	} else {
		t.Logf("gso: %d packets -> %d frames", es.GSOPackets, es.GSOFrames)
	}
	c.Close()

	deadline := time.Now().Add(2 * time.Second)
	for srv.closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.accepted.Load() != 1 || srv.closed.Load() != 1 {
		t.Errorf("server saw accepted %d closed %d, want 1 and 1", srv.accepted.Load(), srv.closed.Load())
	}
	checkStats(t, st)
}

// TestDial is the stack as a client on every backend: it dials a kernel
// listener in the peer namespace, which makes the stack resolve the peer by
// ARP first, and echoes 1 MiB through it.
func TestDial(t *testing.T) {
	for _, name := range backendNames() {
		t.Run(name, func(t *testing.T) { testDial(t, backends[name]) })
	}
}

func testDial(t *testing.T, open func(*testing.T, *lab) packetio.Device) {
	l := newLab(t)
	st := openStack(t, l, open, Config{})

	ln := l.listen(t, labPeerIP+":9090")
	defer ln.Close()
	var srv echoServer
	go srv.serve(ln)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := st.Dial(ctx, "tcp", labPeerIP+":9090")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(20 * time.Second))

	msg := []byte("hello from netstack")
	if _, err := c.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(buf, msg) {
		t.Fatalf("echo differs: %q", buf)
	}
	bulkEcho(t, c, 1<<20)
	c.Close()
	checkStats(t, st)
}

// bulkEcho writes n bytes and reads them back concurrently, byte-exact.
func bulkEcho(t *testing.T, c net.Conn, n int) {
	t.Helper()
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*13 + i>>10)
	}
	werr := make(chan error, 1)
	go func() {
		_, err := c.Write(out)
		werr <- err
	}()
	in := make([]byte, n)
	if _, err := io.ReadFull(c, in); err != nil {
		t.Fatalf("bulk read: %v", err)
	}
	if err := <-werr; err != nil {
		t.Fatalf("bulk write: %v", err)
	}
	if !bytes.Equal(in, out) {
		t.Fatal("bulk echo differs")
	}
}

// checkStats is what every run must end with: bytes moved, nothing dropped,
// every checksum right.
func checkStats(t *testing.T, st *Stack) {
	t.Helper()
	es := st.Stats()
	if es.Rx == 0 || es.Tx == 0 {
		t.Errorf("endpoint moved nothing: %+v", es)
	}
	if es.UnknownMAC != 0 || es.WrongVLAN != 0 || es.TxDropped != 0 || es.Chained != 0 {
		t.Errorf("endpoint drops: %+v", es)
	}
	ts := st.TCPIP().Stats()
	if n := ts.TCP.ChecksumErrors.Value(); n != 0 {
		t.Errorf("netstack saw %d checksum errors", n)
	}
	if n := ts.TCP.Retransmits.Value(); n != 0 {
		t.Logf("note: %d retransmits over the veth", n)
	}
	t.Logf("stats: %+v", es)
}
