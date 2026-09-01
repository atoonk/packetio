// Package clock turns the tick counter in a completion into nanoseconds.
//
// It is its own package, with no cgo in it, so that the arithmetic can be
// tested: the conversion lives on the receive path of the fastest backend and
// is the easiest thing in it to get quietly wrong.
package clock

import "math/bits"

// Info is how a device's timestamps convert, as the driver reports it: a
// duration is ticks * Mult >> Shift, and the counter is Mask bits wide.
type Info struct {
	Mult  uint32
	Shift uint32
	Mask  uint64
}

// OK reports whether the device gave a usable conversion. A device that does
// not report a clock leaves these zero, and every timestamp it produced would
// convert to zero -- which is why a queue must not offer timestamps at all
// unless this is true.
func (i Info) OK() bool { return i.Mult != 0 && i.Mask != 0 }

// A Counter converts the ticks of one queue's completions to nanoseconds,
// carrying the wraps.
//
// The counter in a completion is narrow -- 41 bits on a ConnectX-6 Dx, which
// at a nanosecond a tick comes back round every 37 minutes -- so scaling it on
// its own gives a time that jumps backwards by half an hour several times a
// day, and one interval of about six hundred years each time it does. Anything
// watching a link for longer than half an hour meets this, and the whole point
// of a timestamp is the interval between two of them.
//
// So the wraps are counted here and added back, which makes the result rise
// for as long as the queue is open. A Counter belongs to one queue and is not
// safe to share, like everything else on the receive path.
type Counter struct {
	info  Info
	last  uint64 // the previous raw tick, masked
	wraps uint64
	begun bool
}

// New prepares a Counter for a device's clock.
func New(i Info) *Counter { return &Counter{info: i} }

// NS converts one completion's ticks.
//
// Timestamps that step backwards a little are ordinary: a card stamps a packet
// at the port and places it in a queue afterwards, so under loss the two
// orders come apart. A wrap is a step backwards of half the counter, which is
// eighteen minutes here, so the two cannot be confused by anything the
// hardware does.
func (c *Counter) NS(ticks uint64) uint64 {
	raw := ticks & c.info.Mask
	if c.begun && raw < c.last && c.last-raw > c.info.Mask/2 {
		c.wraps++
	}
	// Always, including after a wrap: leaving last at the old high value would
	// make every packet after the wrap look like another one.
	c.last = raw
	c.begun = true

	// Mask+1 is the counter's period. The product is wider than 64 bits, so
	// the multiply is done in 128.
	hi, lo := bits.Mul64(c.wraps*(c.info.Mask+1)+raw, uint64(c.info.Mult))
	if c.info.Shift == 0 {
		return lo
	}
	return hi<<(64-c.info.Shift) | lo>>c.info.Shift
}
