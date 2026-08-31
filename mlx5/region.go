//go:build linux && cgo && mlx5 && (amd64 || arm64)

package mlx5

import (
	"fmt"

	"github.com/atoonk/packetio"
	"golang.org/x/sys/unix"
)

// region is the frame memory a device transmits out of and receives into: one
// mapping, registered with the NIC once, carved into fixed-size frames.
//
// It is mapped rather than allocated on the Go heap because the NIC reads and
// writes it by direct memory access for as long as the device is open, and the
// garbage collector is entitled to move or reuse anything on the heap. It is
// populated at map time so that no packet waits on a page fault; registering it
// with the device is what pins it.
type region struct {
	b         []byte
	frameSize int
	frameMask uint64 // frameSize-1, for finding the end of a frame without a divide
	frames    int
	hugePages bool
}

func newRegion(frames, frameSize int, hugePages bool) (*region, error) {
	if frames <= 0 || frameSize <= 0 {
		return nil, fmt.Errorf("mlx5: a region of %d frames of %d bytes", frames, frameSize)
	}
	size := frames * frameSize

	flags := unix.MAP_PRIVATE | unix.MAP_ANONYMOUS | unix.MAP_POPULATE
	prot := unix.PROT_READ | unix.PROT_WRITE

	if hugePages {
		// Huge pages mean fewer address translations for the NIC to cache as
		// it walks the region, which at tens of millions of packets a second
		// is worth asking for. It is only worth asking, though: a machine with
		// none reserved should still work.
		if b, err := unix.Mmap(-1, 0, size, prot, flags|unix.MAP_HUGETLB); err == nil {
			return &region{b: b, frameSize: frameSize, frameMask: uint64(frameSize - 1), frames: frames, hugePages: true}, nil
		}
	}

	b, err := unix.Mmap(-1, 0, size, prot, flags)
	if err != nil {
		return nil, fmt.Errorf("mlx5: mapping %d bytes of frame memory: %w", size, err)
	}
	// Whether or not huge pages were asked for, the kernel may still back this
	// with them, and there is no cost to saying they would be welcome. This
	// matters most on the path where they were asked for and no reservation
	// existed to satisfy it.
	_ = unix.Madvise(b, unix.MADV_HUGEPAGE)
	return &region{b: b, frameSize: frameSize, frameMask: uint64(frameSize - 1), frames: frames}, nil
}

func (r *region) close() error {
	if r.b == nil {
		return nil
	}
	err := unix.Munmap(r.b)
	r.b = nil
	if err != nil {
		return fmt.Errorf("mlx5: unmapping frame memory: %w", err)
	}
	return nil
}

// Bytes returns the whole region.
func (r *region) Bytes() []byte { return r.b }

// Frame returns the bytes a descriptor names.
// Frame is the bytes a descriptor names, or nil when it names anything else.
//
// A descriptor that is not inside the region is a bug above this layer, and
// returning nil says so at the point of use. Slicing it unchecked panics
// instead, in a library that runs as root -- and every other backend returns
// nil, so code written against one of them would find this one different.
func (r *region) Frame(d packetio.Desc) []byte {
	if d.Addr >= uint64(len(r.b)) || d.Addr+uint64(d.Len) > uint64(len(r.b)) {
		return nil
	}
	return r.b[d.Addr : d.Addr+uint64(d.Len) : d.Addr+uint64(d.Len)]
}

// Writable returns everything from a descriptor's address to the end of the
// frame it is in, for building a packet whose length is not known yet.
//
// The frame size is a power of two, so the end of the frame is a mask and an
// add rather than a division. That matters: this is called once per packet,
// and a 64-bit divide is twenty cycles that buy nothing.
// Writable is the whole of the frame containing d, or nil when d is not in
// this region.
func (r *region) Writable(d packetio.Desc) []byte {
	if d.Addr >= uint64(len(r.b)) {
		return nil
	}
	end := (d.Addr &^ r.frameMask) + r.frameMask + 1
	if end > uint64(len(r.b)) {
		end = uint64(len(r.b))
	}
	return r.b[d.Addr:end:end]
}

// FrameSize is the size of one frame, and the largest packet that fits in one.
func (r *region) FrameSize() int { return r.frameSize }

// NumFrames is how many frames the region holds.
func (r *region) NumFrames() int { return r.frames }

// va is the address the NIC knows the region by, which for a mapping
// registered by address is the address itself.
func (r *region) va() uint64 { return uint64(uintptr(ptr(r.b))) }
