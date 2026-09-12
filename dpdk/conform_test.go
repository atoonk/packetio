//go:build linux && cgo && dpdk && (amd64 || arm64)

package dpdk_test

import (
	"os"
	"testing"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/dpdk"
	"github.com/atoonk/packetio/internal/conform"
)

// TestConformance runs the shared backend suite.
//
// It needs no NIC: net_null is a driver that accepts everything transmitted and
// delivers a stream of empty packets, which is enough to exercise every rule
// about who owns a frame. PACKETIO_DPDK_DEV points it at a real device instead.
//
// The environment is process-wide and starts once, so every test in this
// package shares whichever device the first one opened. That is why the device
// is chosen in one place.
func TestConformance(t *testing.T) {
	conform.Run(t, func(t *testing.T) packetio.Device {
		d, err := open(t)
		if err != nil {
			t.Logf("open: %v", err)
			return nil
		}
		return d
	})
}

// device is what this package's tests open, and the options that go with it.
// Everything in one process must use the same one: DPDK's environment is
// started once and its arguments are fixed then.
func device() (string, []dpdk.Option) {
	if dev := os.Getenv("PACKETIO_DPDK_DEV"); dev != "" {
		return dev, nil
	}
	if iface := os.Getenv("PACKETIO_IFACE"); iface != "" {
		return iface, nil
	}
	// A virtual device, and no hugepages: this has to run on a laptop.
	return "net_null0", []dpdk.Option{dpdk.WithoutHugePages(256)}
}

func open(t *testing.T, extra ...dpdk.Option) (*dpdk.Device, error) {
	t.Helper()
	if os.Geteuid() != 0 {
		return nil, errNotRoot
	}
	name, opts := device()
	opts = append(opts, dpdk.WithTxQueues(1), dpdk.WithRxQueues(1),
		dpdk.WithFrames(2048), dpdk.WithTxDepth(256), dpdk.WithRxDepth(256))
	return dpdk.Open(name, append(opts, extra...)...)
}

type notRoot struct{}

func (notRoot) Error() string {
	return "this backend needs root: it maps device memory and, on a real NIC, hugepages"
}

var errNotRoot = notRoot{}

// TestConformanceSteering checks that a filter this device cannot express is
// refused rather than installed as something wider.
//
// On net_null there is no flow engine at all, so every filter is refused -- which
// is the right answer and still worth asserting: a backend that quietly ignored
// a filter it could not install would deliver more than it was asked for, and
// nothing downstream could tell.
func TestConformanceSteering(t *testing.T) {
	conform.RunSteering(t, func(t *testing.T, f packetio.SteeringFilter) (packetio.Device, error) {
		if os.Geteuid() != 0 {
			return nil, nil
		}
		d, err := open(t, dpdk.WithSteering(f))
		if d == nil && err == nil {
			return nil, nil
		}
		return d, err
	})
}
