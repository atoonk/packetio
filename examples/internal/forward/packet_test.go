package forward

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"testing"
)

// build makes a frame: Ethernet with the given tags, an IPv4 header with a
// valid checksum, and payload bytes to reach the total length.
func build(tags []uint16, ttl byte, payload int) []byte {
	b := make([]byte, 0, 128)
	b = append(b, 0x7c, 0xc2, 0x55, 0xbe, 0xf3, 0xc7) // dst
	b = append(b, 0x7c, 0xc2, 0x55, 0xbe, 0xf4, 0xe1) // src
	for i, tag := range tags {
		et := uint16(etherTypeVLAN)
		if len(tags) == 2 && i == 0 {
			et = etherTypeQinQ
		}
		b = binary.BigEndian.AppendUint16(b, et)
		b = binary.BigEndian.AppendUint16(b, tag)
	}
	b = binary.BigEndian.AppendUint16(b, etherTypeIPv4)
	h := make([]byte, ipv4HeaderLen)
	h[0] = 0x45
	binary.BigEndian.PutUint16(h[2:], uint16(ipv4HeaderLen+payload))
	binary.BigEndian.PutUint16(h[4:], 0x1234)
	h[8] = ttl
	h[9] = 17
	copy(h[12:], []byte{192, 168, 53, 1})
	copy(h[16:], []byte{192, 168, 43, 2})
	binary.BigEndian.PutUint16(h[10:], ipv4Checksum(h))
	b = append(b, h...)
	for i := 0; i < payload; i++ {
		b = append(b, byte(i))
	}
	return b
}

func TestParse(t *testing.T) {
	for _, c := range []struct {
		name string
		pkt  []byte
		l3   int
		code ErrorCode
	}{
		{"untagged", build(nil, 64, 22), 14, ErrNone},
		{"one tag", build([]uint16{2053}, 64, 22), 18, ErrNone},
		{"two tags", build([]uint16{100, 2053}, 64, 22), 22, ErrNone},
		{"ttl 1", build([]uint16{2053}, 1, 22), 0, ErrTTL},
		{"ttl 0", build([]uint16{2053}, 0, 22), 0, ErrTTL},
		{"ttl 2 is forwardable", build([]uint16{2053}, 2, 22), 18, ErrNone},
		{"short", build(nil, 64, 22)[:30], 0, ErrShort},
		{"tagged and short", build([]uint16{2053}, 64, 22)[:36], 0, ErrShort},
		{"empty", nil, 0, ErrShort},
	} {
		l3, dst, code := Parse(c.pkt, false)
		if code != c.code || l3 != c.l3 {
			t.Errorf("%s: parse gave l3 %d, %v; want %d, %v", c.name, l3, code, c.l3, c.code)
		}
		if code == ErrNone && dst != ip(192, 168, 43, 2) {
			t.Errorf("%s: destination %08x", c.name, dst)
		}
	}

	t.Run("not ipv4", func(t *testing.T) {
		p := build([]uint16{2053}, 64, 22)
		binary.BigEndian.PutUint16(p[16:], 0x86dd)
		if _, _, code := Parse(p, false); code != ErrNotIPv4 {
			t.Errorf("got %v", code)
		}
	})
	t.Run("bad version", func(t *testing.T) {
		p := build(nil, 64, 22)
		p[14] = 0x65
		if _, _, code := Parse(p, false); code != ErrBadVersion {
			t.Errorf("got %v", code)
		}
	})
	t.Run("header length below 20", func(t *testing.T) {
		p := build(nil, 64, 22)
		p[14] = 0x44
		if _, _, code := Parse(p, false); code != ErrBadLength {
			t.Errorf("got %v", code)
		}
	})
	t.Run("total length past the frame", func(t *testing.T) {
		p := build(nil, 64, 22)
		binary.BigEndian.PutUint16(p[16:], 200)
		binary.BigEndian.PutUint16(p[24:], ipv4Checksum(p[14:34]))
		if _, _, code := Parse(p, false); code != ErrBadLength {
			t.Errorf("got %v", code)
		}
	})
	t.Run("total length below the header", func(t *testing.T) {
		p := build(nil, 64, 22)
		binary.BigEndian.PutUint16(p[16:], 12)
		binary.BigEndian.PutUint16(p[24:], ipv4Checksum(p[14:34]))
		if _, _, code := Parse(p, false); code != ErrBadLength {
			t.Errorf("got %v", code)
		}
	})
	t.Run("bad checksum", func(t *testing.T) {
		p := build(nil, 64, 22)
		p[24] ^= 0x01
		if _, _, code := Parse(p, false); code != ErrBadChecksum {
			t.Errorf("got %v", code)
		}
	})
	t.Run("options are accepted and checksummed", func(t *testing.T) {
		p := build(nil, 64, 22)
		h := make([]byte, 24)
		copy(h, p[14:34])
		h[0] = 0x46
		binary.BigEndian.PutUint16(h[2:], 24+22)
		binary.BigEndian.PutUint16(h[10:], ipv4Checksum(h))
		q := append(append(p[:14:14], h...), p[34:]...)
		if l3, _, code := Parse(q, false); code != ErrNone || l3 != 14 {
			t.Errorf("got l3 %d, %v", l3, code)
		}
		q[30] ^= 1 // inside the options
		binary.BigEndian.PutUint16(q[16:], 24+22)
		if _, _, code := Parse(q, false); code != ErrBadChecksum {
			t.Errorf("a corrupted option gave %v", code)
		}
	})
}

// The incremental checksum must agree with a full recompute for every TTL and
// for headers with any content.
func TestRewriteChecksum(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	adj := NewAdjacency([6]byte{1, 2, 3, 4, 5, 6}, [6]byte{7, 8, 9, 10, 11, 12}, 2043)
	for ttl := 2; ttl <= 255; ttl++ {
		for i := 0; i < 64; i++ {
			p := build([]uint16{2053}, byte(ttl), 22)
			buf := make([]byte, 2048)
			copy(buf, p)
			// Scramble everything in the header except version/IHL, total
			// length and the TTL, then make the checksum right again.
			h := buf[18:38]
			for j := 4; j < 20; j++ {
				if j != 8 && j != 10 && j != 11 {
					h[j] = byte(rng.Intn(256))
				}
			}
			binary.BigEndian.PutUint16(h[10:], ipv4Checksum(h))
			want := make([]byte, 20)
			copy(want, h)
			want[8]--
			binary.BigEndian.PutUint16(want[10:], ipv4Checksum(want))

			n := Rewrite(buf, 18, &adj)
			if n != 60 {
				t.Fatalf("ttl %d: length %d, want 60", ttl, n)
			}
			if !bytes.Equal(buf[18:38], want) {
				t.Fatalf("ttl %d: header % x, want % x", ttl, buf[18:38], want)
			}
			if _, _, code := Parse(buf[:n], false); code != ErrNone && code != ErrTTL {
				t.Fatalf("ttl %d: the rewritten packet does not parse: %v", ttl, code)
			}
		}
	}
}

func TestRewriteHeaderAndPayload(t *testing.T) {
	dst := [6]byte{0x7c, 0xc2, 0x55, 0xbe, 0xf4, 0xe1}
	src := [6]byte{0x7c, 0xc2, 0x55, 0xbe, 0xf3, 0xc7}
	tagged := NewAdjacency(dst, src, 2043)
	untagged := NewAdjacency(dst, src, 0)

	for _, c := range []struct {
		name    string
		in      []byte
		adj     *Adjacency
		wantLen int
	}{
		{"tagged to tagged, in place", build([]uint16{2053}, 64, 22), &tagged, 60},
		{"untagged to tagged, grows", build(nil, 64, 22), &tagged, 60},
		{"tagged to untagged, shrinks and pads", build([]uint16{2053}, 64, 22), &untagged, 60},
		{"double tagged to tagged", build([]uint16{100, 2053}, 64, 22), &tagged, 60},
		{"long packet", build([]uint16{2053}, 64, 1000), &tagged, 18 + 20 + 1000},
		{"padding on the way in is not forwarded", append(build([]uint16{2053}, 64, 22), 0xaa, 0xbb, 0xcc, 0xdd), &tagged, 60},
	} {
		buf := make([]byte, 2048)
		copy(buf, c.in)
		l3, _, code := Parse(c.in, false)
		if code != ErrNone {
			t.Fatalf("%s: %v", c.name, code)
		}
		payload := append([]byte(nil), c.in[l3+20:l3+20+22]...)
		n := Rewrite(buf, l3, c.adj)
		if n != c.wantLen {
			t.Errorf("%s: length %d, want %d", c.name, n, c.wantLen)
		}
		l2 := int(c.adj.L2Len)
		if !bytes.Equal(buf[:l2], c.adj.L2[:l2]) {
			t.Errorf("%s: header % x", c.name, buf[:l2])
		}
		if buf[l2+8] != 63 {
			t.Errorf("%s: ttl %d", c.name, buf[l2+8])
		}
		if !bytes.Equal(buf[l2+20:l2+42], payload) {
			t.Errorf("%s: payload moved wrong: % x", c.name, buf[l2+20:l2+42])
		}
		if got, _, code := Parse(buf[:n], false); code != ErrNone || got != l2 {
			t.Errorf("%s: the result does not parse: l3 %d, %v", c.name, got, code)
		}
		for i := l2 + 42; i < n && n == minFrame; i++ {
			if buf[i] != 0 {
				t.Errorf("%s: padding byte %d is %#x", c.name, i, buf[i])
				break
			}
		}
	}
}

func BenchmarkParseAndRewrite(b *testing.B) {
	adj := NewAdjacency([6]byte{1, 2, 3, 4, 5, 6}, [6]byte{7, 8, 9, 10, 11, 12}, 2043)
	buf := make([]byte, 2048)
	copy(buf, build([]uint16{2053}, 64, 22))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l3, _, code := Parse(buf[:60], false)
		if code != ErrNone {
			b.Fatal(code)
		}
		Rewrite(buf, l3, &adj)
		buf[l3+8] = 64 // put the TTL back, and its checksum
		buf[l3+10], buf[l3+11] = 0, 0
		binary.BigEndian.PutUint16(buf[l3+10:], ipv4Checksum(buf[l3:l3+20]))
	}
}

// When the NIC says the header checksum is right, parse must not spend the
// cycles adding it up again -- and must still reject everything else.
func TestParseTrustsTheNIC(t *testing.T) {
	p := build([]uint16{2053}, 64, 22)
	p[24] ^= 0x01 // break the checksum
	if _, _, code := Parse(p, false); code != ErrBadChecksum {
		t.Errorf("unchecked: got %v, want a bad checksum", code)
	}
	if l3, _, code := Parse(p, true); code != ErrNone || l3 != 18 {
		t.Errorf("trusted: got l3 %d, %v; want 18, forwarded", l3, code)
	}
	// Trusting the checksum must not weaken any other check.
	short := p[:30]
	if _, _, code := Parse(short, true); code != ErrShort {
		t.Errorf("short frame with a trusted checksum: %v", code)
	}
	ttl := build([]uint16{2053}, 1, 22)
	if _, _, code := Parse(ttl, true); code != ErrTTL {
		t.Errorf("expired ttl with a trusted checksum: %v", code)
	}
}
