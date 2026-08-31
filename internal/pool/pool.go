// Package pool holds the free-frame list shared by packetio backends.
package pool

import (
	"math/bits"
	"sync/atomic"
)

// Frames is a LIFO free list of frame offsets for one direction of one queue.
//
// It is not synchronized, by design. Each pool belongs to exactly one
// goroutine, and pools that a receive goroutine and a transmit goroutine touch
// concurrently must cover disjoint ranges of the region. That is what lets the
// packet path run without a lock. go-afxdp learned this the hard way: sharing
// one free list across both directions handed the same frame to a fill and a
// transmit at once, which corrupts packets on the wire and is invisible in
// local counters.
//
// A LIFO rather than a FIFO because the most recently freed frame is the one
// most likely to still be in cache.
type Frames struct {
	free       []uint64
	frameSize  int
	frameMask  uint64 // frameSize-1; frame sizes are powers of two
	frameShift uint   // log2(frameSize), so an index is a shift and not a divide
	base       uint64
	end        uint64
	// rejected is atomic only so that a monitoring goroutine may read it
	// through Stats while the queue's goroutine writes it. It is written on the
	// refusal path alone, which a healthy queue never takes, so this costs
	// nothing on the packet path.
	rejected atomic.Uint64

	// in records which frames are on the free list, one bit each. It is only
	// allocated in a checked build; see check_on.go for why it is not always
	// on.
	in []uint64
}

// New builds a pool holding count frames of frameSize bytes each, starting at
// frame index base. The frames cover
// [base*frameSize, (base+count)*frameSize) of the region.
func New(base, count, frameSize int) *Frames {
	p := &Frames{
		free:       make([]uint64, count),
		frameSize:  frameSize,
		frameMask:  uint64(frameSize - 1),
		frameShift: uint(bits.TrailingZeros64(uint64(frameSize))),
		base:       uint64(base) * uint64(frameSize),
	}
	p.end = p.base + uint64(count)*uint64(frameSize)
	p.initChecks(count)
	// Fill so that the first frames popped are the lowest addresses: nicer to
	// read in a packet capture and in a hex dump of the region.
	for i := 0; i < count; i++ {
		p.free[i] = uint64(base+count-1-i) * uint64(frameSize)
	}
	return p
}

// Len is how many frames are free.
func (p *Frames) Len() int { return len(p.free) }

// FrameSize is the size of one frame.
func (p *Frames) FrameSize() int { return p.frameSize }

// Owns reports whether addr falls inside a frame of this pool. It is for
// assertions and tests, not for the packet path.
func (p *Frames) Owns(addr uint64) bool { return addr >= p.base && addr < p.end }

// Pop removes up to n frames and appends their offsets to dst, returning the
// grown slice. It appends fewer than n when fewer are free, and allocates
// nothing when dst has the capacity.
func (p *Frames) Pop(n int, dst []uint64) []uint64 {
	if n > len(p.free) {
		n = len(p.free)
	}
	if n <= 0 {
		return dst
	}
	start := len(p.free) - n
	p.markTaken(p.free[start:])
	dst = append(dst, p.free[start:]...)
	p.free = p.free[:start]
	return dst
}

// index is which frame of this pool addr is, counted from the pool's base.
//
// A shift, not a divide. Frame sizes are powers of two and this is called once
// per frame on the way in and once on the way out, which is once per packet on
// a forwarding path -- exactly where Base says a sixty-four-bit divide is
// twenty cycles that buy nothing. Written as a divide first, it cost 13% of
// the transmit path, more than the check it exists to serve.
func (p *Frames) index(addr uint64) uint64 {
	return (addr - p.base) >> p.frameShift
}

// Push returns one frame. addr must be the start of a frame of this pool:
// callers hold a descriptor whose Addr may point past the frame start, so they
// round down with Base first.
//
// A push is refused and counted, rather than appended, when the frame is not
// this pool's, when it is not the start of a frame, when it is already on the
// free list, or when the pool is already full. Every one of them is a caller
// returning a frame twice or to the wrong queue, and every one would otherwise
// hand a frame to two owners -- the corruption that is invisible in every other
// counter. A shift and a mask; not worth leaving out.
func (p *Frames) Push(addr uint64) {
	if addr < p.base || addr >= p.end || addr&p.frameMask != 0 || len(p.free) >= cap(p.free) {
		p.rejected.Add(1)
		return
	}
	if !p.markFree(addr) {
		// Already on the free list. Appending it would put one frame there
		// twice, and the next two Allocs would hand it to two owners.
		p.rejected.Add(1)
		return
	}
	p.free = append(p.free, addr)
}

// Rejected is how many pushes were refused: foreign, unaligned, already free,
// or surplus. Anything but zero is a bug in the code above the pool.
func (p *Frames) Rejected() uint64 { return p.rejected.Load() }

// Base rounds a descriptor address down to the start of its frame. Frames are
// laid out at fixed multiples of frameSize from the start of the region, so
// this recovers the frame a descriptor belongs to whatever headroom it used.
//
// The frame size is a power of two, so this is a mask. It is called once per
// packet on the way back to the free list, where a divide would be pure waste.
func (p *Frames) Base(addr uint64) uint64 { return addr &^ p.frameMask }
