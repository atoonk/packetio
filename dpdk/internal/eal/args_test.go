//go:build linux && cgo && dpdk && (amd64 || arm64)

package eal

import "testing"

// WithEALArgs was a no-op for a while: the option was recorded and never read.
// This is the check that it reaches the environment's argument list.
func TestArgsExtraReachesArgv(t *testing.T) {
	got := Args{Devices: []string{"net_null0"}, Extra: []string{"--legacy-mem"}}.argv()
	last := got[len(got)-1]
	if last != "--legacy-mem" {
		t.Fatalf("argv = %q; the extra argument did not reach it", got)
	}
}
