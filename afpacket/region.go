//go:build linux

package afpacket

import (
	"fmt"

	"github.com/atoonk/packetio"
	"golang.org/x/sys/unix"
)

// region is the frame memory this device transmits out of and receives into.
//
// Unlike the mlx5 and AF_XDP backends nothing here is shared with the NIC: the
// kernel copies into and out of it, so it could just as well be Go heap memory.
// It is mapped anyway, for the same layout and the same descriptor arithmetic
// as the other backends, so that code above packetio does not have to care
// which one it got.
type region struct {
	b         []byte
	frameSize int
	frameMask uint64 // frameSize-1, to find the end of a frame without a divide
	frames    int
}

func newRegion(frames, frameSize int) (*region, error) {
	if frames <= 0 || frameSize <= 0 {
		return nil, fmt.Errorf("afpacket: a region of %d frames of %d bytes", frames, frameSize)
	}
	if frameSize&(frameSize-1) != 0 {
		return nil, fmt.Errorf("afpacket: frame size %d is not a power of two", frameSize)
	}
	b, err := unix.Mmap(-1, 0, frames*frameSize, unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_PRIVATE|unix.MAP_ANONYMOUS|unix.MAP_POPULATE)
	if err != nil {
		return nil, fmt.Errorf("afpacket: mapping %d bytes of frame memory: %w", frames*frameSize, err)
	}
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
		return fmt.Errorf("afpacket: unmapping frame memory: %w", err)
	}
	return nil
}

func (r *region) Bytes() []byte { return r.b }

func (r *region) Frame(d packetio.Desc) []byte {
	if d.Addr >= uint64(len(r.b)) || d.Addr+uint64(d.Len) > uint64(len(r.b)) {
		return nil
	}
	return r.b[d.Addr : d.Addr+uint64(d.Len) : d.Addr+uint64(d.Len)]
}

func (r *region) Writable(d packetio.Desc) []byte {
	if d.Addr >= uint64(len(r.b)) {
		return nil
	}
	// The end of the frame containing d, found without a divide.
	end := (d.Addr &^ r.frameMask) + r.frameMask + 1
	if end > uint64(len(r.b)) {
		end = uint64(len(r.b))
	}
	return r.b[d.Addr:end:end]
}

func (r *region) FrameSize() int { return r.frameSize }
func (r *region) NumFrames() int { return r.frames }

var _ packetio.Region = (*region)(nil)
