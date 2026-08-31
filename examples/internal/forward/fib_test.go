package forward

import (
	"math/rand"
	"testing"
)

func ip(a, b, c, d byte) uint32 { return uint32(a)<<24 | uint32(b)<<16 | uint32(c)<<8 | uint32(d) }

// The longest prefix wins, whatever order the routes went in.
func TestLongestPrefixWins(t *testing.T) {
	for _, order := range [][]int{{0, 1, 2, 3}, {3, 2, 1, 0}, {2, 0, 3, 1}} {
		routes := []struct {
			prefix uint32
			plen   uint8
			adj    uint32
		}{
			{ip(10, 0, 0, 0), 8, 1},
			{ip(10, 1, 0, 0), 16, 2},
			{ip(10, 1, 2, 0), 24, 3},
			{ip(10, 1, 2, 3), 32, 4},
		}
		f := NewV4Fib()
		for _, i := range order {
			f.Insert(routes[i].prefix, routes[i].plen, routes[i].adj)
		}
		for _, c := range []struct {
			dst  uint32
			want uint32
		}{
			{ip(10, 200, 1, 1), 1},
			{ip(10, 1, 200, 1), 2},
			{ip(10, 1, 2, 200), 3},
			{ip(10, 1, 2, 3), 4},
			{ip(11, 1, 2, 3), 0},
			{ip(9, 255, 255, 255), 0},
		} {
			if got := f.Lookup(c.dst); got != c.want {
				t.Errorf("order %v: lookup %08x = %d, want %d", order, c.dst, got, c.want)
			}
		}
		if f.Routes() != 4 {
			t.Errorf("%d routes, want 4", f.Routes())
		}
	}
}

func TestDefaultRoute(t *testing.T) {
	f := NewV4Fib()
	f.Insert(0, 0, 7)
	f.Insert(ip(192, 168, 0, 0), 16, 8)
	if got := f.Lookup(ip(1, 2, 3, 4)); got != 7 {
		t.Errorf("default route gave %d", got)
	}
	if got := f.Lookup(ip(192, 168, 43, 1)); got != 8 {
		t.Errorf("the /16 under a default route gave %d", got)
	}
}

// A random table, checked against the obvious slow answer.
func TestAgainstLinearScan(t *testing.T) {
	type rt struct {
		prefix uint32
		plen   uint8
		adj    uint32
	}
	rng := rand.New(rand.NewSource(1))
	var routes []rt
	f := NewV4Fib()
	for i := 0; i < 10000; i++ {
		plen := uint8(8 + rng.Intn(25)) // 8..32
		// Keep the addresses in a few /8s so the routes overlap heavily.
		prefix := (ip(10, 0, 0, 0) | uint32(rng.Intn(4))<<24 | uint32(rng.Uint32())&0x00ffffff) & plenMask(plen)
		adj := uint32(i + 1)
		// A later route with the same prefix replaces an earlier one.
		replaced := false
		for j := range routes {
			if routes[j].prefix == prefix && routes[j].plen == plen {
				routes[j].adj = adj
				replaced = true
			}
		}
		if !replaced {
			routes = append(routes, rt{prefix, plen, adj})
		}
		f.Insert(prefix, plen, adj)
	}
	slow := func(dst uint32) uint32 {
		var best uint32
		bestLen := -1
		for _, r := range routes {
			if dst&plenMask(r.plen) == r.prefix && int(r.plen) > bestLen {
				best, bestLen = r.adj, int(r.plen)
			}
		}
		return best
	}
	for i := 0; i < 200000; i++ {
		dst := ip(10, 0, 0, 0) | uint32(rng.Intn(5))<<24 | uint32(rng.Uint32())&0x00ffffff
		if got, want := f.Lookup(dst), slow(dst); got != want {
			t.Fatalf("lookup %08x = %d, want %d", dst, got, want)
		}
	}
}

func TestAdjacencyHeader(t *testing.T) {
	dst := [6]byte{0x7c, 0xc2, 0x55, 0xbe, 0xf4, 0xe1}
	src := [6]byte{0x7c, 0xc2, 0x55, 0xbe, 0xf3, 0xc7}
	a := NewAdjacency(dst, src, 2043)
	want := []byte{0x7c, 0xc2, 0x55, 0xbe, 0xf4, 0xe1, 0x7c, 0xc2, 0x55, 0xbe, 0xf3, 0xc7, 0x81, 0x00, 0x07, 0xfb, 0x08, 0x00}
	if a.L2Len != 18 || string(a.L2[:a.L2Len]) != string(want) {
		t.Errorf("tagged header % x (%d), want % x", a.L2[:a.L2Len], a.L2Len, want)
	}
	a = NewAdjacency(dst, src, 0)
	if a.L2Len != 14 || string(a.L2[12:14]) != "\x08\x00" {
		t.Errorf("untagged header % x (%d)", a.L2[:a.L2Len], a.L2Len)
	}
}

func BenchmarkLookup(b *testing.B) {
	f := NewV4Fib()
	f.Insert(ip(192, 168, 43, 0), 24, 1)
	f.Insert(ip(10, 0, 0, 0), 8, 2)
	dsts := []uint32{ip(192, 168, 43, 7), ip(10, 1, 2, 3), ip(192, 168, 43, 200)}
	var sink uint32
	for i := 0; i < b.N; i++ {
		sink += f.Lookup(dsts[i%3])
	}
	_ = sink
}
