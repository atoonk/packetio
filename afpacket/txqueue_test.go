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
			// Alloc points frameHeadroom into the frame, the same offset
			// Receive delivers at: room to prepend, and a copy source that
			// stays out of the 4 KiB-aliasing window (see frameHeadroom).
			if d.Addr%frameSize != frameHeadroom {
				t.Errorf("queue %d: frame at %d is not headroom-offset", q, d.Addr)
			}
			if seen[d.Addr] {
				t.Fatalf("queue %d: frame %d handed out twice", q, d.Addr)
			}
			seen[d.Addr] = true
			if w := r.Writable(d); len(w) != frameSize-frameHeadroom {
				t.Fatalf("queue %d: Writable(%d) is %d bytes, want %d", q, d.Addr, len(w), frameSize-frameHeadroom)
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

// The rules a multi-buffer packet has to follow, and what happens when it
// does not. A socket of -1 means the send itself fails, which is fine: every
// case here is refused before the syscall, and the refusal is the subject.
func TestMultiBufferRefusals(t *testing.T) {
	const frames, frameSize = 64, 2048
	r := testRegion(t, frames, frameSize)

	chain := func(tx *TxQueue, lens ...int) []packetio.Desc {
		descs := tx.Alloc(len(lens))
		if len(descs) != len(lens) {
			t.Fatalf("allocated %d of %d", len(descs), len(lens))
		}
		for i, n := range lens {
			descs[i].Len = uint32(n)
			if i < len(lens)-1 {
				descs[i].Options |= packetio.OptContinued
			}
		}
		return descs
	}

	t.Run("a chain that never ends is refused", func(t *testing.T) {
		tx := testTx(t, r, 0, 16)
		descs := chain(tx, 100, 100)
		descs[1].Options |= packetio.OptContinued // no last frame
		n, err := tx.transmit(descs, nil)
		if n != 0 {
			t.Errorf("took %d frames of a packet that is not whole", n)
		}
		if !errors.Is(err, packetio.ErrBadLength) {
			t.Errorf("got %v, want ErrBadLength", err)
		}
	})

	t.Run("offload metadata on a continuation is refused", func(t *testing.T) {
		tx := newTxQueue(r, -1, 16, 16, true) // GSO, so offload metadata is allowed
		descs := chain(tx, 100, 100)
		offs := []packetio.Offload{{}, {GSOType: packetio.OffloadGSOTCPv4, GSOSize: 100}}
		n, err := tx.transmit(descs, offs)
		if n != 0 {
			t.Errorf("took %d frames whose metadata contradicts the chain", n)
		}
		if !errors.Is(err, packetio.ErrBadLength) {
			t.Errorf("got %v, want ErrBadLength", err)
		}
	})

	t.Run("a bad frame anywhere in a chain refuses the whole packet", func(t *testing.T) {
		tx := testTx(t, r, 32, 16)
		descs := chain(tx, 100, 100, 100)
		descs[2].Len = frameSize * 2 // past the end of its frame
		n, err := tx.transmit(descs, nil)
		if n != 0 {
			t.Errorf("took %d frames of a packet with a bad one in it", n)
		}
		if !errors.Is(err, packetio.ErrBadLength) {
			t.Errorf("got %v, want ErrBadLength", err)
		}
	})

	t.Run("offload is checked against the whole packet, not its first frame", func(t *testing.T) {
		tx := newTxQueue(r, -1, 48, 16, true) // GSO, so offload metadata is allowed
		descs := chain(tx, 100, 100, 100)
		// A checksum field in the second buffer: past frame one, inside the
		// packet. Checking against the first frame alone would refuse it, and
		// every real super-frame looks like this.
		offs := make([]packetio.Offload, 3)
		offs[0] = packetio.Offload{Flags: packetio.OffloadNeedsCsum, CsumStart: 150, CsumOff: 16}
		if _, err := tx.transmit(descs, offs); errors.Is(err, packetio.ErrBadLength) {
			t.Errorf("refused a checksum offset that is inside the packet: %v", err)
		}
		tx.Free(descs)

		// And one past the packet's total must still be refused.
		descs = chain(tx, 100, 100, 100)
		offs[0].CsumStart = 400
		if _, err := tx.transmit(descs, offs); !errors.Is(err, packetio.ErrBadLength) {
			t.Errorf("accepted a checksum offset past the whole packet: %v", err)
		}
	})
}

// The frame-size rule is about having somewhere to put an arriving
// super-frame, and there are two ways to have that: one frame big enough, or
// WithMultiBuffer and a chain of small ones. Refusing the second is refusing
// the configuration receive chains exist for.
func TestGSOFrameSizeRuleAllowsMultiBuffer(t *testing.T) {
	base := func() config {
		c := defaults()
		c.gso, c.rxQueues, c.txQueues = true, 1, 1
		c.frameSize, c.frameSizeSet = 2048, true
		return c
	}
	c := base()
	if err := c.validate(); err == nil {
		t.Error("small frames with GSO receive and no chaining were accepted; nothing could hold a super-frame")
	}
	c = base()
	c.multiBuffer = true
	if err := c.validate(); err != nil {
		t.Errorf("small frames with GSO receive and chaining were refused: %v", err)
	}
	// And a big frame is still fine either way.
	c = base()
	c.frameSize = 1 << 16
	if err := c.validate(); err != nil {
		t.Errorf("a frame big enough for a super-frame was refused: %v", err)
	}
}

// What TransmitGather refuses. Nothing here reaches the socket: the point is
// that a caller's mistake is named before the kernel sees it.
func TestTransmitGatherRefusals(t *testing.T) {
	r := testRegion(t, 64, 2048)
	seg := func(n int) []byte { return make([]byte, n) }

	for _, tc := range []struct {
		name   string
		gso    bool
		segs   [][]byte
		counts []int
		offs   []packetio.Offload
		want   error
	}{
		{"counts claim more segments than there are", false,
			[][]byte{seg(10)}, []int{2}, nil, packetio.ErrBadLength},
		{"a packet of no segments", false,
			[][]byte{seg(10)}, []int{0}, nil, packetio.ErrBadLength},
		{"an empty segment", false,
			[][]byte{seg(10), seg(0)}, []int{2}, nil, packetio.ErrBadLength},
		{"one offload per packet, not per segment", true,
			[][]byte{seg(10), seg(10)}, []int{2}, make([]packetio.Offload, 2), packetio.ErrBadLength},
		{"offload without WithGSO", false,
			[][]byte{seg(10)}, []int{1}, make([]packetio.Offload, 1), packetio.ErrUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := newTxQueue(r, -1, 0, 16, tc.gso)
			n, err := q.TransmitGather(tc.segs, tc.counts, tc.offs)
			if n != 0 {
				t.Errorf("accepted %d packets", n)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}

	t.Run("offload is checked against the whole packet", func(t *testing.T) {
		q := newTxQueue(r, -1, 16, 16, true)
		segs := [][]byte{seg(100), seg(100), seg(100)}
		offs := []packetio.Offload{{Flags: packetio.OffloadNeedsCsum, CsumStart: 150, CsumOff: 16}}
		if _, err := q.TransmitGather(segs, []int{3}, offs); errors.Is(err, packetio.ErrBadLength) {
			t.Errorf("refused a checksum offset inside the packet: %v", err)
		}
		offs[0].CsumStart = 400 // past the 300 bytes there are
		if _, err := q.TransmitGather(segs, []int{3}, offs); !errors.Is(err, packetio.ErrBadLength) {
			t.Errorf("accepted a checksum offset past the packet: %v", err)
		}
	})
}

// A chained super-frame needs a whole chain of frames at once, out of one
// queue's share of the pool. A queue that can never hold one would take the
// packet, fail part way, put it back, and do that for as long as the device is
// open: no packets, no counter moving, no error. It is refused at Open instead.
func TestPoolMustHoldOneChainedSuperFrame(t *testing.T) {
	base := func() config {
		c := defaults()
		c.gso, c.multiBuffer = true, true
		c.frameSize, c.frameSizeSet = 2048, true
		c.txQueues, c.rxQueues = 4, 4
		c.frames, c.framesSet = 256, true
		return c
	}
	// 256 frames over 8 queues is 32 each; a 64 KB packet in 1984-byte frames
	// needs 34.
	if err := base().validate(); err == nil {
		t.Error("accepted a pool that cannot hold one chained super-frame")
	}
	c := base()
	c.frames = 1024
	if err := c.validate(); err != nil {
		t.Errorf("refused a pool with room to spare: %v", err)
	}
	// Without chaining the rule does not apply: a super-frame lands in one
	// big frame instead, which validate checks separately.
	c = base()
	c.multiBuffer = false
	c.frameSize = 1 << 16
	if err := c.validate(); err != nil {
		t.Errorf("refused a big-frame GSO device: %v", err)
	}
}

// WithGSO cuts the frame count because it makes frames 32 times bigger. When
// the caller keeps small frames it has made them no bigger, and cutting the
// count anyway leaves a pool too small for the chains those small frames exist
// to carry.
func TestWithGSOKeepsTheFrameCountWhenItKeepsTheFrameSize(t *testing.T) {
	build := func(opts ...Option) config {
		c := defaults()
		for _, o := range opts {
			o(&c)
		}
		c.resolve()
		return c
	}

	c := build(WithFrameSize(2048), WithGSO())
	if c.frameSize != 2048 {
		t.Errorf("frame size is %d, want the 2048 asked for", c.frameSize)
	}
	if c.frames == 256 {
		t.Errorf("frame count was cut to 256 though the frames were not enlarged")
	}
	// And when it does enlarge them, it still scales the count back.
	c = build(WithGSO())
	if c.frameSize != 1<<16 || c.frames != 256 {
		t.Errorf("plain WithGSO gave %d frames of %d, want 256 of 65536", c.frames, c.frameSize)
	}
}

// The same two options in either order must reserve the same memory. They did
// not: WithGSO consulted whether WithFrameSize had run yet, so listing it
// first cut the frame count to 256 and listing it second left it at 4096 --
// the same 64 KB frames, sixteen times the region, and no complaint from
// validate because both sizes are legal.
func TestFrameSizingDoesNotDependOnOptionOrder(t *testing.T) {
	build := func(opts ...Option) config {
		c := defaults()
		for _, o := range opts {
			o(&c)
		}
		c.resolve()
		return c
	}

	a := build(WithGSO(), WithFrameSize(1<<16))
	b := build(WithFrameSize(1<<16), WithGSO())
	if a.frames != b.frames || a.frameSize != b.frameSize {
		t.Errorf("order changed the region: gso-first %d x %d = %d MiB, "+
			"size-first %d x %d = %d MiB",
			a.frames, a.frameSize, int64(a.frames)*int64(a.frameSize)>>20,
			b.frames, b.frameSize, int64(b.frames)*int64(b.frameSize)>>20)
	}
}
