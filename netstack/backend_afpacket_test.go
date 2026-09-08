//go:build linux

package netstack

import (
	"testing"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/afpacket"
)

func init() { backends["afpacket"] = openAFPacket }

// openAFPacket is the tap: no steering, the kernel sees every frame too,
// which is why the stack's address is one the kernel does not own.
func openAFPacket(t *testing.T, l *lab) packetio.Device {
	t.Helper()
	dev, err := afpacket.Open(l.near, afpacket.WithQueues(1), afpacket.WithFrames(1024))
	if err != nil {
		t.Logf("afpacket: %v", err)
		return nil
	}
	return dev
}
