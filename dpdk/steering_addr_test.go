//go:build linux && cgo && dpdk && amd64

package dpdk

import "testing"

// A PCI address is joined into a /sys path, so anything that is not one has to
// be refused before it gets there.
func TestSafePCIAddr(t *testing.T) {
	for _, ok := range []string{"0000:c1:00.1", "c1:00.1", "0000:01:00.0", "ffff:ff:1f.7"} {
		if !safePCIAddr(ok) {
			t.Errorf("%q refused, want accepted", ok)
		}
	}
	for _, bad := range []string{
		"", "../../etc/passwd:x.y", "0000:c1:00.1/../..", "0000:c1:00.8",
		"zzzz:c1:00.1", "0000:c1:00", "0000:c1:00.1\x00",
	} {
		if safePCIAddr(bad) {
			t.Errorf("%q accepted, want refused", bad)
		}
	}
}
