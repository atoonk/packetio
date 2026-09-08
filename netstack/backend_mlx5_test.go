//go:build linux && cgo && mlx5 && (amd64 || arm64)

package netstack

import (
	"context"
	"net"
	"net/netip"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/mlx5"
)

// The mlx5 backend needs a ConnectX, and a ConnectX has no veth, so its test
// runs on a rig: this box's interface on one end, a kernel echo service on
// another box at the other. The stack takes the whole VLAN -- every frame
// to this port's MAC or to broadcast -- so it owns its address outright and
// does ARP itself, exactly as on the veth; the kernel, which has no
// interface on that VLAN, sees none of it.
//
//	PACKETIO_IFACE=eno2 PACKETIO_NETSTACK_VLAN=2043 \
//	PACKETIO_NETSTACK_ADDR=192.168.43.1/24 PACKETIO_NETSTACK_PEER=192.168.43.2:9090 \
//	sudo -E go test -tags mlx5 -run TestRigDial ./netstack
//
// with, on the peer, something that echoes: `socat TCP-LISTEN:9090,fork,reuseaddr EXEC:cat`.
func TestRigDial(t *testing.T) {
	iface := os.Getenv("PACKETIO_IFACE")
	addr := os.Getenv("PACKETIO_NETSTACK_ADDR")
	peer := os.Getenv("PACKETIO_NETSTACK_PEER")
	if iface == "" || addr == "" || peer == "" {
		t.Skip("set PACKETIO_IFACE, PACKETIO_NETSTACK_ADDR and PACKETIO_NETSTACK_PEER to run on a rig")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	prefix, err := netip.ParsePrefix(addr)
	if err != nil {
		t.Fatalf("PACKETIO_NETSTACK_ADDR: %v", err)
	}
	vlan, _ := strconv.Atoi(os.Getenv("PACKETIO_NETSTACK_VLAN"))
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		t.Fatalf("interface %s: %v", iface, err)
	}
	mac := ifi.HardwareAddr
	f := packetio.SteeringFilter{Match: []packetio.Match{
		packetio.MatchDstMAC([6]byte(mac)), packetio.MatchDstMAC(broadcastMAC),
	}}
	if vlan > 0 {
		f.Match = append(f.Match, packetio.MatchVLAN(uint16(vlan)))
	}
	opts := []mlx5.Option{mlx5.WithQueues(1), mlx5.WithSteering(f)}
	if vlan > 0 {
		opts = append(opts, mlx5.WithEthernetHeaderLen(mlx5.EthernetHeaderLen(1)))
	}
	dev, err := mlx5.Open(iface, opts...)
	if err != nil {
		if required() {
			t.Fatalf("mlx5: %v", err)
		}
		t.Skipf("mlx5: %v", err)
	}
	defer dev.Close()
	st, err := New(dev, Config{Addr: prefix, MAC: mac, VLAN: uint16(vlan)})
	if err != nil {
		t.Fatalf("netstack.New: %v", err)
	}
	defer st.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := st.Dial(ctx, "tcp", peer)
	if err != nil {
		t.Fatalf("dial %s: %v", peer, err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(30 * time.Second))
	bulkEcho(t, c, 16<<20)
	c.Close()
	checkStats(t, st)
}
