//go:build linux && cgo && dpdk && (amd64 || arm64)

package netstack

import (
	"fmt"
	"testing"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/dpdk"
)

func init() { backends["dpdk"] = openDPDK }

// vdevIndex numbers the af_packet vdevs: the environment starts once per
// process, and a closed vdev's name cannot be reused within it.
var vdevIndex int

// openDPDK runs DPDK's af_packet driver on the veth: no hugepages, and a
// frame big enough for the driver's own ring frame. Like afpacket it is a
// tap the kernel shares, and the vdev shows the endpoint its own
// transmissions, which the endpoint drops.
func openDPDK(t *testing.T, l *lab) packetio.Device {
	t.Helper()
	name := fmt.Sprintf("net_af_packet%d,iface=%s", vdevIndex, l.near)
	vdevIndex++
	dev, err := dpdk.Open(name,
		dpdk.WithoutHugePages(256), dpdk.WithQueues(1),
		dpdk.WithFrameSize(4096), dpdk.WithFrames(1024),
		dpdk.WithTxDepth(256), dpdk.WithRxDepth(256))
	if err != nil {
		t.Logf("dpdk: %v", err)
		return nil
	}
	return dev
}
