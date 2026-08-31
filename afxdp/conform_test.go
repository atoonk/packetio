//go:build linux

package afxdp_test

import (
	"os"
	"testing"

	xdp "github.com/atoonk/go-afxdp"
	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/afxdp"
	"github.com/atoonk/packetio/internal/conform"
)

// TestConformance runs the shared backend suite against AF_XDP. It attaches an
// XDP program, so it needs root and a real interface; PACKETIO_IFACE names it.
func TestConformance(t *testing.T) {
	conform.Run(t, func(t *testing.T) packetio.Device {
		iface := os.Getenv("PACKETIO_IFACE")
		if iface == "" || os.Geteuid() != 0 {
			return nil
		}
		// A filter that matches nothing. go-afxdp refuses to open without
		// one, so this suite used to call Open with no options, always fail,
		// and skip -- which is how every bug it exists to catch went unnoticed.
		// Matching nothing is also the only safe choice: PACKETIO_IFACE may be
		// a live interface, and this test must not take its traffic from the
		// kernel.
		d, err := afxdp.Open(iface, afxdp.WithXDP(xdp.WithFilter(xdp.MatchNone())))
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
		if err := func() error {
			_, err := afxdp.SteeringOption(f)
			return err
		}(); err != nil {
			return nil, err
		}
		return afxdp.Open(iface, afxdp.WithSteering(f))
	})
}
