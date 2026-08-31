//go:build !amd64 && !arm64

package prefetch

import "unsafe"

// T0 is a hint, and no hint is a correct implementation of a hint.
func T0(p unsafe.Pointer) {}
