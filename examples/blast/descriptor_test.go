//go:build linux && cgo && mlx5 && (amd64 || arm64)

package main

import "testing"

// The step is the point: a frame four bytes larger can cost three packets out
// of every descriptor. These are the values measured on a ConnectX-6 Dx, where
// 64-byte frames ran at 96 cycles a packet and 68-byte at 111.
func TestPacketsPerDescriptor(t *testing.T) {
	for _, tc := range []struct{ wire, want int }{
		{64, 14}, // 60 bytes to the NIC: 4 octowords
		{68, 11}, // 64 bytes: 5 octowords, and three packets fewer
		{72, 11},
		{80, 11}, // still 5 -- the band is 68 to 80
		{84, 9},  // 6 octowords
		{96, 9},
		{100, 8},
		{116, 7},
		{132, 6},
		{148, 5},
		{180, 4},
	} {
		if got := packetsPerDescriptor(tc.wire); got != tc.want {
			t.Errorf("packetsPerDescriptor(%d) = %d, want %d", tc.wire, got, tc.want)
		}
	}
}

// A size below the frame check sequence is nonsense rather than a crash.
func TestPacketsPerDescriptorRefusesNonsense(t *testing.T) {
	for _, wire := range []int{0, 2, 4} {
		if got := packetsPerDescriptor(wire); got != 0 {
			t.Errorf("packetsPerDescriptor(%d) = %d, want 0", wire, got)
		}
	}
}
