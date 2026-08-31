package pool

import "testing"

// The shift must agree with the divide it replaced, for every frame of a pool
// and for every address inside each frame.
func TestIndexIsTheDivideItReplaced(t *testing.T) {
	for _, fs := range []int{64, 2048, 4096, 65536} {
		for _, base := range []int{0, 1, 7, 1000} {
			p := New(base, 64, fs)
			for f := 0; f < 64; f++ {
				for _, off := range []uint64{0, 1, uint64(fs) - 1} {
					addr := uint64(base+f)*uint64(fs) + off
					want := (addr - p.base) / uint64(p.frameSize)
					if got := p.index(addr); got != want {
						t.Fatalf("frameSize=%d base=%d addr=%d: index=%d want %d", fs, base, addr, got, want)
					}
				}
			}
		}
	}
}
