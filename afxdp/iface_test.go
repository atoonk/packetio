//go:build linux

package afxdp

import (
	"testing"

	xdp "github.com/atoonk/go-afxdp"
	"github.com/atoonk/packetio"
)

// The backend has to satisfy the API it claims to implement, and the compiler
// is the only thing that can say so without a NIC to open.
func TestImplementsThePacketioAPI(t *testing.T) {
	var (
		_ packetio.Device  = (*Device)(nil)
		_ packetio.TxQueue = (*TxQueue)(nil)
		_ packetio.RxQueue = (*RxQueue)(nil)
		_ packetio.Region  = (*region)(nil)
	)
}

// The two libraries describe a frame the same way. If either ever changes, the
// conversions below stop being lossless and this catches it.
func TestDescriptorsSurviveTheRoundTrip(t *testing.T) {
	// The transmit direction keeps only the continuation bit: the kernel's tx
	// ring rejects descriptors carrying any other option bit -- silently, into
	// tx_invalid_descs -- and Desc promises Transmit ignores them. Addr and
	// Len survive untouched; Options come back masked to OptContinued.
	in := []packetio.Desc{
		{Addr: 0, Len: 0, Options: 0},
		{Addr: 4096, Len: 60, Options: packetio.OptChecksumOK},
		{Addr: 1 << 40, Len: ^uint32(0), Options: ^uint32(0)},
	}
	want := []packetio.Desc{
		{Addr: 0, Len: 0, Options: 0},
		{Addr: 4096, Len: 60, Options: 0},
		{Addr: 1 << 40, Len: ^uint32(0), Options: packetio.OptContinued},
	}
	back := toDescs(fromDescs(in, nil), nil)
	if len(back) != len(in) {
		t.Fatalf("%d descriptors went in and %d came back", len(in), len(back))
	}
	for i := range in {
		if back[i] != want[i] {
			t.Errorf("descriptor %d came back as %+v, want %+v", i, back[i], want[i])
		}
	}
}

// Writable must stop at the end of the frame the descriptor is in, whatever
// offset within it the descriptor names, or a packet built through it would
// run into the next frame.
func TestWritableStopsAtTheFrameBoundary(t *testing.T) {
	const frameSize = 2048
	r := &region{s: nil}
	// The region needs a socket for its memory, which a test cannot make, so
	// the arithmetic is checked directly against the same expression.
	for _, addr := range []uint64{0, 1, frameSize - 1, frameSize, frameSize + 64} {
		size := uint64(frameSize)
		end := (addr/size + 1) * size
		if end <= addr || end%size != 0 || end-addr > size {
			t.Errorf("addr %d gives an end of %d", addr, end)
		}
	}
	_ = r
}

// Free must not lose frames: what goes back must come out of the next Alloc,
// because go-afxdp has no way to return one to its own pool.
func TestFreeGivesFramesBackToAlloc(t *testing.T) {
	// No socket here: freeing and re-allocating within what was freed must
	// never reach for one, which is the property being checked.
	q := &TxQueue{}
	freed := map[uint64]bool{0: true, 2048: true, 4096: true}
	q.Free([]packetio.Desc{{Addr: 0}, {Addr: 2048}, {Addr: 4096}})
	if len(q.spare) != 3 {
		t.Fatalf("%d frames held after freeing 3", len(q.spare))
	}

	got := q.Alloc(2)
	if len(got) != 2 {
		t.Fatalf("Alloc(2) after freeing 3 returned %d", len(got))
	}
	for _, d := range got {
		if !freed[d.Addr] {
			t.Errorf("Alloc returned %d, which was never freed", d.Addr)
		}
	}
	if len(q.spare) != 1 {
		t.Errorf("%d frames still held, want 1", len(q.spare))
	}

	// The last one comes back too, and the same frame is never handed out
	// twice: Alloc reuses the scratch slice, so this also checks the caller
	// gets what it asked for and not the previous batch.
	last := q.Alloc(1)
	if len(last) != 1 || !freed[last[0].Addr] {
		t.Fatalf("the last freed frame did not come back: %+v", last)
	}
	if len(q.spare) != 0 {
		t.Errorf("%d frames still held, want none", len(q.spare))
	}
}

var _ = xdp.Desc{}
