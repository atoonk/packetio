//go:build amd64 || arm64

// Package prefetch hints frame memory into the cache ahead of first use.
//
// A received frame was written by the NIC's DMA engine, so its first read by
// the CPU misses every cache on machines without inbound-DMA cache placement
// (EPYC has none). A parser that walks a batch pays that miss serially,
// packet after packet; hinting the whole batch first lets the misses overlap.
package prefetch

import "unsafe"

// T0 requests the cache line at p into every level of the cache.
//
//go:noescape
func T0(p unsafe.Pointer)
