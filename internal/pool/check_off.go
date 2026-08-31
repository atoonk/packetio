//go:build !packetio_checked

package pool

// The unchecked build. Returning a frame twice is a caller bug that the pool
// cannot catch for free: it needs a bit per frame, and maintaining it is a
// read-modify-write on both Pop and Push. Measured on a ConnectX-6 Dx that cost
// 14.4 cycles a packet out of 95 -- fifteen per cent of the transmit path, to
// detect a violation of a documented contract after the fact.
//
// So it is on in tests and off in production. The tests, including the shared
// conformance suite, run both ways; -tags packetio_checked turns it on. What
// stays on everywhere is the cheap half: a frame outside this pool, one that is
// not a frame start, and a push past the pool's capacity are all refused and
// counted, and those are the ones that corrupt memory rather than merely
// accounting for it wrongly.
//
// The guard that actually stops bad memory reaching the wire is in the backend,
// not here: mlx5 refuses a descriptor that does not name bytes inside one
// frame, on every Transmit, and that check is never compiled out.

func (p *Frames) initChecks(int) {}

// markFree reports whether addr may be added to the free list. Unchecked, so
// it always may.
func (p *Frames) markFree(uint64) bool { return true }

// markTaken records that these frames have left the free list.
func (p *Frames) markTaken([]uint64) {}

// Checked reports whether this build detects a frame returned twice.
func Checked() bool { return false }
