//go:build linux && cgo && mlx5 && (amd64 || arm64)

package mlx5

import (
	"fmt"

	"github.com/atoonk/packetio/internal/affinity"
)

// Where this backend's workers run is decided by internal/affinity, which is
// shared with the other polling backends because the reasoning is not specific
// to Direct Verbs: workers are packed into as few cache complexes as possible,
// on the memory node the card is attached to, because they share the
// write-combining page their doorbells are written through. The measurement
// behind that -- and it is worth a third of the transmit rate -- is in that
// package's doc.
//
// Errors are wrapped here rather than there, so that a caller reading
// "mlx5: processor 99 is outside what this process may run on" still learns
// which backend refused, while the same text serves any other backend.

// placement is the order in which processors are handed to this device's
// workers.
type placement = affinity.Placement

// newPlacement works out where this device's workers should run, or returns
// nil when the machine offers nothing to place them on.
func newPlacement(ifname string, want []int) (*placement, error) {
	p, err := affinity.New(ifname, want)
	if err != nil {
		return nil, fmt.Errorf("mlx5: %w", err)
	}
	return p, nil
}

// pin places the calling goroutine on a processor of its own and reports which,
// or -1 when there is no plan.
func pin(p *placement) (int, error) {
	cpu, err := p.Pin()
	if err != nil {
		return cpu, fmt.Errorf("mlx5: %w", err)
	}
	return cpu, nil
}
