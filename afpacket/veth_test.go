//go:build linux

package afpacket

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/atoonk/packetio"
)

// These run against a real veth pair, so they need root. They are the only
// tests that touch a socket: everything above is the ring walk in isolation.
func needRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root to create a veth pair and open AF_PACKET sockets")
	}
	// These tests create and destroy network interfaces on the machine they
	// run on. That is fine on a throwaway box and not fine on a shared one, so
	// it is opt-in: a plain "sudo go test ./..." must not reconfigure someone's
	// networking as a side effect of running the unit tests.
	if os.Getenv("PACKETIO_VETH_TESTS") == "" {
		t.Skip("set PACKETIO_VETH_TESTS=1 to allow this test to create veth interfaces on this machine")
	}
}

// vethPair creates a connected pair and returns both names.
func vethPair(t *testing.T) (string, string) {
	t.Helper()
	// Names are derived from the pid, so two runs on one machine do not
	// collide. They are NOT deleted first: an interface of this name that
	// already exists belongs to something else -- another run, or a real
	// interface someone named unfortunately -- and deleting it because the
	// name collided is not this test's business. Fail instead.
	a := fmt.Sprintf("pio%da", os.Getpid()%1000)
	b := fmt.Sprintf("pio%db", os.Getpid()%1000)
	for _, n := range []string{a, b} {
		if _, err := net.InterfaceByName(n); err == nil {
			t.Skipf("an interface called %s already exists; not touching it", n)
		}
	}
	if out, err := exec.Command("ip", "link", "add", a, "type", "veth", "peer", "name", b).CombinedOutput(); err != nil {
		t.Skipf("cannot create a veth pair: %v: %s", err, out)
	}
	t.Cleanup(func() { exec.Command("ip", "link", "del", a).Run() })
	for _, n := range []string{a, b} {
		// The kernel sends IPv6 duplicate-address-detection packets the moment
		// a link comes up, and a promiscuous receiver sees them. They are not
		// this test's traffic and asserting on them made these tests flaky.
		exec.Command("sysctl", "-qw", "net.ipv6.conf."+n+".disable_ipv6=1").Run()
		if out, err := exec.Command("ip", "link", "set", n, "up").CombinedOutput(); err != nil {
			t.Fatalf("bringing %s up: %v: %s", n, err, out)
		}
	}
	return a, b
}

// testSrcMAC is the source address every frame these tests send carries, and
// the only traffic they assert on. See vethPair.
var testSrcMAC = [6]byte{0x02, 0, 0, 0, 0, 0x01}

// ours reports whether a received frame is one this test sent.
func ours(b []byte) bool {
	return len(b) >= 12 && [6]byte(b[6:12]) == testSrcMAC
}

// frame builds a recognisable Ethernet frame of the given total length.
func frame(seq byte, n int) []byte {
	b := make([]byte, n)
	copy(b[0:6], []byte{0x02, 0, 0, 0, 0, 0x02})
	copy(b[6:12], []byte{0x02, 0, 0, 0, 0, 0x01})
	b[12], b[13] = 0x08, 0x00
	for i := 14; i < n; i++ {
		b[i] = seq
	}
	return b
}

func TestVethSendAndReceive(t *testing.T) {
	needRoot(t)
	a, b := vethPair(t)

	rx, err := Open(b, WithTxQueues(0), WithRxQueues(1), WithPromiscuous())
	if err != nil {
		t.Fatalf("open receiver on %s: %v", b, err)
	}
	defer rx.Close()

	tx, err := Open(a, WithTxQueues(1), WithRxQueues(0))
	if err != nil {
		t.Fatalf("open sender on %s: %v", a, err)
	}
	defer tx.Close()

	const n = 64
	txq := tx.TxQueue(0)
	sent, err := txq.SendFunc(n, func(i int, buf []byte) int {
		return copy(buf, frame(byte(i), 100))
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if sent != n {
		t.Errorf("sent %d of %d", sent, n)
	}

	rxq := rx.RxQueue(0)
	var got [][]byte
	deadline := time.Now().Add(3 * time.Second)
	for len(got) < n && time.Now().Before(deadline) {
		if _, err := rxq.Poll(200 * time.Millisecond); err != nil {
			t.Fatalf("poll: %v", err)
		}
		descs := rxq.Receive(n)
		for _, d := range descs {
			if f := rxq.Region().Frame(d); ours(f) {
				got = append(got, append([]byte(nil), f...))
			}
		}
		rxq.Recycle(descs)
	}
	if len(got) < n {
		t.Fatalf("received %d of %d frames", len(got), n)
	}
	// The pair carries nothing else, so what arrives should be what was sent,
	// in order and byte for byte.
	for i := 0; i < n; i++ {
		if want := frame(byte(i), 100); !bytes.Equal(got[i], want) {
			t.Fatalf("frame %d differs: got %d bytes, want %d", i, len(got[i]), len(want))
		}
	}
}

func TestVethFramesAreConserved(t *testing.T) {
	needRoot(t)
	a, _ := vethPair(t)
	d, err := Open(a, WithTxQueues(1), WithRxQueues(0), WithFrames(512))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	q := d.TxQueue(0)
	before := q.NumFreeFrames()
	if before == 0 {
		t.Fatal("no frames to start with")
	}
	// Several rounds, because a frame leaked once per round is invisible in one.
	for round := 0; round < 8; round++ {
		if _, err := q.SendFunc(32, func(i int, buf []byte) int {
			return copy(buf, frame(byte(i), 200))
		}); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	// Sent frames wait for Complete, as on any other backend, so ask for them
	// before checking the pool.
	q.Complete(1 << 20)
	if after := q.NumFreeFrames(); after != before {
		t.Errorf("frames leaked: %d free before, %d after", before, after)
	}
	if s, _ := q.Stats(); s.Backend["pool_rejected"] != 0 {
		t.Errorf("the pool refused %d frames; something returned a frame twice",
			s.Backend["pool_rejected"])
	}
}

func TestVethAllocAndFreeRoundTrip(t *testing.T) {
	needRoot(t)
	a, _ := vethPair(t)
	d, err := Open(a, WithTxQueues(1), WithRxQueues(0), WithFrames(256))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	q := d.TxQueue(0)
	before := q.NumFreeFrames()
	descs := q.Alloc(10)
	if len(descs) != 10 {
		t.Fatalf("allocated %d, want 10", len(descs))
	}
	if q.NumFreeFrames() != before-10 {
		t.Errorf("free frames %d, want %d", q.NumFreeFrames(), before-10)
	}
	// Every descriptor must name a distinct frame inside the region.
	seen := map[uint64]bool{}
	for _, d2 := range descs {
		if seen[d2.Addr] {
			t.Fatalf("frame %d handed out twice", d2.Addr)
		}
		seen[d2.Addr] = true
		if w := q.Region().Writable(d2); len(w) != q.Region().FrameSize()-frameHeadroom {
			t.Errorf("writable frame is %d bytes, want %d", len(w), q.Region().FrameSize()-frameHeadroom)
		}
	}
	q.Free(descs)
	if q.NumFreeFrames() != before {
		t.Errorf("after Free: %d free, want %d", q.NumFreeFrames(), before)
	}
}

func TestOpenRejectsBadConfiguration(t *testing.T) {
	// No socket needed: these fail in validation.
	for _, tc := range []struct {
		name string
		opts []Option
	}{
		{"no queues", []Option{WithTxQueues(0), WithRxQueues(0)}},
		{"frame size not a power of two", []Option{WithFrameSize(1000)}},
		{"frame size past the ring frame", []Option{WithFrameSize(8192)}},
		{"too few frames", []Option{WithFrames(8)}},
		{"too many queues", []Option{WithQueues(maxQueues + 1)}},
		{"gso frame too small", []Option{WithGSO(), WithFrameSize(2048)}},
		{"region too large", []Option{WithFrames(1 << 20), WithFrameSize(2048)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Open("lo", tc.opts...); err == nil {
				t.Error("accepted a configuration that should have been refused")
			}
		})
	}
}

var _ packetio.Device = (*Device)(nil)

func TestVethGSOOpensAndTransmits(t *testing.T) {
	needRoot(t)
	a, b := vethPair(t)
	// A veth pair with a large MTU, so a super-frame is not refused outright.
	exec.Command("ip", "link", "set", a, "mtu", "9000").Run()
	exec.Command("ip", "link", "set", b, "mtu", "9000").Run()

	rx, err := Open(b, WithTxQueues(0), WithRxQueues(1), WithGSO(), WithFrames(256), WithPromiscuous())
	if err != nil {
		t.Skipf("GSO receive unavailable here: %v", err)
	}
	defer rx.Close()
	if !rx.Capabilities().Offload {
		t.Error("a GSO device should report Offload")
	}
	if _, ok := rx.RxQueue(0).(packetio.OffloadReceiver); !ok {
		t.Fatal("a GSO receive queue must implement OffloadReceiver")
	}

	tx, err := Open(a, WithTxQueues(1), WithRxQueues(0), WithGSO(), WithFrames(256))
	if err != nil {
		t.Fatalf("open sender: %v", err)
	}
	defer tx.Close()
	otx, ok := tx.TxQueue(0).(packetio.OffloadTransmitter)
	if !ok {
		t.Fatal("a GSO transmit queue must implement OffloadTransmitter")
	}

	// An ordinary frame through the offload path: a zero Offload must behave
	// exactly like Transmit, header and all.
	q := tx.TxQueue(0)
	descs := q.Alloc(4)
	if len(descs) != 4 {
		t.Fatalf("allocated %d", len(descs))
	}
	for i := range descs {
		n := copy(q.Region().Writable(descs[i]), frame(byte(i), 300))
		descs[i].Len = uint32(n)
	}
	sent, err := otx.TransmitOffload(descs, make([]packetio.Offload, len(descs)))
	if err != nil {
		t.Fatalf("TransmitOffload: %v", err)
	}
	if sent != len(descs) {
		t.Fatalf("TransmitOffload sent %d of %d (errno %d)", sent, len(descs), tx.Tx(0).LastErrno())
	}

	// A mismatched offload slice, and an offload whose offsets do not fit the
	// frame, are both the caller's bug and must be named as one rather than
	// handed to the kernel to refuse for the whole batch.
	if _, err := otx.TransmitOffload(descs[:1], nil); !errors.Is(err, packetio.ErrBadLength) {
		t.Errorf("mismatched lengths: got %v, want ErrBadLength", err)
	}
	bad := []packetio.Offload{{Flags: packetio.OffloadNeedsCsum, CsumStart: 60000, CsumOff: 16}}
	if _, err := otx.TransmitOffload(descs[:1], bad); !errors.Is(err, packetio.ErrBadLength) {
		t.Errorf("csum past the frame: got %v, want ErrBadLength", err)
	}
	seg := []packetio.Offload{{GSOType: packetio.OffloadGSOTCPv4, GSOSize: 0}}
	if _, err := otx.TransmitOffload(descs[:1], seg); !errors.Is(err, packetio.ErrBadLength) {
		t.Errorf("zero segment size: got %v, want ErrBadLength", err)
	}

	rxq := rx.RxQueue(0).(packetio.OffloadReceiver)
	var got int
	deadline := time.Now().Add(3 * time.Second)
	for got < 4 && time.Now().Before(deadline) {
		if _, err := rx.RxQueue(0).Poll(200 * time.Millisecond); err != nil {
			t.Fatalf("poll: %v", err)
		}
		ds, offs := rxq.ReceiveOffload(16)
		if len(ds) != len(offs) {
			t.Fatalf("got %d descriptors but %d offloads", len(ds), len(offs))
		}
		for i, d := range ds {
			f := rx.Region().Frame(d)
			if !ours(f) {
				continue // not ours; the link carries its own housekeeping
			}
			if len(f) != 300 {
				t.Errorf("frame %d is %d bytes, want 300", i, len(f))
			}
			if offs[i].Segmented() {
				t.Errorf("an ordinary frame came back marked segmented: %+v", offs[i])
			}
			got++
		}
		rx.RxQueue(0).Recycle(ds)
	}
	if got < 4 {
		t.Fatalf("received %d of 4", got)
	}
}

// tcpSegment builds Ethernet/IPv4/TCP with payload bytes of data, and returns
// the offsets a virtio header needs. The kernel refuses a GSO descriptor whose
// frame it cannot parse, so a super-frame test needs a real one.
func tcpSegment(payload int) (frame []byte, hdrLen, csumStart, csumOff int) {
	const eth, ip, tcp = 14, 20, 20
	f := make([]byte, eth+ip+tcp+payload)
	copy(f[0:6], []byte{0x02, 0, 0, 0, 0, 0x02})
	copy(f[6:12], []byte{0x02, 0, 0, 0, 0, 0x01})
	f[12], f[13] = 0x08, 0x00 // IPv4
	f[14] = 0x45              // version 4, IHL 5
	binary.BigEndian.PutUint16(f[16:], uint16(ip+tcp+payload))
	f[22] = 64 // TTL
	f[23] = 6  // TCP
	copy(f[26:30], []byte{192, 168, 0, 1})
	copy(f[30:34], []byte{192, 168, 0, 2})
	binary.BigEndian.PutUint16(f[34:], 12345)
	binary.BigEndian.PutUint16(f[36:], 80)
	f[46] = 5 << 4 // data offset: 20 bytes
	f[47] = 0x10   // ACK
	binary.BigEndian.PutUint16(f[48:], 65535)
	for i := eth + ip + tcp; i < len(f); i++ {
		f[i] = byte(i)
	}
	return f, eth + ip + tcp, eth + ip, 16
}

func TestVethGSOFramesAreConserved(t *testing.T) {
	needRoot(t)
	a, _ := vethPair(t)
	d, err := Open(a, WithTxQueues(1), WithRxQueues(0), WithGSO(), WithFrames(128))
	if err != nil {
		t.Skipf("GSO unavailable here: %v", err)
	}
	defer d.Close()
	q := d.TxQueue(0)
	otx := q.(packetio.OffloadTransmitter)
	before := q.NumFreeFrames()
	for round := 0; round < 8; round++ {
		descs := q.Alloc(16)
		offs := make([]packetio.Offload, len(descs))
		seg, hdrLen, _, _ := tcpSegment(4000)
		for i := range descs {
			n := copy(q.Region().Writable(descs[i]), seg)
			descs[i].Len = uint32(n)
			offs[i] = packetio.Offload{
				GSOType: packetio.OffloadGSOTCPv4,
				GSOSize: 1400,
				HdrLen:  uint16(hdrLen),
			}
		}
		sent, err := otx.TransmitOffload(descs, offs)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if sent != len(descs) {
			// The kernel refused a super-frame it should have segmented. That
			// is the whole feature, so fail rather than quietly leak.
			t.Fatalf("round %d: sent %d of %d super-frames (errno %d)",
				round, sent, len(descs), d.Tx(0).LastErrno())
		}
		q.Complete(1 << 20)
	}
	if after := q.NumFreeFrames(); after != before {
		t.Errorf("frames leaked through the offload path: %d before, %d after", before, after)
	}
}

func TestTransmitOffloadRefusedWithoutGSO(t *testing.T) {
	needRoot(t)
	a, _ := vethPair(t)
	d, err := Open(a, WithTxQueues(1), WithRxQueues(0))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()
	q := d.TxQueue(0)
	otx, ok := q.(packetio.OffloadTransmitter)
	if !ok {
		t.Skip("not an OffloadTransmitter")
	}
	descs := q.Alloc(2)
	defer q.Free(descs)
	// Without PACKET_VNET_HDR the kernel would read the header bytes as packet
	// data, so this must refuse rather than corrupt the wire, and say why.
	n, err := otx.TransmitOffload(descs, make([]packetio.Offload, len(descs)))
	if n != 0 {
		t.Errorf("sent %d frames with offload on a non-GSO socket", n)
	}
	if !errors.Is(err, packetio.ErrUnsupported) {
		t.Errorf("got %v, want ErrUnsupported", err)
	}
}

func TestCloseWakesABlockedPoll(t *testing.T) {
	needRoot(t)
	a, _ := vethPair(t)
	d, err := Open(a, WithTxQueues(0), WithRxQueues(1))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	q := d.RxQueue(0)

	// Nothing is sent on this pair, so Poll(-1) blocks indefinitely. Closing
	// the socket underneath it does not wake poll(2); the queue's eventfd is
	// what makes shutdown possible at all.
	done := make(chan error, 1)
	go func() {
		_, err := q.Poll(-1)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond) // let it reach poll(2)

	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, packetio.ErrClosed) {
			t.Errorf("Poll returned %v, want ErrClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Poll did not return after Close: a shutdown would hang here")
	}
}

func TestCloseTwiceAndAfterClose(t *testing.T) {
	needRoot(t)
	a, _ := vethPair(t)
	d, err := Open(a, WithTxQueues(1), WithRxQueues(1))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	tx, rx := d.TxQueue(0), d.RxQueue(0)
	if err := d.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
	// Every queue method must refuse rather than touch unmapped memory.
	if n := tx.Transmit(tx.Alloc(1)); n != 0 {
		t.Errorf("Transmit after Close sent %d", n)
	}
	if _, err := tx.SendFunc(4, func(int, []byte) int { return 64 }); !errors.Is(err, packetio.ErrClosed) {
		t.Errorf("SendFunc after Close: %v, want ErrClosed", err)
	}
	if _, err := rx.Poll(0); !errors.Is(err, packetio.ErrClosed) {
		t.Errorf("Poll after Close: %v, want ErrClosed", err)
	}
	if d := rx.Receive(16); len(d) != 0 {
		t.Errorf("Receive after Close returned %d descriptors", len(d))
	}
}

func TestMultiQueueReceiveDeliversOnEveryQueue(t *testing.T) {
	needRoot(t)
	a, b := vethPair(t)
	// Four receive queues in a fanout group. Every queue must be able to
	// deliver: the bug this exists for left every queue but the first silently
	// dead, because its pool addresses fell outside the region.
	rx, err := Open(b, WithTxQueues(0), WithRxQueues(4), WithFrames(1024), WithPromiscuous())
	if err != nil {
		t.Fatalf("open receiver: %v", err)
	}
	defer rx.Close()
	for i := 0; i < rx.NumRxQueues(); i++ {
		q := rx.RxQueue(i)
		if q.NumFreeFrames() == 0 {
			t.Fatalf("queue %d has no frames", i)
		}
		// The decisive check, and the cheap one: a frame from this queue's
		// pool has to lie inside the region.
		if f := q.Region().Frame(packetio.Desc{Addr: 0, Len: 1}); f == nil {
			t.Fatalf("queue %d: region rejects its own first frame", i)
		}
	}

	tx, err := Open(a, WithTxQueues(1), WithRxQueues(0))
	if err != nil {
		t.Fatalf("open sender: %v", err)
	}
	defer tx.Close()

	// Many flows, so the fanout hash spreads them.
	const flows = 512
	txq := tx.TxQueue(0)
	for round := 0; round < 8; round++ {
		sent, err := txq.SendFunc(64, func(i int, buf []byte) int {
			f := frame(byte(i), 100)
			// Vary the last four bytes so the hash sees distinct flows.
			binary.BigEndian.PutUint32(f[len(f)-4:], uint32(round*64+i)%flows)
			return copy(buf, f)
		})
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if sent == 0 {
			t.Fatalf("round %d sent nothing", round)
		}
		txq.Complete(1 << 20)
	}

	// Give the kernel a moment, then count per queue.
	deadline := time.Now().Add(2 * time.Second)
	perQueue := make([]int, rx.NumRxQueues())
	total := 0
	for time.Now().Before(deadline) {
		for i := range perQueue {
			q := rx.RxQueue(i)
			ds := q.Receive(64)
			for _, d := range ds {
				if ours(q.Region().Frame(d)) {
					perQueue[i]++
					total++
				}
			}
			q.Recycle(ds)
		}
		if total >= 400 {
			break
		}
	}
	if total == 0 {
		t.Fatal("received nothing on any queue")
	}
	live := 0
	for _, n := range perQueue {
		if n > 0 {
			live++
		}
	}
	t.Logf("received %d packets, per queue: %v", total, perQueue)
	if live < 2 {
		t.Errorf("only %d of %d queues delivered anything (%v); "+
			"a fanout group where one queue takes everything is the pool-base bug",
			live, len(perQueue), perQueue)
	}
}
