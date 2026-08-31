//go:build linux && cgo && mlx5 && (amd64 || arm64)

package mlx5_test

import (
	"os"
	"testing"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/internal/conform"
	"github.com/atoonk/packetio/mlx5"
)

// TestConformance runs the shared backend suite against mlx5. It needs a real
// ConnectX and the privilege to open it, so it skips almost everywhere;
// PACKETIO_IFACE names the interface to use.
func TestConformance(t *testing.T) {
	conform.Run(t, func(t *testing.T) packetio.Device {
		iface := os.Getenv("PACKETIO_IFACE")
		if iface == "" || os.Geteuid() != 0 {
			return nil
		}
		d, err := mlx5.Open(iface, mlx5.WithTxQueues(1), mlx5.WithRxQueues(1),
			mlx5.WithFrames(4096))
		if err != nil {
			t.Logf("open %s: %v", iface, err)
			return nil
		}
		return d
	})
}

func TestConformanceSteering(t *testing.T) {
	conform.RunSteering(t, func(t *testing.T, f packetio.SteeringFilter) (packetio.Device, error) {
		iface := os.Getenv("PACKETIO_IFACE")
		if iface == "" || os.Geteuid() != 0 {
			return nil, nil
		}
		return mlx5.Open(iface, mlx5.WithTxQueues(0), mlx5.WithRxQueues(1),
			mlx5.WithFrames(2048), mlx5.WithSteering(f))
	})
}
