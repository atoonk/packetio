//go:build linux

package netstack

import (
	"testing"

	xdp "github.com/atoonk/go-afxdp"
	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/afxdp"
)

func init() { backends["afxdp"] = openAFXDP }

// openAFXDP binds an AF_XDP socket on the veth in generic mode (a veth has
// no native XDP driver). The filter takes ARP and all of TCP, so the stack
// owns its address outright and the echo and dial tests share one filter;
// everything else stays with the kernel.
func openAFXDP(t *testing.T, l *lab) packetio.Device {
	t.Helper()
	dev, err := afxdp.Open(l.near, afxdp.WithXDP(
		xdp.WithGenericMode(),
		xdp.WithFilter(xdp.MatchEtherType(etherTypeARP), xdp.MatchTCPPort()),
	))
	if err != nil {
		t.Logf("afxdp: %v", err)
		return nil
	}
	return dev
}
