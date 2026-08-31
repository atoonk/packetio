//go:build linux

package afxdp

import (
	"testing"

	"github.com/atoonk/packetio"
)

// Transmit must refuse a descriptor that does not name bytes inside one frame,
// and take the prefix before it. The kernel counts a bad one in
// tx_invalid_descs and carries on, so without this the caller is told the whole
// batch went and never learns which packet did not.
func TestTransmitRefusesDescriptorsOutsideOneFrame(t *testing.T) {
	q := &TxQueue{regionLen: 8 * 2048, frameSize: 2048}
	for _, tc := range []struct {
		name string
		d    packetio.Desc
		want bool
	}{
		{"a whole frame", packetio.Desc{Addr: 2048, Len: 2048}, true},
		{"one byte", packetio.Desc{Addr: 2048, Len: 1}, true},
		{"the last frame, exactly", packetio.Desc{Addr: 7 * 2048, Len: 2048}, true},
		{"past the region", packetio.Desc{Addr: 8 * 2048, Len: 64}, false},
		{"past its own frame", packetio.Desc{Addr: 2048 + 1600, Len: 1024}, false},
		{"one byte past its frame", packetio.Desc{Addr: 2048, Len: 2049}, false},
		{"zero length", packetio.Desc{Addr: 2048, Len: 0}, false},
		{"address wraps", packetio.Desc{Addr: ^uint64(0), Len: 64}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := q.descOK(tc.d); got != tc.want {
				t.Errorf("descOK(%+v) = %v, want %v", tc.d, got, tc.want)
			}
		})
	}
}

// A queue with no region -- only the unit tests build one -- has nothing to
// check against and must not refuse everything.
func TestDescOKWithoutARegion(t *testing.T) {
	q := &TxQueue{}
	if !q.descOK(packetio.Desc{Addr: 4096, Len: 64}) {
		t.Error("a queue with no region refused a descriptor")
	}
}
