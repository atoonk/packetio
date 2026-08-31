package mbuf

import "testing"

// pkt builds an ethernet frame: dst, src, then the tags and ethertype given,
// then payload.
func pkt(tags []uint16, et uint16, payload ...byte) []byte {
	b := make([]byte, 12)
	for _, t := range tags {
		b = append(b, byte(t>>8), byte(t), 0x08, 0x07) // tag ethertype, then TCI
	}
	b = append(b, byte(et>>8), byte(et))
	return append(b, payload...)
}

// ipv4 is a header with the given IHL in 32-bit words and protocol.
func ipv4(ihl int, proto byte) []byte {
	h := make([]byte, ihl*4)
	h[0] = 0x40 | byte(ihl)
	h[9] = proto
	return h
}

func ipv6(next byte) []byte {
	h := make([]byte, 40)
	h[0] = 0x60
	h[6] = next
	return h
}

func TestParseUntagged(t *testing.T) {
	p := pkt(nil, etherIPv4, append(ipv4(5, ProtoUDP), make([]byte, 8)...)...)
	h, ok := Parse(p)
	if !ok {
		t.Fatal("a plain IPv4/UDP frame must parse")
	}
	if h.L2Len != 14 || h.L3Len != 20 || !h.IPv4 || h.Proto != ProtoUDP {
		t.Fatalf("got %+v, want L2 14 L3 20 IPv4 UDP", h)
	}
}

// The tagged cases are the ones that matter here: this library sends tagged
// traffic by default, and counting a tag into L2Len is the difference between a
// checksum written into the L4 header and one written four bytes short of it.
func TestParseTagged(t *testing.T) {
	for _, tc := range []struct {
		name string
		tags []uint16
		want int
	}{
		{"one 802.1Q tag", []uint16{etherVLAN}, 18},
		{"QinQ, 0x88a8 outer", []uint16{etherQinQ, etherVLAN}, 22},
		{"QinQ, 0x9100 outer", []uint16{etherQinQ2, etherVLAN}, 22},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pkt(tc.tags, etherIPv4, append(ipv4(5, ProtoTCP), make([]byte, 20)...)...)
			h, ok := Parse(p)
			if !ok {
				t.Fatal("must parse")
			}
			if h.L2Len != tc.want {
				t.Errorf("L2Len %d, want %d: a tag not counted puts the NIC's "+
					"checksum in the wrong place", h.L2Len, tc.want)
			}
			if h.L3Len != 20 || !h.IPv4 || h.Proto != ProtoTCP {
				t.Errorf("got %+v, want L3 20 IPv4 TCP", h)
			}
		})
	}
}

func TestParseOptionsAndV6(t *testing.T) {
	// IPv4 with options: L3Len must follow IHL, not the usual 20.
	p := pkt(nil, etherIPv4, append(ipv4(8, ProtoTCP), make([]byte, 20)...)...)
	if h, ok := Parse(p); !ok || h.L3Len != 32 {
		t.Errorf("IHL 8 gave %+v ok=%v, want L3Len 32", h, ok)
	}
	p = pkt([]uint16{etherVLAN}, etherIPv6, append(ipv6(ProtoUDP), make([]byte, 8)...)...)
	h, ok := Parse(p)
	if !ok || !h.IPv6 || h.L2Len != 18 || h.L3Len != 40 || h.Proto != ProtoUDP {
		t.Errorf("tagged IPv6/UDP gave %+v ok=%v", h, ok)
	}
}

// Everything Parse refuses. Each of these would otherwise become a checksum
// written at an offset derived from a header that is not there.
func TestParseRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    []byte
	}{
		{"too short for ethernet", make([]byte, 13)},
		{"ARP, not IP", pkt(nil, 0x0806, make([]byte, 28)...)},
		{"truncated IPv4 header", pkt(nil, etherIPv4, make([]byte, 10)...)},
		{"IHL below 5", pkt(nil, etherIPv4,
			append([]byte{0x44, 0, 0, 0, 0, 0, 0, 0, 0, ProtoTCP},
				make([]byte, 30)...)...)},
		{"IPv4 header longer than the packet", pkt(nil, etherIPv4, ipv4(15, ProtoTCP)[:24]...)},
		{"version not 4", pkt(nil, etherIPv4, append([]byte{0x55}, make([]byte, 25)...)...)},
		{"ICMP, no L4 checksum offload", pkt(nil, etherIPv4,
			append(ipv4(5, 1), make([]byte, 8)...)...)},
		{"IPv6 with an extension header", pkt(nil, etherIPv6,
			append(ipv6(43), make([]byte, 8)...)...)},
		{"tag with nothing after it", pkt([]uint16{etherVLAN}, etherVLAN)[:16]},
		{"no room for an L4 header", pkt(nil, etherIPv4, ipv4(5, ProtoUDP)...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if h, ok := Parse(tc.p); ok {
				t.Errorf("parsed %+v, but this must be refused rather than guessed", h)
			}
		})
	}
}

// A third tag is refused rather than walked: the loop is bounded because its
// input is packet bytes.
func TestParseBoundedTagWalk(t *testing.T) {
	p := pkt([]uint16{etherVLAN, etherVLAN, etherVLAN}, etherIPv4,
		append(ipv4(5, ProtoTCP), make([]byte, 20)...)...)
	if _, ok := Parse(p); ok {
		t.Error("a triple-tagged frame must be refused, not walked indefinitely")
	}
}

func TestChecksumFlags(t *testing.T) {
	v4tcp := ChecksumFlags(Headers{IPv4: true, Proto: ProtoTCP})
	if v4tcp&TxIPv4 == 0 || v4tcp&TxIPChecksum == 0 || v4tcp&TxL4Mask != TxTCPChecksum {
		t.Errorf("IPv4/TCP flags %#x", v4tcp)
	}
	v4udp := ChecksumFlags(Headers{IPv4: true, Proto: ProtoUDP})
	if v4udp&TxL4Mask != TxUDPChecksum {
		t.Errorf("IPv4/UDP L4 field %#x, want the UDP enumeration", v4udp&TxL4Mask)
	}
	// IPv6 has no header checksum; asking for one writes over payload.
	v6 := ChecksumFlags(Headers{IPv6: true, Proto: ProtoTCP})
	if v6&TxIPChecksum != 0 {
		t.Error("IPv6 must not ask for an IP header checksum")
	}
	if v6&TxIPv6 == 0 {
		t.Error("IPv6 must be declared, or the NIC guesses the version")
	}
	if ChecksumFlags(Headers{}) != 0 {
		t.Error("neither version must ask for nothing")
	}
}

func TestSetTxOffloadTSO(t *testing.T) {
	b := make([]byte, Size)
	SetTxOffloadTSO(b, 0, 18, 20, 32, 1448)
	got := TxOffload(b, 0)
	if got&0x7f != 18 {
		t.Errorf("l2_len %d", got&0x7f)
	}
	if (got>>7)&0x1ff != 20 {
		t.Errorf("l3_len %d", (got>>7)&0x1ff)
	}
	if (got>>16)&0xff != 32 {
		t.Errorf("l4_len %d", (got>>16)&0xff)
	}
	if (got>>24)&0xffff != 1448 {
		t.Errorf("tso_segsz %d", (got>>24)&0xffff)
	}
	// The fields must not bleed into each other: an over-wide value is masked,
	// not allowed to corrupt its neighbour.
	SetTxOffloadTSO(b, 0, 0xffff, 0xffff, 0xffff, 0xffffff)
	got = TxOffload(b, 0)
	if got&0x7f != 0x7f || (got>>7)&0x1ff != 0x1ff || (got>>16)&0xff != 0xff {
		t.Errorf("masking failed: %#x", got)
	}
}

// A packet carrying only part of its L4 header must be refused. The NIC writes
// the checksum at a fixed offset into that header -- six bytes in for UDP,
// sixteen for TCP -- so accepting one has the card write past the packet.
func TestParseRefusesAPartialL4Header(t *testing.T) {
	for _, tc := range []struct {
		name  string
		proto byte
		l4    int
		want  bool
	}{
		{"UDP with 4 of 8 bytes", ProtoUDP, 4, false},
		{"UDP with all 8", ProtoUDP, 8, true},
		{"TCP with 4 of 20 bytes", ProtoTCP, 4, false},
		{"TCP with 19 of 20", ProtoTCP, 19, false},
		{"TCP with all 20", ProtoTCP, 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pkt(nil, etherIPv4, append(ipv4(5, tc.proto), make([]byte, tc.l4)...)...)
			if _, ok := Parse(p); ok != tc.want {
				t.Errorf("Parse ok = %v, want %v", ok, tc.want)
			}
		})
	}
}
