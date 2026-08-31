package conform

import (
	"os"
	"testing"
)

// Every backend's conformance test must be able to open a device when the
// environment says it can. This is the guard against the way AF_XDP went
// unverified: its opener called Open in a way that backend always refuses, so
// every subtest skipped and the package reported ok for months.
//
// Run it with the hardware present:
//
//	sudo PACKETIO_IFACE=eno2 PACKETIO_VETH_TESTS=1 PACKETIO_CONFORM_REQUIRED=1 go test ./...
//
// and any backend whose opener is broken fails loudly instead of skipping.
func TestRequiredIsOffByDefault(t *testing.T) {
	if os.Getenv("PACKETIO_CONFORM_REQUIRED") != "" {
		t.Skip("the variable is set in this environment, which is the point")
	}
	if Required() {
		t.Error("Required is true with the variable unset")
	}
}

func TestRequiredIsOnWhenSet(t *testing.T) {
	t.Setenv("PACKETIO_CONFORM_REQUIRED", "1")
	if !Required() {
		t.Error("Required is false with the variable set")
	}
}
