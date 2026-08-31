//go:build linux && cgo && dpdk && amd64

package dpdk_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/dpdk"
)

func needRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: this maps device memory and, on a real NIC, hugepages")
	}
}

// The environment is started once per process and devices are hot-plugged in
// and out of it. A program that opens and closes in a loop must therefore not
// run out of ports, memzones or hugepages -- and the way it would is by losing
// the device handle at close, which is exactly what happened the first time.
func TestOpenCloseManyTimes(t *testing.T) {
	needRoot(t)
	const rounds = 50
	for i := 0; i < rounds; i++ {
		d, err := open(t)
		if err != nil {
			t.Fatalf("open %d of %d: %v", i+1, rounds, err)
		}
		if got := d.Info().Port; got < 0 {
			t.Fatalf("round %d: port %d", i, got)
		}
		if err := d.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
}

// Close is idempotent, and a device that never finished opening is closed by
// the same path.
func TestCloseTwiceAndHalfOpen(t *testing.T) {
	needRoot(t)
	d, err := open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}

	// An Open that fails part-way must leave nothing behind: if it leaked the
	// port or the region, the next Open would fail too.
	if _, err := open(t, dpdk.WithTxQueues(1<<20)); err == nil {
		t.Error("a device with a million queues was accepted")
	}
	d2, err := open(t)
	if err != nil {
		t.Fatalf("opening after a failed open: %v", err)
	}
	d2.Close()
}

// Two devices on one port cannot both exist: the second must be refused rather
// than quietly reconfiguring the first out from under it.
func TestSecondOpenOfTheSameDeviceIsRefused(t *testing.T) {
	needRoot(t)
	d, err := open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	d2, err := open(t)
	if err == nil {
		d2.Close()
		t.Fatal("the same device was opened twice at once")
	}
}

// A goroutine blocked in Poll must be woken by Close, or a program using this
// backend can never shut down. It is also where a backend that calls the driver
// on a stopped port faults, which no race detector catches.
func TestCloseWakesAPollerRepeatedly(t *testing.T) {
	needRoot(t)
	for i := 0; i < 10; i++ {
		d, err := open(t)
		if err != nil {
			t.Fatal(err)
		}
		rx := d.RxQueue(0)
		if rx == nil {
			d.Close()
			t.Skip("no receive queue")
		}
		done := make(chan error, 1)
		go func() {
			_, err := rx.Poll(10 * time.Second)
			done <- err
		}()
		time.Sleep(20 * time.Millisecond)
		if err := d.Close(); err != nil {
			t.Fatalf("close while polling: %v", err)
		}
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, packetio.ErrClosed) {
				t.Fatalf("Poll returned %v, want nil or ErrClosed", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Poll did not return after Close")
		}
	}
}

// An error from the environment must carry what the environment said. Without
// it a refusal is a bare code and the caller has nothing to act on.
func TestOpenErrorsExplainThemselves(t *testing.T) {
	needRoot(t)
	for _, c := range []struct{ name, dev, want string }{
		{"no such interface", "definitely-not-an-interface", "no interface named"},
		{"no such virtual device", "net_nosuchdriver0", "opening"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, err := dpdk.Open(c.dev, dpdk.WithoutHugePages(256))
			if err == nil {
				d.Close()
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

// A device still held by a kernel driver is refused with the command that would
// hand it over, and this library never rebinds anything itself.
func TestKernelBoundDeviceIsRefusedWithAdvice(t *testing.T) {
	iface := kernelBoundInterface(t)
	if iface == "" {
		t.Skip("no interface bound to a driver this backend cannot share")
	}
	_, err := dpdk.Open(iface)
	if err == nil {
		t.Fatalf("%s was opened although a kernel driver has it", iface)
	}
	for _, want := range []string{"dpdk-devbind.py", "vfio-pci", "off the network"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// The refusal must fire for a PCI address as well as an interface name. The
// documentation and every example open devices by address, and that form used
// to return before the driver check -- so the caller who followed the docs got
// the driver's bare "Operation not supported" instead of the command that
// fixes it.
func TestKernelBoundPCIAddressIsRefusedWithAdvice(t *testing.T) {
	iface := kernelBoundInterface(t)
	if iface == "" {
		t.Skip("no interface bound to a driver this backend cannot share")
	}
	link, err := os.Readlink(filepath.Join("/sys/class/net", iface, "device"))
	if err != nil {
		t.Skipf("%s has no device behind it", iface)
	}
	addr := filepath.Base(link)
	if _, err := os.Stat(filepath.Join("/sys/bus/pci/devices", addr)); err != nil {
		// A USB or platform NIC: it has a device but not a PCI address, and
		// this test is about the PCI form of Open.
		t.Skipf("%s is not a PCI device (%s)", iface, addr)
	}

	d, err := dpdk.Open(addr, dpdk.WithTxQueues(1))
	if err == nil {
		d.Close()
		t.Fatalf("%s was opened although a kernel driver has it", addr)
	}
	for _, want := range []string{"dpdk-devbind.py", addr, iface} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so it does not say what to run "+
				"or what goes off the network:\n%s", want, err)
		}
	}
}

// An interface with no PCI address behind it -- a USB or platform NIC -- is
// refused in its own words. Telling someone to rebind it with devbind, which
// is what happened before, is advice that cannot be followed.
func TestNonPCIInterfaceIsRefusedInItsOwnWords(t *testing.T) {
	iface := nonPCIInterface(t)
	if iface == "" {
		t.Skip("no non-PCI interface on this machine")
	}
	d, err := dpdk.Open(iface, dpdk.WithTxQueues(1))
	if err == nil {
		d.Close()
		t.Fatalf("%s was opened although it has no PCI address", iface)
	}
	if !strings.Contains(err.Error(), "not a PCI device") {
		t.Errorf("the refusal does not say why:\n%s", err)
	}
	if strings.Contains(err.Error(), "devbind") {
		t.Errorf("the refusal offers devbind, which cannot help here:\n%s", err)
	}
}

// nonPCIInterface finds an interface with a device that is not a PCI device.
func nonPCIInterface(t *testing.T) string {
	t.Helper()
	ents, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return ""
	}
	for _, e := range ents {
		link, err := os.Readlink("/sys/class/net/" + e.Name() + "/device")
		if err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join("/sys/bus/pci/devices", filepath.Base(link))); err != nil {
			return e.Name()
		}
	}
	return ""
}

// kernelBoundInterface finds a PCI interface held by a driver this backend
// cannot share, without touching it.
//
// PCI only: a USB or platform NIC has a kernel driver too, but no PCI address,
// and "hand it over with dpdk-devbind" is not advice that means anything for
// one. Open says so in its own words, which is a different message and a
// different test from this one.
func kernelBoundInterface(t *testing.T) string {
	t.Helper()
	ents, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return ""
	}
	for _, e := range ents {
		link, err := os.Readlink("/sys/class/net/" + e.Name() + "/device")
		if err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join("/sys/bus/pci/devices", filepath.Base(link))); err != nil {
			continue // not a PCI device
		}
		drv, err := os.Readlink("/sys/class/net/" + e.Name() + "/device/driver")
		if err != nil {
			continue
		}
		switch base := drv[strings.LastIndex(drv, "/")+1:]; base {
		case "vfio-pci", "uio_pci_generic", "igb_uio", "mlx5_core", "mlx4_core":
			continue
		default:
			return e.Name()
		}
	}
	return ""
}

// The options are checked before anything is opened, so a mistake says what it
// was rather than surfacing as whatever the driver complained about first.
func TestOptionValidation(t *testing.T) {
	for _, c := range []struct {
		name string
		opts []dpdk.Option
	}{
		{"no queues at all", []dpdk.Option{dpdk.WithTxQueues(0), dpdk.WithRxQueues(0)}},
		{"a transmit depth that is not a power of two", []dpdk.Option{dpdk.WithTxDepth(1000)}},
		{"a receive depth of zero", []dpdk.Option{dpdk.WithRxQueues(1), dpdk.WithRxDepth(0)}},
		{"a frame size that is not a power of two", []dpdk.Option{dpdk.WithFrameSize(1500)}},
		{"a frame too small to hold an mbuf", []dpdk.Option{dpdk.WithFrameSize(256)}},
		{"more queues than this backend opens", []dpdk.Option{dpdk.WithTxQueues(999)}},
		{"contradictory affinity", []dpdk.Option{dpdk.WithAffinity(0), dpdk.WithoutAffinity()}},
		{"a steering filter with no receive queues", []dpdk.Option{
			dpdk.WithRxQueues(0), dpdk.WithTxQueues(1),
			dpdk.WithSteering(packetio.SteeringFilter{Promiscuous: true})}},
		{"promiscuous together with a match", []dpdk.Option{
			dpdk.WithRxQueues(1),
			dpdk.WithSteering(packetio.SteeringFilter{
				Promiscuous: true,
				Match:       []packetio.Match{packetio.MatchVLAN(100)},
			})}},
		{"an MTU that cannot fit a frame", []dpdk.Option{dpdk.WithMTU(9000), dpdk.WithFrameSize(2048)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			// These are refused before any device is touched, so they need no
			// hardware and no root.
			d, err := dpdk.Open("net_null9", append(c.opts, dpdk.WithoutHugePages(256))...)
			if err == nil {
				d.Close()
				t.Error("accepted")
			}
		})
	}
}

// Real bytes, over a veth pair: what one end sends the other receives, with the
// frame intact. net_null proves the ownership rules but never carries a packet,
// and a backend can satisfy every rule and still put nothing on the wire.
func TestSendAndReceiveOverVeth(t *testing.T) {
	needRoot(t)
	if os.Getenv("PACKETIO_VETH_TESTS") == "" {
		t.Skip("set PACKETIO_VETH_TESTS=1 to allow this test to create veth interfaces")
	}
	if os.Getenv("PACKETIO_DPDK_DEV") != "" || os.Getenv("PACKETIO_IFACE") != "" {
		t.Skip("this test opens its own device, and the environment is pinned to another")
	}

	name := "piod" + strconv.Itoa(os.Getpid()%1000)
	peer := name + "p"
	if out, err := exec.Command("ip", "link", "add", name, "type", "veth",
		"peer", "name", peer).CombinedOutput(); err != nil {
		t.Skipf("cannot create a veth pair: %v: %s", err, out)
	}
	t.Cleanup(func() { exec.Command("ip", "link", "del", name).Run() })
	for _, n := range []string{name, peer} {
		exec.Command("sysctl", "-qw", "net.ipv6.conf."+n+".disable_ipv6=1").Run()
		if out, err := exec.Command("ip", "link", "set", n, "up").CombinedOutput(); err != nil {
			t.Skipf("cannot bring %s up: %v: %s", n, err, out)
		}
	}

	// The af_packet driver needs a data room big enough for its ring frame,
	// which is larger than the default frame leaves.
	tx, err := dpdk.Open("net_af_packet0,iface="+name,
		dpdk.WithoutHugePages(256), dpdk.WithTxQueues(1), dpdk.WithRxQueues(1),
		dpdk.WithFrameSize(4096), dpdk.WithFrames(1024),
		dpdk.WithTxDepth(256), dpdk.WithRxDepth(256))
	if err != nil {
		t.Skipf("cannot open the af_packet driver on %s: %v", name, err)
	}
	defer tx.Close()

	rx, err := dpdk.Open("net_af_packet1,iface="+peer,
		dpdk.WithTxQueues(1), dpdk.WithRxQueues(1),
		dpdk.WithFrameSize(4096), dpdk.WithFrames(1024),
		dpdk.WithTxDepth(256), dpdk.WithRxDepth(256), dpdk.WithPromiscuous())
	if err != nil {
		t.Skipf("cannot open the af_packet driver on %s: %v", peer, err)
	}
	defer rx.Close()

	const packets = 32
	want := frame(64)
	sent, err := tx.TxQueue(0).SendFunc(packets, func(i int, b []byte) int {
		return copy(b, want)
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if sent == 0 {
		t.Fatal("nothing was sent")
	}

	rq := rx.RxQueue(0)
	got, deadline := 0, time.Now().Add(5*time.Second)
	for got < sent && time.Now().Before(deadline) {
		rq.Fill(rq.NumFreeFillSlots())
		if n, err := rq.Poll(200 * time.Millisecond); err != nil {
			t.Fatalf("poll: %v", err)
		} else if n == 0 {
			continue
		}
		descs := rq.Receive(64)
		for _, d := range descs {
			b := rq.Region().Frame(d)
			if len(b) < len(want) {
				t.Errorf("received %d bytes, want at least %d", len(b), len(want))
				continue
			}
			if string(b[:len(want)]) != string(want) {
				t.Fatalf("the frame came back changed:\n got % x\nwant % x",
					b[:min(len(b), 32)], want[:32])
			}
			got++
		}
		rq.Recycle(descs)
	}
	if got == 0 {
		st, _ := rx.PortStats()
		t.Fatalf("sent %d frames and received none (the port counted %d in, %d missed, "+
			"%d without a buffer)", sent, st.InPackets, st.Missed, st.NoBuffer)
	}
	t.Logf("sent %d, received %d over the veth pair", sent, got)

	// Nothing may have been lost or double-freed along the way.
	ts, _ := tx.TxQueue(0).Stats()
	rs, _ := rq.Stats()
	for name, v := range map[string]uint64{
		"transmit pool_rejected": ts.Backend["pool_rejected"],
		"transmit mempool_drops": ts.Backend["mempool_drops"],
		"transmit foreign":       ts.Backend["foreign"],
		"receive pool_rejected":  rs.Backend["pool_rejected"],
		"receive mempool_drops":  rs.Backend["mempool_drops"],
		"receive foreign":        rs.Backend["foreign"],
		"receive chained":        rs.Backend["chained"],
	} {
		if v != 0 {
			t.Errorf("%s is %d, want 0", name, v)
		}
	}
}

// frame builds a minimal Ethernet frame of n bytes, with a payload that would
// show a truncation or an off-by-one in the layout arithmetic.
func frame(n int) []byte {
	b := make([]byte, n)
	copy(b[0:6], []byte{0x02, 0, 0, 0, 0, 0x02})
	copy(b[6:12], []byte{0x02, 0, 0, 0, 0, 0x01})
	b[12], b[13] = 0x88, 0xb5 // an EtherType reserved for experiments
	for i := 14; i < n; i++ {
		b[i] = byte(i)
	}
	return b
}

// The device reports what it is, and the report has to be true: a caller
// branches on Coexists to decide whether steering can promise anything about
// the host's traffic.
func TestInfoIsTruthful(t *testing.T) {
	needRoot(t)
	d, err := open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	i := d.Info()
	if i.Driver == "" {
		t.Error("no driver name")
	}
	if i.FrameSize <= 0 || i.MaxPacket <= 0 || i.MaxPacket >= i.FrameSize {
		t.Errorf("a %d-byte frame reports %d bytes of packet", i.FrameSize, i.MaxPacket)
	}
	if i.RegionVA == 0 {
		t.Error("no region address")
	}
	if i.IOVAMode != "physical" && i.IOVAMode != "virtual" {
		t.Errorf("IOVA mode %q", i.IOVAMode)
	}
	if i.Steering == "" {
		t.Error("the device does not say what it is taking")
	}
	// A virtual device has no kernel interface behind it, so it cannot claim
	// to be sharing one.
	if strings.HasPrefix(i.Device, "net_") && i.Coexists {
		t.Errorf("%s claims the kernel still has an interface for it", i.Device)
	}

	c := d.Capabilities()
	if c.Backend != "dpdk" {
		t.Errorf("Backend is %q", c.Backend)
	}
	if c.MaxFrameSize != i.MaxPacket {
		t.Errorf("Capabilities says %d bytes and Info says %d", c.MaxFrameSize, i.MaxPacket)
	}
	if !c.HandsBackFrames {
		t.Error("HandsBackFrames is false, but Reclaim does hand frames back")
	}
	if c.BlockingPoll {
		t.Error("BlockingPoll is true, but no interrupt is armed")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = fmt.Sprint
