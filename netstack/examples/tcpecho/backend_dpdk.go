//go:build linux && cgo && dpdk && (amd64 || arm64)

package main

import (
	"net"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/dpdk"
)

func init() { openers["dpdk"] = openDPDK }

// openDPDK opens a PCI device or a vdev. Where the kernel has no interface
// for it (a device bound to vfio-pci) the MAC is the device's own, from
// Info; the stack then does ARP for its address with no kernel beside it.
// On a device the kernel shares (a bifurcated card, or a vdev over a kernel
// interface) the port's own address filter is what selects traffic.
func openDPDK(c config) (packetio.Device, net.HardwareAddr, error) {
	opts := []dpdk.Option{dpdk.WithQueues(c.queues)}
	if c.direct {
		// Direct transmit sends from whatever goroutine produced the
		// packet, and these backends place the first goroutine to
		// transmit on the queue's CPU and leave it there.
		opts = append(opts, dpdk.WithoutAffinity())
	}
	if c.frames > 0 {
		opts = append(opts, dpdk.WithFrames(c.frames))
	}
	if c.mtu > 0 {
		opts = append(opts, dpdk.WithMTU(c.mtu))
	}
	if c.devargs != "" {
		opts = append(opts, dpdk.WithDevArgs(c.devargs))
	}
	if c.noHuge > 0 {
		opts = append(opts, dpdk.WithoutHugePages(c.noHuge), dpdk.WithFrameSize(4096))
	}
	dev, err := dpdk.Open(c.iface, opts...)
	if err != nil {
		return nil, nil, err
	}
	mac := dev.Info().MAC
	return dev, net.HardwareAddr(mac[:]), nil
}
