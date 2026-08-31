//go:build linux && cgo && mlx5

package main

import (
	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/mlx5"
)

func init() { openers["mlx5"] = openMLX5 }

func openMLX5(c config) (packetio.Device, error) {
	tx, rx := c.queues, c.queues
	switch c.mode {
	case "tx":
		rx = 0
	case "rx":
		tx = 0
	}
	opts := []mlx5.Option{
		mlx5.WithTxQueues(tx), mlx5.WithRxQueues(rx),
		mlx5.WithTxDepth(c.depth), mlx5.WithRxDepth(c.depth),
	}
	if c.rings > 0 {
		opts = append(opts, mlx5.WithRingsPerQueue(c.rings))
	}
	if c.compEvery > 0 {
		opts = append(opts, mlx5.WithCompletionEvery(c.compEvery))
	}
	if c.multiPacket {
		// Copied descriptors: ~17 Mpps per queue, but the ceiling multiplies
		// by queue count, which is the only way to 64-byte line rate.
		opts = append(opts, mlx5.WithMultiPacket(192))
	}
	if len(c.cpus) > 0 {
		opts = append(opts, mlx5.WithAffinity(c.cpus...))
	}
	if c.vlan != 0 {
		opts = append(opts, mlx5.WithEthernetHeaderLen(mlx5.EthernetHeaderLen(1)))
	}
	return mlx5.Open(c.dev, opts...)
}
