//go:build packetio_checked

package pool

// The checked build, selected with -tags packetio_checked.
//
// One bit per frame says whether it is on the free list, so a frame returned
// twice is refused and counted rather than appended twice. Counting alone
// cannot do it: a pool one frame short because the caller lost one, then given
// another back twice, is the right length with one frame in it twice and one
// missing -- and hands that frame to two owners on the next Alloc. That is the
// aliasing corruption this package exists to prevent, and it is invisible in
// every other counter.
//
// It is not free, which is why it is a build tag rather than always on. See
// check_off.go.

func (p *Frames) initChecks(count int) {
	p.in = make([]uint64, (count+63)/64)
	for i := range p.in {
		p.in[i] = ^uint64(0) // every frame starts on the free list
	}
	// The bits past the last frame belong to no frame, and must not read as
	// free, or a foreign push could land on one.
	if r := count % 64; r != 0 {
		p.in[len(p.in)-1] = 1<<uint(r) - 1
	}
}

// markFree reports whether addr may be added to the free list, and records that
// it is there. It is false when the frame is already free.
func (p *Frames) markFree(addr uint64) bool {
	i := p.index(addr)
	if p.in[i>>6]&(1<<(i&63)) != 0 {
		return false
	}
	p.in[i>>6] |= 1 << (i & 63)
	return true
}

// markTaken records that these frames have left the free list.
func (p *Frames) markTaken(addrs []uint64) {
	for _, addr := range addrs {
		i := p.index(addr)
		p.in[i>>6] &^= 1 << (i & 63)
	}
}

// Checked reports whether this build detects a frame returned twice.
func Checked() bool { return true }
