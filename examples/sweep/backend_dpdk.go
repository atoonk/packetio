//go:build linux && cgo && dpdk && amd64

package main

import (
	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/dpdk"
)

func init() { openers["dpdk"] = openDPDK }

func openDPDK(c config) (packetio.Device, error) {
	tx, rx := c.queues, c.queues
	switch c.mode {
	case "tx":
		rx = 0
	case "rx":
		tx = 0
	}
	opts := []dpdk.Option{}
	if len(c.cpus) > 0 {
		opts = append(opts, dpdk.WithAffinity(c.cpus...))
	}
	if c.devargs != "" {
		opts = append(opts, dpdk.WithDevArgs(c.devargs))
	}
	return dpdk.Open(c.dev, append(opts,
		dpdk.WithTxQueues(tx), dpdk.WithRxQueues(rx),
		dpdk.WithTxDepth(c.depth), dpdk.WithRxDepth(c.depth),
		dpdk.WithFrames(nextPow2((tx+rx)*c.depth*4)),
		dpdk.WithPromiscuous())...)
}
