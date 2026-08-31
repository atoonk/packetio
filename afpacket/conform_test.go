//go:build linux

package afpacket_test

import (
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/afpacket"
	"github.com/atoonk/packetio/internal/conform"
)

// TestConformance runs the shared backend suite against afpacket, on a veth
// pair so it needs no hardware. Without root it skips, like the other veth
// tests.
func TestConformance(t *testing.T) {
	conform.Run(t, func(t *testing.T) packetio.Device {
		if os.Geteuid() != 0 {
			t.Log("needs root to create a veth pair and open AF_PACKET sockets")
			return nil
		}
		// Opt-in, for the reason in veth_test.go: this creates and destroys
		// interfaces on the machine it runs on.
		if os.Getenv("PACKETIO_VETH_TESTS") == "" {
			t.Log("set PACKETIO_VETH_TESTS=1 to allow this test to create veth interfaces")
			return nil
		}
		name := "pioc" + strconv.Itoa(os.Getpid()%1000)
		if _, err := net.InterfaceByName(name); err == nil {
			t.Logf("an interface called %s already exists; not touching it", name)
			return nil
		}
		if out, err := exec.Command("ip", "link", "add", name, "type", "veth",
			"peer", "name", name+"p").CombinedOutput(); err != nil {
			t.Logf("cannot create a veth pair: %v: %s", err, out)
			return nil
		}
		t.Cleanup(func() { exec.Command("ip", "link", "del", name).Run() })
		for _, n := range []string{name, name + "p"} {
			exec.Command("sysctl", "-qw", "net.ipv6.conf."+n+".disable_ipv6=1").Run()
			exec.Command("ip", "link", "set", n, "up").Run()
		}
		d, err := afpacket.Open(name, afpacket.WithTxQueues(1), afpacket.WithRxQueues(1),
			afpacket.WithFrames(256))
		if err != nil {
			t.Logf("open: %v", err)
			return nil
		}
		return d
	})
}
