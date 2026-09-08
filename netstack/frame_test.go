//go:build linux

package netstack

import "testing"

func TestParseFrame(t *testing.T) {
	untagged := make([]byte, 60)
	untagged[12], untagged[13] = 0x08, 0x00
	tagged := make([]byte, 64)
	tagged[12], tagged[13] = 0x81, 0x00
	tagged[14], tagged[15] = 0x08, 0x53 // PCP 0, VID 2131
	tagged[16], tagged[17] = 0x08, 0x00
	prio := append([]byte(nil), tagged...)
	prio[14] |= 0xa0 // PCP 5: must still match the VID

	cases := []struct {
		name   string
		f      []byte
		vlan   uint16
		hdr    int
		et     uint16
		wantOK bool
	}{
		{"untagged on untagged", untagged, 0, 14, 0x0800, true},
		{"untagged on tagged", untagged, 2131, 0, 0, false},
		{"tagged on tagged", tagged, 2131, 18, 0x0800, true},
		{"tagged with priority", prio, 2131, 18, 0x0800, true},
		{"tagged other vlan", tagged, 2154, 0, 0, false},
		{"tagged on untagged", tagged, 0, 0, 0, false},
		{"runt", untagged[:13], 0, 0, 0, false},
		{"runt tagged", tagged[:17], 2131, 0, 0, false},
	}
	for _, c := range cases {
		hdr, et, ok := parseFrame(c.f, c.vlan)
		if ok != c.wantOK || hdr != c.hdr || et != c.et {
			t.Errorf("%s: parseFrame = (%d, %#x, %v), want (%d, %#x, %v)",
				c.name, hdr, et, ok, c.hdr, c.et, c.wantOK)
		}
	}
}

func TestPutHeaderRoundTrip(t *testing.T) {
	dst := [6]byte{1, 2, 3, 4, 5, 6}
	src := [6]byte{0xa, 0xb, 0xc, 0xd, 0xe, 0xf}
	for _, vlan := range []uint16{0, 2131, 4095} {
		b := make([]byte, 64)
		n := putHeader(b, dst, src, vlan, etherTypeIPv4)
		if n != linkHeaderLen(vlan) {
			t.Fatalf("vlan %d: wrote %d bytes, want %d", vlan, n, linkHeaderLen(vlan))
		}
		hdr, et, ok := parseFrame(b, vlan)
		if !ok || hdr != n || et != etherTypeIPv4 {
			t.Fatalf("vlan %d: parse of own header = (%d, %#x, %v)", vlan, hdr, et, ok)
		}
		if [6]byte(b[0:6]) != dst || [6]byte(b[6:12]) != src {
			t.Fatalf("vlan %d: MACs not written", vlan)
		}
	}
}

func TestNeighbors(t *testing.T) {
	n := newNeighbors()
	ip := [4]byte{10, 0, 0, 1}
	if _, ok := n.lookup(ip); ok {
		t.Fatal("empty table answered")
	}
	m1 := [6]byte{1, 1, 1, 1, 1, 1}
	n.learn(ip, m1)
	n.learn(ip, m1) // second time takes the read-only path
	if got, ok := n.lookup(ip); !ok || got != m1 {
		t.Fatalf("lookup = %v, %v", got, ok)
	}
	m2 := [6]byte{2, 2, 2, 2, 2, 2}
	n.lastWrite.Store(0)
	n.learn(ip, m2) // a peer that moved
	if got, _ := n.lookup(ip); got != m2 {
		t.Fatalf("update not applied: %v", got)
	}
	m3 := [6]byte{3, 3, 3, 3, 3, 3}
	if changed, _, _ := n.learn(ip, m3); changed {
		t.Fatal("a second change within writeInterval was not throttled")
	}
	if n.size() != 1 {
		t.Fatalf("size = %d", n.size())
	}

	// A pinned entry: learning cannot change it, pinning again can.
	gw := [4]byte{10, 0, 0, 254}
	n.lastWrite.Store(0)
	n.pin(gw, m1)
	n.lastWrite.Store(0)
	if changed, _, _ := n.learn(gw, m2); changed {
		t.Fatal("learning overwrote a pinned entry")
	}
	if got, _ := n.lookup(gw); got != m1 {
		t.Fatalf("pinned entry = %v, want %v", got, m1)
	}
	n.lastWrite.Store(0)
	if changed, _, _ := n.pin(gw, m2); !changed {
		t.Fatal("pinning again did not change the entry")
	}
}

// TestNeighborsFull is the table at its bound: a new source evicts a learned
// entry, never a pinned one, and at most one per writeInterval.
func TestNeighborsFull(t *testing.T) {
	n := newNeighbors()
	gw := [4]byte{10, 255, 0, 1}
	n.pin(gw, [6]byte{9, 9, 9, 9, 9, 9})
	for i := 1; i < maxNeighbors; i++ {
		n.learn([4]byte{10, 0, byte(i >> 8), byte(i)}, [6]byte{1, 2, 3, 4, byte(i >> 8), byte(i)})
	}
	if n.size() != maxNeighbors {
		t.Fatalf("size = %d, want %d", n.size(), maxNeighbors)
	}
	n.lastWrite.Store(0)
	changed, evicted, didEvict := n.learn([4]byte{10, 1, 0, 1}, [6]byte{5, 5, 5, 5, 5, 5})
	if !changed || !didEvict || evicted == gw {
		t.Fatalf("full table: changed %v evicted %v (%v)", changed, evicted, didEvict)
	}
	if changed, _, _ := n.learn([4]byte{10, 1, 0, 2}, [6]byte{6, 6, 6, 6, 6, 6}); changed {
		t.Fatal("a second eviction within writeInterval was not throttled")
	}
	if got, ok := n.lookup(gw); !ok || got != [6]byte{9, 9, 9, 9, 9, 9} {
		t.Fatal("the pinned entry did not survive the full table")
	}
}
