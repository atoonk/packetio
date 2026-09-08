//go:build linux

package netstack

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The end-to-end tests run a stack over a real device on one end of a veth
// pair and a kernel TCP peer on the other, the peer in a network namespace of
// its own so the two never share a routing table. The near end has no
// address: the stack owns 10.99.0.1, answers ARP for it, and the kernel --
// which on afpacket and the dpdk af_packet vdev sees every frame too --
// leaves an address that is not its own alone. That is the topology every
// backend runs on, so ARP is exercised on all of them.
//
// Two things about veth that a physical NIC would not need: the namespace
// side has transmit checksum offload and segmentation turned off, because a
// veth hands its peer packets with checksums still to be filled in and
// segments still to be split, which the stack would reject.

const (
	labStackIP  = "10.99.0.1"
	labStackPfx = "10.99.0.1/24"
	labPeerIP   = "10.99.0.2"
)

// lab is a veth pair with the peer in its own namespace.
type lab struct {
	near, peer, ns string
	mac            net.HardwareAddr // the near end's, which the stack uses
}

// needLab skips unless the test may create interfaces: root, and an explicit
// opt-in, because a plain `sudo go test ./...` must not reconfigure someone's
// networking.
func needLab(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: this creates veth interfaces and opens raw sockets")
	}
	if os.Getenv("PACKETIO_VETH_TESTS") == "" {
		t.Skip("set PACKETIO_VETH_TESTS=1 to allow this test to create veth interfaces")
	}
	if os.Getenv("PACKETIO_IFACE") != "" || os.Getenv("PACKETIO_DPDK_DEV") != "" {
		t.Skip("this test opens its own device, and the environment is pinned to another")
	}
	for _, tool := range []string{"ip", "ethtool"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
}

func run(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v: %s", strings.Join(args, " "), err, out)
	}
}

// newLab builds the pair. Names come from the pid, so two runs on one
// machine do not collide, and an interface that already exists is not
// touched: it belongs to something else.
func newLab(t *testing.T) *lab {
	t.Helper()
	needLab(t)
	id := os.Getpid() % 1000
	l := &lab{near: fmt.Sprintf("pns%da", id), peer: fmt.Sprintf("pns%db", id), ns: fmt.Sprintf("pns%d", id)}
	for _, n := range []string{l.near, l.peer} {
		if _, err := net.InterfaceByName(n); err == nil {
			t.Skipf("an interface called %s already exists; not touching it", n)
		}
	}
	if out, err := exec.Command("ip", "link", "add", l.near, "type", "veth", "peer", "name", l.peer).CombinedOutput(); err != nil {
		t.Skipf("cannot create a veth pair: %v: %s", err, out)
	}
	t.Cleanup(func() { exec.Command("ip", "link", "del", l.near).Run() })
	exec.Command("ip", "netns", "del", l.ns).Run() // a leftover from a killed run
	run(t, "ip", "netns", "add", l.ns)
	t.Cleanup(func() { exec.Command("ip", "netns", "del", l.ns).Run() })

	// The kernel sends IPv6 duplicate-address-detection packets the moment
	// a link comes up; they are not this test's traffic.
	exec.Command("sysctl", "-qw", "net.ipv6.conf."+l.near+".disable_ipv6=1").Run()
	run(t, "ip", "link", "set", l.peer, "netns", l.ns)
	run(t, "ip", "-n", l.ns, "addr", "add", labPeerIP+"/24", "dev", l.peer)
	run(t, "ip", "-n", l.ns, "link", "set", "lo", "up")
	run(t, "ip", "-n", l.ns, "link", "set", l.peer, "up")
	run(t, "ip", "netns", "exec", l.ns, "sysctl", "-qw", "net.ipv6.conf."+l.peer+".disable_ipv6=1")
	run(t, "ip", "netns", "exec", l.ns, "ethtool", "-K", l.peer, "tx", "off", "tso", "off", "gso", "off")
	run(t, "ip", "link", "set", l.near, "up")

	ifi, err := net.InterfaceByName(l.near)
	if err != nil {
		t.Fatalf("interface %s: %v", l.near, err)
	}
	l.mac = ifi.HardwareAddr
	// A veth reports carrier once both ends are up; give it a moment.
	time.Sleep(100 * time.Millisecond)
	return l
}

// inNetns runs f on a thread inside the peer's namespace. A socket is bound
// to the namespace of the thread that creates it, so the goroutine is locked
// for the duration; what f returns works from any thread afterwards.
func (l *lab) inNetns(t *testing.T, f func() error) {
	t.Helper()
	runtime.LockOSThread()
	restored := false
	defer func() {
		// A thread left in the peer's namespace must not return to the
		// scheduler; keeping it locked makes the runtime discard it.
		if restored {
			runtime.UnlockOSThread()
		}
	}()
	orig, err := os.Open("/proc/self/ns/net")
	if err != nil {
		t.Fatalf("current netns: %v", err)
	}
	defer orig.Close()
	ns, err := os.Open("/var/run/netns/" + l.ns)
	if err != nil {
		t.Fatalf("netns %s: %v", l.ns, err)
	}
	defer ns.Close()
	if err := unix.Setns(int(ns.Fd()), unix.CLONE_NEWNET); err != nil {
		t.Fatalf("enter netns: %v", err)
	}
	defer func() { restored = unix.Setns(int(orig.Fd()), unix.CLONE_NEWNET) == nil }()
	if err := f(); err != nil {
		t.Fatal(err)
	}
}

// dial connects from the peer's namespace to the stack.
func (l *lab) dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	var c net.Conn
	l.inNetns(t, func() error {
		d := net.Dialer{Timeout: 5 * time.Second}
		var err error
		if c, err = d.Dial("tcp", addr); err != nil {
			return fmt.Errorf("dial %s from the namespace: %w", addr, err)
		}
		return nil
	})
	c.SetDeadline(time.Now().Add(20 * time.Second))
	return c
}

// listen opens a kernel listener in the peer's namespace, for the stack to
// dial.
func (l *lab) listen(t *testing.T, addr string) net.Listener {
	t.Helper()
	var ln net.Listener
	l.inNetns(t, func() error {
		var err error
		if ln, err = net.Listen("tcp", addr); err != nil {
			return fmt.Errorf("listen %s in the namespace: %w", addr, err)
		}
		return nil
	})
	return ln
}
