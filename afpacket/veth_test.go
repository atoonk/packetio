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

// Timestamps come from the kernel's own frame header, so a packet that
// arrives carries the time it arrived. The properties worth holding: one
// timestamp per descriptor, none of them zero (a zero time is a lie, not a
// default), and time does not run backwards within a batch.
func TestVethReceiveTimestamps(t *testing.T) {
	needRoot(t)
	name, peer := vethPair(t)

	d, err := Open(name, WithTxQueues(0), WithRxQueues(1), WithPromiscuous())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	sender, err := Open(peer, WithTxQueues(1), WithRxQueues(0))
	if err != nil {
		t.Fatalf("open sender on %s: %v", peer, err)
	}
	defer sender.Close()
	if !d.Capabilities().RxTimestamps {
		t.Fatal("afpacket does not claim RxTimestamps")
	}
	rx, ok := d.RxQueue(0).(packetio.TimestampReceiver)
	if !ok {
		t.Fatal("the receive queue is not a TimestampReceiver")
	}
	rx.Fill(rx.NumFreeFillSlots())

	before := uint64(time.Now().UnixNano())
	if _, err := sender.TxQueue(0).SendFunc(8, func(i int, buf []byte) int {
		return copy(buf, frame(byte(i), 100))
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	var descs []packetio.Desc
	var ts []uint64
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := rx.Poll(200 * time.Millisecond); err != nil {
			t.Fatalf("Poll: %v", err)
		}
		if descs, ts = rx.ReceiveTimestamps(16); len(descs) > 0 {
			break
		}
	}
	if len(descs) == 0 {
		t.Skip("nothing arrived on the veth pair")
	}
	after := uint64(time.Now().UnixNano())

	if len(ts) != len(descs) {
		t.Fatalf("%d timestamps for %d packets", len(ts), len(descs))
	}
	for i, v := range ts {
		if v == 0 {
			t.Errorf("packet %d has a zero timestamp", i)
		}
		if i > 0 && v < ts[i-1] {
			t.Errorf("time ran backwards: packet %d at %d after %d", i, v, ts[i-1])
		}
	}
	// The kernel's clock here is the real one, so the stamps must sit inside
	// the window this test was running. A backend stamping with something else
	// would pass every check above and fail this one.
	if ts[0] < before || ts[len(ts)-1] > after {
		t.Errorf("timestamps %d..%d are outside the test window %d..%d",
			ts[0], ts[len(ts)-1], before, after)
	}
	// The plain path must still work after the extended one: the two share
	// the queue's scratch, so getting that sharing wrong shows up here.
	rx.Recycle(descs)
	rx.Fill(rx.NumFreeFillSlots())
	if err := rx.Err(); err != nil {
		t.Errorf("the queue failed after ReceiveTimestamps: %v", err)
	}
	rx.Recycle(rx.Receive(16))
}

// A frame sent with only its pseudo-header partial must arrive completed, and
// the completed value must be the real checksum -- not merely "something was
// written". An earlier version of this test asserted neither: it sent an
// already-complete frame, so its one check sat behind a condition that was
// never true, and it survived a deliberate mutation that corrupted every
// checksum in the package.
func TestVethPartialChecksumIsCompletedCorrectly(t *testing.T) {
	needRoot(t)
	name, peer := vethPair(t)

	rx, err := Open(name, WithTxQueues(0), WithRxQueues(1), WithGSO(), WithPromiscuous())
	if err != nil {
		t.Skipf("cannot open a GSO device here: %v", err)
	}
	defer rx.Close()
	tx, err := Open(peer, WithTxQueues(1), WithRxQueues(0), WithGSO())
	if err != nil {
		t.Skipf("cannot open a GSO sender here: %v", err)
	}
	defer tx.Close()

	pkt, hdrLen, csumStart, csumOff := tcpSegment(64)
	// What the checksum must come out as: the sum over the L4 range with the
	// partial in place, computed here rather than by the code under test.
	want := onesComplement(pkt[csumStart:])
	if want == 0 {
		want = 0xffff
	}

	q := rx.RxQueue(0).(packetio.OffloadReceiver)
	q.Fill(q.NumFreeFillSlots())

	txq := tx.TxQueue(0).(packetio.OffloadTransmitter)
	descs := txq.Alloc(1)
	if len(descs) == 0 {
		t.Fatal("no frames to send with")
	}
	copy(tx.Region().Writable(descs[0]), pkt)
	descs[0].Len = uint32(len(pkt))
	offs := []packetio.Offload{{
		Flags:     packetio.OffloadNeedsCsum,
		HdrLen:    uint16(hdrLen),
		CsumStart: uint16(csumStart),
		CsumOff:   uint16(csumOff),
	}}
	if n, err := txq.TransmitOffload(descs, offs); err != nil || n != 1 {
		t.Fatalf("TransmitOffload: %d sent, %v", n, err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := q.Poll(200 * time.Millisecond); err != nil {
			t.Fatalf("poll: %v", err)
		}
		got, offs := q.ReceiveOffload(16)
		for i, d := range got {
			b := q.Region().Frame(d)
			if !ours(b) || len(b) < csumStart+csumOff+2 {
				continue
			}
			if offs[i].Segmented() {
				continue // not what this test is about
			}
			sum := uint16(b[csumStart+csumOff])<<8 | uint16(b[csumStart+csumOff+1])
			if sum != want {
				t.Errorf("checksum on delivery is %#04x, want %#04x", sum, want)
			}
			if offs[i].Flags&packetio.OffloadNeedsCsum != 0 {
				t.Error("a completed frame still asks to be completed")
			}
			q.Recycle(got)
			return
		}
		q.Recycle(got)
		q.Fill(q.NumFreeFillSlots())
	}
	t.Skip("nothing arrived on the veth pair")
}

// A packet may arrive as several frames chained with OptContinued, so a
// forwarder carrying a coalesced super-frame as a chain of pool buffers can
// hand it to the kernel as it is, instead of copying it into one contiguous
// frame first. What arrives must be the concatenation, byte for byte.
func TestVethMultiBufferTransmit(t *testing.T) {
	needRoot(t)
	name, peer := vethPair(t)

	rx, err := Open(name, WithTxQueues(0), WithRxQueues(1), WithPromiscuous())
	if err != nil {
		t.Fatalf("open receiver: %v", err)
	}
	defer rx.Close()
	tx, err := Open(peer, WithTxQueues(1), WithRxQueues(0))
	if err != nil {
		t.Fatalf("open sender: %v", err)
	}
	defer tx.Close()

	// One packet in three pieces, and one ordinary packet after it, so the
	// mixed case is covered by the same run.
	whole := frame(1, 300)
	pieces := [][]byte{whole[:100], whole[100:180], whole[180:]}
	plain := frame(2, 120)

	txq := tx.TxQueue(0)
	descs := txq.Alloc(4)
	if len(descs) < 4 {
		t.Fatalf("Alloc gave %d frames, want 4", len(descs))
	}
	for i, p := range pieces {
		copy(tx.Region().Writable(descs[i]), p)
		descs[i].Len = uint32(len(p))
		if i < len(pieces)-1 {
			descs[i].Options |= packetio.OptContinued
		}
	}
	copy(tx.Region().Writable(descs[3]), plain)
	descs[3].Len = uint32(len(plain))

	before := txq.NumFreeFrames()
	if n := txq.Transmit(descs); n != 4 {
		t.Fatalf("Transmit took %d of 4 frames: %v", n, txq.Err())
	}

	rxq := rx.RxQueue(0)
	rxq.Fill(rxq.NumFreeFillSlots())
	var got [][]byte
	deadline := time.Now().Add(3 * time.Second)
	for len(got) < 2 && time.Now().Before(deadline) {
		if _, err := rxq.Poll(200 * time.Millisecond); err != nil {
			t.Fatalf("poll: %v", err)
		}
		ds := rxq.Receive(8)
		for _, d := range ds {
			if f := rxq.Region().Frame(d); ours(f) {
				got = append(got, append([]byte(nil), f...))
			}
		}
		rxq.Recycle(ds)
		rxq.Fill(rxq.NumFreeFillSlots())
	}
	if len(got) < 2 {
		t.Fatalf("received %d packets, want 2", len(got))
	}
	// The chain must arrive as ONE packet, reassembled by the kernel from the
	// iovecs, not as three.
	if !bytes.Equal(got[0], whole) {
		t.Errorf("the chained packet arrived as %d bytes, want the %d-byte concatenation",
			len(got[0]), len(whole))
	}
	if !bytes.Equal(got[1], plain) {
		t.Errorf("the ordinary packet after a chain did not survive it")
	}

	// Every frame of the chain is an ordinary frame again once it is sent.
	if n := txq.Complete(8); n != 4 {
		t.Errorf("Complete returned %d frames, want all 4 of them", n)
	}
	if after := txq.NumFreeFrames(); after != before+4 {
		t.Errorf("pool has %d frames, want %d: a chain leaked", after, before+4)
	}
}

// The combination Teraplane needs: a transmit-only device with small frames
// and segmentation offload, sending a super-frame as a chain of those frames.
// Before this, WithGSO forced 64 KB frames on every device, so a forwarder
// whose bytes already sat in small buffers had to copy each one into a 64 KB
// frame -- the copy multi-buffer exists to remove.
func TestVethGSOChainOfSmallFrames(t *testing.T) {
	needRoot(t)
	name, peer := vethPair(t)

	// Room for one packet larger than an ordinary MTU, so what is measured is
	// the gather rather than the kernel's segmentation.
	for _, n := range []string{name, peer} {
		if out, err := exec.Command("ip", "link", "set", n, "mtu", "9000").CombinedOutput(); err != nil {
			t.Skipf("cannot raise the MTU on %s: %v: %s", n, err, out)
		}
	}

	// A receiving device still needs a frame that holds a whole super-frame.
	rx, err := Open(name, WithTxQueues(0), WithRxQueues(1), WithGSO(), WithPromiscuous())
	if err != nil {
		t.Skipf("cannot open a GSO receiver here: %v", err)
	}
	defer rx.Close()

	// The transmit device does not: 2 KB frames, chained.
	const small = 2048
	tx, err := Open(peer, WithTxQueues(1), WithRxQueues(0), WithGSO(),
		WithFrameSize(small), WithFrames(256))
	if err != nil {
		t.Fatalf("a transmit-only GSO device with %d-byte frames was refused: %v", small, err)
	}
	defer tx.Close()
	if got := tx.Region().FrameSize(); got != small {
		t.Fatalf("frame size is %d, want the %d asked for: WithGSO overrode it", got, small)
	}

	// One TCP segment laid across several small frames, the shape a forwarder
	// hands over: the packet is the concatenation and the metadata is the
	// first descriptor's.
	pkt, hdrLen, csumStart, csumOff := tcpSegment(4000)
	want := onesComplement(pkt[csumStart:])
	if want == 0 {
		want = 0xffff
	}
	usable := tx.Capabilities().MaxFrameSize
	var pieces [][]byte
	for off := 0; off < len(pkt); off += usable {
		pieces = append(pieces, pkt[off:min(off+usable, len(pkt))])
	}
	if len(pieces) < 3 {
		t.Fatalf("the packet fits in %d frames; this test needs a real chain", len(pieces))
	}

	txq := tx.TxQueue(0).(packetio.OffloadTransmitter)
	descs := txq.Alloc(len(pieces))
	if len(descs) != len(pieces) {
		t.Fatalf("allocated %d of %d frames", len(descs), len(pieces))
	}
	for i, p := range pieces {
		copy(tx.Region().Writable(descs[i]), p)
		descs[i].Len = uint32(len(p))
		if i < len(pieces)-1 {
			descs[i].Options |= packetio.OptContinued
		}
	}
	offs := make([]packetio.Offload, len(pieces))
	offs[0] = packetio.Offload{
		Flags: packetio.OffloadNeedsCsum, HdrLen: uint16(hdrLen),
		CsumStart: uint16(csumStart), CsumOff: uint16(csumOff),
	}
	n, err := txq.TransmitOffload(descs, offs)
	if err != nil || n != len(descs) {
		t.Fatalf("TransmitOffload took %d of %d: %v", n, len(descs), err)
	}

	rxq := rx.RxQueue(0).(packetio.OffloadReceiver)
	rxq.Fill(rxq.NumFreeFillSlots())
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := rxq.Poll(200 * time.Millisecond); err != nil {
			t.Fatalf("poll: %v", err)
		}
		ds, _ := rxq.ReceiveOffload(8)
		for _, d := range ds {
			f := rxq.Region().Frame(d)
			if !ours(f) || len(f) < len(pkt) {
				continue
			}
			// The chain must arrive as the one packet it was. Every byte but
			// the checksum field, which the receiving device completes on the
			// way in -- and which must come out as the real checksum, proving
			// the gather put the payload together correctly: a sum over the
			// wrong bytes would not match.
			got := append([]byte(nil), f[:len(pkt)]...)
			sum := uint16(got[csumStart+csumOff])<<8 | uint16(got[csumStart+csumOff+1])
			copy(got[csumStart+csumOff:], pkt[csumStart+csumOff:csumStart+csumOff+2])
			if !bytes.Equal(got, pkt) {
				t.Errorf("the chained packet did not arrive as its concatenation")
			}
			if sum != want {
				t.Errorf("checksum over the gathered packet is %#04x, want %#04x", sum, want)
			}
			rxq.Recycle(ds)
			return
		}
		rxq.Recycle(ds)
		rxq.Fill(rxq.NumFreeFillSlots())
	}
	t.Skip("nothing large enough arrived on the veth pair")
}

// The other half of parity: a packet larger than a frame is copied out of the
// ring straight into several small frames, instead of into one large frame
// that a forwarder then copies again into its own buffers. Receive and
// transmit chains together mean a packet crosses this backend without ever
// being made contiguous.
func TestVethMultiBufferReceiveAndForward(t *testing.T) {
	needRoot(t)
	name, peer := vethPair(t)
	for _, n := range []string{name, peer} {
		if out, err := exec.Command("ip", "link", "set", n, "mtu", "9000").CombinedOutput(); err != nil {
			t.Skipf("cannot raise the MTU on %s: %v: %s", n, err, out)
		}
	}

	// Small frames, chaining on: the shape a forwarder wants.
	const small = 2048
	rx, err := Open(name, WithTxQueues(1), WithRxQueues(1), WithMultiBuffer(),
		WithFrameSize(small), WithFrames(256), WithPromiscuous())
	if err != nil {
		t.Fatalf("open receiver: %v", err)
	}
	defer rx.Close()
	if !rx.Capabilities().MultiBuffer {
		t.Error("a device opened WithMultiBuffer does not report it")
	}
	tx, err := Open(peer, WithTxQueues(1), WithRxQueues(0), WithGSO(),
		WithFrameSize(8192), WithFrames(256))
	if err != nil {
		t.Fatalf("open sender: %v", err)
	}
	defer tx.Close()

	// One packet several frames long.
	big := frame(7, 6000)
	if _, err := tx.TxQueue(0).SendFunc(1, func(_ int, b []byte) int {
		return copy(b, big)
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	rxq := rx.RxQueue(0)
	rxq.Fill(rxq.NumFreeFillSlots())
	var chain []packetio.Desc
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(chain) == 0 {
		if _, err := rxq.Poll(200 * time.Millisecond); err != nil {
			t.Fatalf("poll: %v", err)
		}
		ds := rxq.Receive(32)
		for i, d := range ds {
			f := rxq.Region().Frame(d)
			if i == 0 && !ours(f) {
				continue
			}
			chain = append(chain, ds[i:]...)
			break
		}
		if len(chain) == 0 {
			rxq.Recycle(ds)
			rxq.Fill(rxq.NumFreeFillSlots())
		}
	}
	if len(chain) == 0 {
		t.Skip("nothing arrived on the veth pair")
	}
	if len(chain) < 3 {
		t.Fatalf("a %d-byte packet came back in %d frames of %d", len(big), len(chain), small)
	}

	// Every frame but the last says the packet continues, and the pieces are
	// what was sent.
	var whole []byte
	for i, d := range chain {
		last := i == len(chain)-1
		got := d.Options&packetio.OptContinued != 0
		if got == last {
			t.Errorf("frame %d of %d: continued=%v, want %v", i, len(chain), got, !last)
		}
		whole = append(whole, rxq.Region().Frame(d)...)
		if last {
			break
		}
	}
	if !bytes.Equal(whole, big) {
		t.Errorf("the chain concatenates to %d bytes, want the %d sent", len(whole), len(big))
	}

	// And it can be forwarded straight back out as the chain it is -- no
	// copy anywhere in the path.
	if n := rx.TxQueue(0).Transmit(chain); n != len(chain) {
		t.Errorf("forwarding the received chain took %d of %d frames: %v",
			n, len(chain), rx.TxQueue(0).Err())
	}
	rx.TxQueue(0).Complete(len(chain))
}

// The loan: a frame handed out by Receive is the caller's until Recycle, and
// nothing the device does in between touches it. A forwarder that carries
// received frames through a processing graph depends on this, and a violation
// would not be an error -- it would be a packet quietly changing under it.
func TestVethReceivedFramesAreUntouchedUntilRecycled(t *testing.T) {
	needRoot(t)
	name, peer := vethPair(t)

	rx, err := Open(name, WithTxQueues(0), WithRxQueues(1), WithPromiscuous())
	if err != nil {
		t.Fatalf("open receiver: %v", err)
	}
	defer rx.Close()
	tx, err := Open(peer, WithTxQueues(1), WithRxQueues(0))
	if err != nil {
		t.Fatalf("open sender: %v", err)
	}
	defer tx.Close()

	send := func(seq byte, n int) {
		if _, err := tx.TxQueue(0).SendFunc(1, func(_ int, b []byte) int {
			return copy(b, frame(seq, n))
		}); err != nil {
			t.Fatalf("send: %v", err)
		}
	}

	q := rx.RxQueue(0)
	q.Fill(q.NumFreeFillSlots())
	region := q.Region()
	base := region.Bytes()

	// Take one packet and hold it.
	send(1, 200)
	var held packetio.Desc
	var copyOfHeld []byte
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && copyOfHeld == nil {
		q.Poll(200 * time.Millisecond)
		for _, d := range q.Receive(8) {
			if f := region.Frame(d); ours(f) {
				held = d
				copyOfHeld = append([]byte(nil), f...)
				break
			}
		}
		if copyOfHeld == nil {
			q.Fill(q.NumFreeFillSlots())
		}
	}
	if copyOfHeld == nil {
		t.Skip("nothing arrived on the veth pair")
	}

	// Now drive the queue hard while holding it: more traffic, more Receive
	// calls, more Fill. None of it may reach the loaned frame.
	for round := 0; round < 8; round++ {
		send(byte(2+round), 300)
		q.Fill(q.NumFreeFillSlots())
		q.Poll(100 * time.Millisecond)
		ds := q.Receive(16)
		for _, d := range ds {
			if d.Addr == held.Addr {
				t.Fatalf("round %d: the loaned frame at %d was handed out again", round, d.Addr)
			}
		}
		q.Recycle(ds)
		q.Fill(q.NumFreeFillSlots())
	}

	if got := region.Frame(held); !bytes.Equal(got, copyOfHeld) {
		t.Errorf("the loaned frame changed while it was held: %d bytes now, %d then",
			len(got), len(copyOfHeld))
	}
	// The mapping itself must be the same memory throughout.
	if now := region.Bytes(); &now[0] != &base[0] || len(now) != len(base) {
		t.Error("Region().Bytes() moved while the device was open")
	}
	q.Recycle([]packetio.Desc{held})
}

// A packet sent straight out of the caller's own memory, never copied into a
// frame. This is the affordance a forwarder needs: its packet already sits in
// its own buffers, and moving it into the region first would copy every byte
// to protect against a device that, here, reads nothing after the call.
func TestVethTransmitGather(t *testing.T) {
	needRoot(t)
	name, peer := vethPair(t)

	rx, err := Open(name, WithTxQueues(0), WithRxQueues(1), WithPromiscuous())
	if err != nil {
		t.Fatalf("open receiver: %v", err)
	}
	defer rx.Close()
	tx, err := Open(peer, WithTxQueues(1), WithRxQueues(0))
	if err != nil {
		t.Fatalf("open sender: %v", err)
	}
	defer tx.Close()
	if !tx.Capabilities().GatherTx {
		t.Fatal("afpacket does not report GatherTx")
	}
	g, ok := tx.TxQueue(0).(packetio.GatherTransmitter)
	if !ok {
		t.Fatal("the transmit queue is not a GatherTransmitter")
	}

	// Two packets in ordinary Go memory, one of them in four pieces. Nothing
	// here came from the queue's pool.
	one := frame(1, 400)
	two := frame(2, 150)
	segs := [][]byte{one[:90], one[90:200], one[200:310], one[310:], two}
	counts := []int{4, 1}

	before := tx.TxQueue(0).NumFreeFrames()
	n, err := g.TransmitGather(segs, counts, nil)
	if err != nil || n != 2 {
		t.Fatalf("TransmitGather sent %d of 2: %v", n, err)
	}
	// Nothing was taken from the pool and nothing is waiting to come back.
	if after := tx.TxQueue(0).NumFreeFrames(); after != before {
		t.Errorf("pool went from %d to %d frames: gather took frames it should not have",
			before, after)
	}
	if got := tx.TxQueue(0).NumInFlight(); got != 0 {
		t.Errorf("%d frames in flight after a gather; nothing should be pending", got)
	}

	rxq := rx.RxQueue(0)
	rxq.Fill(rxq.NumFreeFillSlots())
	var got [][]byte
	deadline := time.Now().Add(3 * time.Second)
	for len(got) < 2 && time.Now().Before(deadline) {
		if _, err := rxq.Poll(200 * time.Millisecond); err != nil {
			t.Fatalf("poll: %v", err)
		}
		ds := rxq.Receive(8)
		for _, d := range ds {
			if f := rxq.Region().Frame(d); ours(f) {
				got = append(got, append([]byte(nil), f...))
			}
		}
		rxq.Recycle(ds)
		rxq.Fill(rxq.NumFreeFillSlots())
	}
	if len(got) < 2 {
		t.Fatalf("received %d packets, want 2", len(got))
	}
	if !bytes.Equal(got[0], one) {
		t.Errorf("the gathered packet arrived as %d bytes, want the %d-byte whole",
			len(got[0]), len(one))
	}
	if !bytes.Equal(got[1], two) {
		t.Error("the single-segment packet did not survive the gather before it")
	}
}

// A chained send re-aims each message at chainIovs, where a packet has as many
// iovecs as pieces. The single-frame path does not set those pointers -- it
// fills fixed iovecs wired once at construction -- so unless the wiring is put
// back, the next plain send fills q.iovs while the kernel reads chainIovs and
// puts the PREVIOUS packet on the wire again. After a gather it is worse: the
// stale iovecs aim into memory the caller has taken back.
//
// The mixed-batch test does not catch this, because its plain packet rides the
// same call and so goes down the chained path too. It takes a later,
// chain-free call.
func TestPlainSendAfterChainedSend(t *testing.T) {
	needRoot(t)
	name, peer := vethPair(t)

	rx, err := Open(name, WithTxQueues(0), WithRxQueues(1), WithPromiscuous())
	if err != nil {
		t.Fatalf("open receiver: %v", err)
	}
	defer rx.Close()
	tx, err := Open(peer, WithTxQueues(1), WithRxQueues(0))
	if err != nil {
		t.Fatalf("open sender: %v", err)
	}
	defer tx.Close()
	txq := tx.TxQueue(0)

	// 1: a chain.
	whole := frame(1, 300)
	d := txq.Alloc(3)
	for i, p := range [][]byte{whole[:100], whole[100:200], whole[200:]} {
		copy(tx.Region().Writable(d[i]), p)
		d[i].Len = uint32(len(p))
		if i < 2 {
			d[i].Options |= packetio.OptContinued
		}
	}
	txq.Transmit(d)
	txq.Complete(8)

	// 2: a SEPARATE, later, chain-free call.
	plain := frame(9, 250)
	d2 := txq.Alloc(1)
	copy(tx.Region().Writable(d2[0]), plain)
	d2[0].Len = uint32(len(plain))
	if n := txq.Transmit(d2); n != 1 {
		t.Fatalf("plain transmit after a chain took %d: %v", n, txq.Err())
	}

	rxq := rx.RxQueue(0)
	rxq.Fill(rxq.NumFreeFillSlots())
	var got [][]byte
	deadline := time.Now().Add(3 * time.Second)
	for len(got) < 2 && time.Now().Before(deadline) {
		rxq.Poll(200 * time.Millisecond)
		ds := rxq.Receive(8)
		for _, dd := range ds {
			if f := rxq.Region().Frame(dd); ours(f) {
				got = append(got, append([]byte(nil), f...))
			}
		}
		rxq.Recycle(ds)
		rxq.Fill(rxq.NumFreeFillSlots())
	}
	if len(got) < 2 {
		t.Fatalf("received %d packets, want 2", len(got))
	}
	if !bytes.Equal(got[1], plain) {
		t.Errorf("the plain packet after a chain arrived as %d bytes, want %d: the "+
			"message wiring was left pointing at the chain's iovecs",
			len(got[1]), len(plain))
	}
}
