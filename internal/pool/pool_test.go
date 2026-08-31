package pool

import "testing"

func TestPopPush(t *testing.T) {
	p := New(0, 4, 2048)
	if got := p.Len(); got != 4 {
		t.Fatalf("Len = %d, want 4", got)
	}

	var dst []uint64
	dst = p.Pop(3, dst)
	if len(dst) != 3 {
		t.Fatalf("popped %d, want 3", len(dst))
	}
	if p.Len() != 1 {
		t.Fatalf("Len after Pop = %d, want 1", p.Len())
	}

	// Asking for more than is free yields only what is free.
	dst = p.Pop(10, dst[:0])
	if len(dst) != 1 {
		t.Fatalf("popped %d, want 1", len(dst))
	}
	if p.Len() != 0 {
		t.Fatalf("Len = %d, want 0", p.Len())
	}

	// An empty pool yields nothing rather than panicking.
	if got := p.Pop(5, nil); len(got) != 0 {
		t.Fatalf("Pop on empty returned %d", len(got))
	}
	if got := p.Pop(0, nil); len(got) != 0 {
		t.Fatalf("Pop(0) returned %d", len(got))
	}
	if got := p.Pop(-1, nil); len(got) != 0 {
		t.Fatalf("Pop(-1) returned %d", len(got))
	}

	p.Push(4096)
	if p.Len() != 1 {
		t.Fatalf("Len after Push = %d, want 1", p.Len())
	}
}

// A pool must hand out every frame of its range exactly once, and no other.
func TestPopCoversRangeExactlyOnce(t *testing.T) {
	const base, count, frameSize = 3, 5, 2048
	p := New(base, count, frameSize)

	seen := map[uint64]int{}
	for _, a := range p.Pop(count, nil) {
		seen[a]++
	}
	if len(seen) != count {
		t.Fatalf("got %d distinct frames, want %d", len(seen), count)
	}
	for i := base; i < base+count; i++ {
		addr := uint64(i * frameSize)
		if seen[addr] != 1 {
			t.Fatalf("frame at %d handed out %d times, want 1", addr, seen[addr])
		}
		if !p.Owns(addr) {
			t.Fatalf("Owns(%d) = false, want true", addr)
		}
	}
	if p.Owns(uint64((base - 1) * frameSize)) {
		t.Fatal("Owns claims a frame below the pool")
	}
	if p.Owns(uint64((base + count) * frameSize)) {
		t.Fatal("Owns claims a frame above the pool")
	}
}

// Two pools carved from one region must never share a frame: this is the
// property that lets a receive goroutine and a transmit goroutine run without
// a lock.
func TestSplitPoolsDisjoint(t *testing.T) {
	const numFrames, txFrames, frameSize = 8, 3, 2048
	rxFrames := numFrames - txFrames
	rx := New(0, rxFrames, frameSize)
	tx := New(rxFrames, txFrames, frameSize)

	seen := map[uint64]string{}
	for _, a := range rx.Pop(rxFrames, nil) {
		seen[a] = "rx"
	}
	for _, a := range tx.Pop(txFrames, nil) {
		if who, ok := seen[a]; ok {
			t.Fatalf("frame %d is in both the %s and tx pools", a, who)
		}
		seen[a] = "tx"
	}
	if len(seen) != numFrames {
		t.Fatalf("covered %d frames, want %d", len(seen), numFrames)
	}
	for addr, who := range seen {
		if who == "tx" && addr < uint64(rxFrames*frameSize) {
			t.Fatalf("tx frame %d overlaps the rx region [0,%d)", addr, rxFrames*frameSize)
		}
		if rx.Owns(addr) == tx.Owns(addr) {
			t.Fatalf("frame %d is owned by both pools or neither", addr)
		}
	}
}

// Base recovers the frame a descriptor belongs to whatever headroom it used,
// so a frame handed back after receive returns to the slot it came from.
func TestBase(t *testing.T) {
	p := New(0, 4, 2048)
	for _, tc := range []struct{ addr, want uint64 }{
		{0, 0}, {1, 0}, {2047, 0}, {2048, 2048}, {2048 + 64, 2048}, {6143, 4096},
	} {
		if got := p.Base(tc.addr); got != tc.want {
			t.Errorf("Base(%d) = %d, want %d", tc.addr, got, tc.want)
		}
	}
}

// The packet path must not allocate: Pop into a slice with capacity, Push back.
func TestPopPushDoNotAllocate(t *testing.T) {
	p := New(0, 64, 2048)
	dst := make([]uint64, 0, 64)
	allocs := testing.AllocsPerRun(100, func() {
		dst = p.Pop(32, dst[:0])
		for _, a := range dst {
			p.Push(a)
		}
	})
	if allocs != 0 {
		t.Fatalf("Pop/Push allocated %v times per run, want 0", allocs)
	}
}

// Base must round to the frame start for every offset inside a frame, which is
// what a masked implementation has to get right at the boundaries.
func TestBaseIsAMask(t *testing.T) {
	for _, frameSize := range []int{64, 128, 2048, 4096} {
		p := New(3, 4, frameSize)
		for f := 3; f < 7; f++ {
			start := uint64(f) * uint64(frameSize)
			for _, off := range []uint64{0, 1, 63, uint64(frameSize) / 2, uint64(frameSize) - 1} {
				if got := p.Base(start + off); got != start {
					t.Fatalf("frame size %d: Base(%d) = %d, want %d", frameSize, start+off, got, start)
				}
			}
		}
	}
}

// A frame returned twice is refused the second time. Counting alone cannot
// catch this: the pool can be the right length with one frame in it twice and
// another missing, and then hands one frame to two owners.
func TestPushRefusesAFrameAlreadyFree(t *testing.T) {
	if !Checked() {
		t.Skip("duplicate detection is a checked build: run with -tags packetio_checked")
	}
	p := New(0, 4, 2048)
	got := p.Pop(2, nil)
	a := got[0]

	p.Push(a)
	if n := p.Rejected(); n != 0 {
		t.Fatalf("the first return was refused: rejected=%d", n)
	}
	p.Push(a)
	if n := p.Rejected(); n != 1 {
		t.Errorf("rejected=%d after returning one frame twice, want 1", n)
	}

	// The whole pool, and every frame in it exactly once.
	seen := map[uint64]int{}
	for _, f := range p.Pop(4, nil) {
		seen[f]++
	}
	for f, n := range seen {
		if n != 1 {
			t.Errorf("frame %d handed out %d times", f, n)
		}
	}
}

// An address in the middle of a frame is not a frame start. Accepting it would
// put an address on the free list that Alloc later hands out as a whole frame,
// so the packet built in it runs off the end.
func TestPushRefusesAnUnalignedAddress(t *testing.T) {
	p := New(0, 4, 2048)
	p.Pop(4, nil)
	p.Push(2048 + 64)
	if n := p.Rejected(); n != 1 {
		t.Errorf("rejected=%d for an address inside a frame, want 1", n)
	}
	if p.Len() != 0 {
		t.Errorf("an unaligned address reached the free list: %d free", p.Len())
	}
}

// A pool whose frame count is not a multiple of 64 must not have spare bits
// that read as free, or a foreign push could land on one.
func TestPushRefusesPastAPartialWord(t *testing.T) {
	const count = 70
	p := New(0, count, 2048)
	p.Pop(count, nil)
	for _, addr := range []uint64{count * 2048, (count + 1) * 2048} {
		p.Push(addr)
	}
	if n := p.Rejected(); n != 2 {
		t.Errorf("rejected=%d for two frames past the end, want 2", n)
	}
	if p.Len() != 0 {
		t.Errorf("%d frames on the list, want 0", p.Len())
	}
}

// The bitmap must survive many round trips: every frame out and back, checked
// for duplicates each time.
func TestPushPopManyRounds(t *testing.T) {
	const count = 130
	p := New(3, count, 4096)
	for round := 0; round < 50; round++ {
		got := p.Pop(count, nil)
		if len(got) != count {
			t.Fatalf("round %d: popped %d, want %d", round, len(got), count)
		}
		for _, f := range got {
			p.Push(f)
		}
		if p.Len() != count {
			t.Fatalf("round %d: %d free, want %d", round, p.Len(), count)
		}
	}
	if n := p.Rejected(); n != 0 {
		t.Errorf("rejected=%d over 50 clean rounds, want 0", n)
	}
}
