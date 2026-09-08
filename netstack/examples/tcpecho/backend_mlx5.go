//go:build linux && cgo && mlx5 && (amd64 || arm64)

package main

import (
	"net"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/mlx5"
)

func init() { openers["mlx5"] = openMLX5 }

// openMLX5 steers a whole VLAN -- every frame to this port's MAC or to
// broadcast -- to the stack's queues, so the stack owns its address on that
// VLAN and does ARP itself; the kernel keeps the rest of the port. Without
// -vlan the same rule would take the port's own traffic away from the
// kernel; on an untagged link the stack shares the kernel's address and only
// the TCP port is steered.
func openMLX5(c config) (packetio.Device, net.HardwareAddr, error) {
	mac, err := ifaceMAC(c.iface)
	if err != nil {
		return nil, nil, err
	}
	f := packetio.SteeringFilter{}
	switch {
	case c.vlan > 0:
		f.Match = append(f.Match, packetio.MatchVLAN(uint16(c.vlan)),
			packetio.MatchDstMAC([6]byte(mac)), packetio.MatchDstMAC([6]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}))
	case c.client != "":
		f.Match = append(f.Match, packetio.MatchSrcPort(packetio.IPProtoTCP, uint16(c.peer))...)
	default:
		f.Match = append(f.Match, packetio.MatchDstPort(packetio.IPProtoTCP, uint16(c.port))...)
	}
	opts := []mlx5.Option{mlx5.WithQueues(c.queues), mlx5.WithSteering(f)}
	if c.direct {
		// Direct transmit sends from whatever goroutine produced the
		// packet, and these backends place the first goroutine to
		// transmit on the queue's CPU and leave it there.
		opts = append(opts, mlx5.WithoutAffinity())
	}
	if c.vlan > 0 {
		opts = append(opts, mlx5.WithEthernetHeaderLen(mlx5.EthernetHeaderLen(1)))
	}
	if c.frames > 0 {
		opts = append(opts, mlx5.WithFrames(c.frames))
	}
	dev, err := mlx5.Open(c.iface, opts...)
	return dev, mac, err
}
