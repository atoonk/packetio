//go:build linux && cgo && dpdk && (amd64 || arm64)

package main

import (
	"os"
	"strings"
	"testing"
)

// The point of this example is that it forwards packets exactly the way the
// native mlx5 one does, so a benchmark between the two measures the packet
// path and nothing else. That only holds while both call the same forwarding
// code rather than growing private copies of it.
//
// This is cheap insurance against the copy that gets made in a hurry and then
// quietly diverges: a fixed checksum here and not there is a difference nobody
// would attribute to the backend.
func TestBothRoutersShareTheirForwardingCode(t *testing.T) {
	for _, dir := range []string{".", "../../l3fwd"} {
		if !usesSharedForwarding(t, dir) {
			t.Errorf("%s no longer calls examples/internal/forward; the two routers have "+
				"diverged and comparing them no longer measures the packet path", dir)
		}
	}
}

func usesSharedForwarding(t *testing.T, dir string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(dir + "/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		// Both the import and a real call: importing it and not using it would
		// not compile, but a partial copy that keeps the import would.
		if strings.Contains(string(b), "examples/internal/forward") &&
			strings.Contains(string(b), "forward.Parse(") {
			return true
		}
	}
	return false
}
