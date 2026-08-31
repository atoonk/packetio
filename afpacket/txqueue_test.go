//go:build linux

package afpacket

import (
	"errors"
	"testing"

	"github.com/atoonk/packetio"
)

// These need no socket and no root: newTxQueue takes a region, so everything
// except the sendmmsg call itself can be driven directly. The pool bookkeeping
// is what most needs the coverage, because getting it wrong hands one frame to
// two owners and no counter says so.

func testRegion(t *testing.T, frames, frameSize int) *region {
	t.Helper()
	r, err := newRegion(frames, frameSize)
	if err != nil {
		t.Fatalf("newRegion: %v", err)
	}
	t.Cleanup(func() { r.close() })
	return r
}

// testTx builds a queue over its own slice of the region, with a socket that
// cannot be sent on. fd -1 makes sendmmsg fail, which is what we want for the
// paths that must not reach it.
func testTx(t *testing.T, r *region, firstFrame, frames int) *TxQueue {
	t.Helper()
	return newTxQueue(r, -1, firstFrame, frames, false)
}

func TestAllocGivesFramesInsideItsOwnSliceOfTheRegion(t *testing.T) {
	// The bug this exists for: a queue whose pool base was scaled twice hands
	// out addresses past the end of the region, Writable returns nil, and the
	// queue silently does nothing forever. Every queue after the first had it.
	const frames, frameSize, per = 64, 2048, 16
	r := testRegion(t, frames, frameSize)

	for q, first := range []int{0, 16, 32, 48} {
		tx := testTx(t, r, first, per)
		descs := tx.Alloc(per)
		if len(descs) != per {
			t.Fatalf("queue %d: allocated %d of %d", q, len(descs), per)
		}
		lo, hi := uint64(first*frameSize), uint64((first+per)*frameSize)
		seen := map[uint64]bool{}
		for _, d := range descs {
			if d.Addr < lo || d.Addr >= hi {
				t.Fatalf("queue %d: frame at %d is outside its range [%d,%d)", q, d.Addr, lo, hi)
			}
			if d.Addr%frameSize != 0 {
				t.Errorf("queue %d: frame at %d is not frame-aligned", q, d.Addr)
			}
			if seen[d.Addr] {
				t.Fatalf("queue %d: frame %d handed out twice", q, d.Addr)
			}
			seen[d.Addr] = true
			if w := r.Writable(d); len(w) != frameSize {
				t.Fatalf("queue %d: Writable(%d) is %d bytes, want %d", q, d.Addr, len(w), frameSize)
			}
		}
	}
}

func TestPoolRefusesAForeignFrame(t *testing.T) {
	const frameSize, per = 2048, 16
	r := testRegion(t, 64, frameSize)
	a := testTx(t, r, 0, per)
	b := testTx(t, r, 16, per)

	// A frame belonging to b, returned to a. Accepting it would let a hand
	// out a frame b also owns: two packets built into one buffer.
	stolen := b.Alloc(1)
	before := a.NumFreeFrames()
	a.Free(stolen)
	if got := a.NumFreeFrames(); got != before {
		t.Errorf("a took a frame it does not own: %d free, want %d", got, before)
	}
	if s, _ := a.Stats(); s.Backend["pool_rejected"] != 1 {
		t.Errorf("pool_rejected = %d, want 1", s.Backend["pool_rejected"])
	}
}

func TestPoolRefusesADoubleFree(t *testing.T) {
	r := testRegion(t, 32, 2048)
	q := testTx(t, r, 0, 16)
	descs := q.Alloc(4)
	full := q.NumFreeFrames() + len(descs)

	q.Free(descs)
	q.Free(descs) // the same frames again
	if got := q.NumFreeFrames(); got != full {
		t.Errorf("after a double free: %d frames, want %d", got, full)
	}
	if s, _ := q.Stats(); s.Backend["pool_rejected"] != 4 {
		t.Errorf("pool_rejected = %d, want 4", s.Backend["pool_rejected"])
	}
}

func TestTransmitRefusesADescriptorOutsideTheRegion(t *testing.T) {
	r := testRegion(t, 32, 2048)
	q := testTx(t, r, 0, 16)
	descs := q.Alloc(2)
	descs[0].Len = 64
	descs[1] = packetio.Desc{Addr: 1 << 40, Len: 64} // nowhere

	n, err := q.transmit(descs, nil)
	if !errors.Is(err, packetio.ErrBadLength) {
		t.Errorf("got %v, want ErrBadLength", err)
	}
	// The accepted set must be a prefix, so the bad descriptor stops the batch
	// rather than being skipped over or laundered into the pool.
	if n != 0 {
		t.Errorf("accepted %d frames, want 0 (fd -1 cannot send)", n)
	}
	if s, _ := q.Stats(); s.Backend["pool_rejected"] != 0 {
		t.Errorf("a refused descriptor reached the pool: %d", s.Backend["pool_rejected"])
	}
}

func TestTransmitRefusesAZeroLengthDescriptor(t *testing.T) {
	r := testRegion(t, 32, 2048)
	q := testTx(t, r, 0, 16)
	descs := q.Alloc(1) // Len stays 0
	if _, err := q.transmit(descs, nil); !errors.Is(err, packetio.ErrBadLength) {
		t.Errorf("got %v, want ErrBadLength: a zero-length frame is a malformed descriptor", err)
	}
}

func TestSendFuncAbandonsTheBatchOnABadLength(t *testing.T) {
	r := testRegion(t, 64, 2048)
	q := testTx(t, r, 0, 32)
	before := q.NumFreeFrames()

	n, err := q.SendFunc(8, func(i int, f []byte) int {
		if i == 3 {
			return len(f) + 1 // past the frame
		}
		return 64
	})
	if !errors.Is(err, packetio.ErrBadLength) {
		t.Errorf("got %v, want ErrBadLength", err)
	}
	if n != 0 {
		t.Errorf("reported %d packets sent; nothing was transmitted", n)
	}
	if got := q.NumFreeFrames(); got != before {
		t.Errorf("frames leaked: %d free, want %d", got, before)
	}
}

func TestSendFuncStopsAtAZeroLength(t *testing.T) {
	r := testRegion(t, 64, 2048)
	q := testTx(t, r, 0, 32)
	before := q.NumFreeFrames()

	// Zero means "nothing more to send": the batch ends, the rest go back, and
	// it is not an error. fd -1 means nothing is actually transmitted.
	if _, err := q.SendFunc(8, func(i int, f []byte) int {
		if i == 3 {
			return 0
		}
		return 64
	}); err != nil {
		t.Errorf("a zero length should not be an error: %v", err)
	}
	if got := q.NumFreeFrames(); got != before {
		t.Errorf("frames leaked: %d free, want %d", got, before)
	}
}

func TestAllocIsBoundedByThePendingList(t *testing.T) {
	r := testRegion(t, 32, 2048)
	q := testTx(t, r, 0, 16)
	// Nothing is pending, so the whole pool is available.
	if n := len(q.Alloc(1 << 20)); n != 16 {
		t.Errorf("allocated %d, want the pool's 16", n)
	}
}

func TestCheckOffload(t *testing.T) {
	const n = 100
	for _, tc := range []struct {
		name string
		o    packetio.Offload
		ok   bool
	}{
		{"zero is an ordinary frame", packetio.Offload{}, true},
		{"checksum inside the frame",
			packetio.Offload{Flags: packetio.OffloadNeedsCsum, CsumStart: 34, CsumOff: 16}, true},
		{"checksum start past the frame",
			packetio.Offload{Flags: packetio.OffloadNeedsCsum, CsumStart: n + 1, CsumOff: 0}, false},
		{"checksum field straddles the end",
			packetio.Offload{Flags: packetio.OffloadNeedsCsum, CsumStart: n - 1, CsumOff: 0}, false},
		{"offsets that would wrap a uint16 sum",
			packetio.Offload{Flags: packetio.OffloadNeedsCsum, CsumStart: 65535, CsumOff: 65535}, false},
		{"offsets ignored without NEEDS_CSUM",
			packetio.Offload{CsumStart: 65535, CsumOff: 65535}, true},
		{"segmented with a real size",
			packetio.Offload{GSOType: packetio.OffloadGSOTCPv4, GSOSize: 1448, HdrLen: 54}, true},
		{"segmented with no size",
			packetio.Offload{GSOType: packetio.OffloadGSOTCPv4, GSOSize: 0}, false},
		{"header past the frame",
			packetio.Offload{GSOType: packetio.OffloadGSOTCPv4, GSOSize: 1448, HdrLen: n + 1}, false},
		{"ECN does not hide the type",
			packetio.Offload{GSOType: packetio.OffloadGSOTCPv4 | packetio.OffloadGSOECN, GSOSize: 0}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkOffload(tc.o, n)
			if tc.ok && err != nil {
				t.Errorf("refused a valid offload: %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("accepted an offload that does not fit the frame")
			}
		})
	}
}
