//go:build linux && cgo && mlx5 && (amd64 || arm64)

package main

import (
	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/mlx5"
)

func init() {
	backends["mlx5"] = func(iface string, f packetio.SteeringFilter) (packetio.Device, string, error) {
		d, err := mlx5.Open(iface,
			mlx5.WithTxQueues(0), mlx5.WithRxQueues(1),
			mlx5.WithFrames(4096), mlx5.WithSteering(f))
		if err != nil {
			return nil, "", err
		}
		return d, "hardware steering: " + d.Info().Steering, nil
	}
}
