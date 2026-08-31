//go:build linux && cgo && mlx5 && (amd64 || arm64)

package mlx5

import "unsafe"

// ptr is the address of the first byte of b, or nil if b is empty. It is
// written once here so that the packet path does not repeat the unsafe
// conversion in a dozen places.
func ptr(b []byte) unsafe.Pointer {
	if len(b) == 0 {
		return nil
	}
	return unsafe.Pointer(&b[0])
}
