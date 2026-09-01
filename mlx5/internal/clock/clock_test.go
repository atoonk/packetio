package clock

import (
	"math/big"
	"testing"
)

// The real ConnectX-6 Dx reports these: a nanosecond a tick, and a counter 41
// bits wide that comes back round every 2199 seconds.
var cx6 = Info{Mult: 2147483648, Shift: 31, Mask: 1<<41 - 1}

// The conversion must match exact arithmetic, not approximately.
func TestNSMatchesExactArithmetic(t *testing.T) {
	for _, i := range []Info{
		cx6,
		{Mult: 1, Shift: 0, Mask: 1<<41 - 1},
		{Mult: 1 << 20, Shift: 20, Mask: 1<<41 - 1},
		{Mult: 268435456, Shift: 28, Mask: 1<<48 - 1},
		{Mult: 1 << 30, Shift: 31, Mask: 1<<63 - 1},
	} {
		c := New(i)
		for _, ticks := range []uint64{0, 1, 4, 1 << 20, 1 << 30, i.Mask / 3, i.Mask / 2, i.Mask - 1} {
			// Each value on a fresh counter, so no wrap is involved.
			c = New(i)
			got := c.NS(ticks)
			want := new(big.Int).Mul(new(big.Int).SetUint64(ticks&i.Mask), new(big.Int).SetUint64(uint64(i.Mult)))
			want.Rsh(want, uint(i.Shift))
			if !want.IsUint64() || got != want.Uint64() {
				t.Errorf("mult=%d shift=%d ticks=%d: got %d, want %v", i.Mult, i.Shift, ticks, got, want)
			}
		}
	}
}

// The counter wraps every 37 minutes on this card. Time must keep going
// forwards across it, and the interval across the wrap must be the real one.
func TestTimeCrossesTheWrap(t *testing.T) {
	c := New(cx6)
	const step = 1000 // ticks, a microsecond apart

	last := c.NS(cx6.Mask - 2*step)
	for i := 0; i < 6; i++ {
		// Walk over the wrap: ...Mask-step, Mask, then round to 0, step, ...
		ticks := (cx6.Mask - 2*step + uint64(i+1)*step) & cx6.Mask
		ns := c.NS(ticks)
		if ns <= last {
			t.Fatalf("step %d: time went backwards or stood still: %d after %d", i, ns, last)
		}
		if d := ns - last; d != step {
			t.Errorf("step %d: interval %d ns, want %d", i, d, step)
		}
		last = ns
	}
}

// A packet that arrives out of order steps the stamp back by microseconds, not
// by half the counter, and must not be mistaken for a wrap -- nor be allowed
// to move the point the wrap is measured from.
func TestOutOfOrderIsNotAWrap(t *testing.T) {
	c := New(cx6)
	base := cx6.Mask - 10000

	a := c.NS(base)
	b := c.NS(base + 2000)
	back := c.NS(base + 1000) // arrived late, stamped earlier
	on := c.NS(base + 3000)

	if b <= a || on <= b {
		t.Fatalf("ordinary packets did not advance: %d %d %d", a, b, on)
	}
	if back >= b || back <= a {
		t.Errorf("the late packet should sit between the two: got %d, between %d and %d", back, a, b)
	}
	// The wrap counter must not have moved: the next real wrap has to be
	// detected correctly after this.
	if c.wraps != 0 {
		t.Errorf("an out-of-order packet was counted as %d wraps", c.wraps)
	}
	if next := c.NS(5000); next <= on {
		t.Errorf("the wrap after a late packet was missed: %d follows %d", next, on)
	}
}

// A device that reports no clock produces zeroes, which is why the queue must
// not offer timestamps at all in that case.
func TestNoClockIsNotUsable(t *testing.T) {
	if (Info{}).OK() {
		t.Error("a zero clock reports itself usable")
	}
	if (Info{Mult: 1, Mask: 0}).OK() {
		t.Error("a clock with no counter reports itself usable")
	}
	if !cx6.OK() {
		t.Error("the ConnectX-6 Dx clock reports itself unusable")
	}
}
