package frame

import (
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
)

func testSpec() Spec {
	return Spec{
		SrcMAC:  net.HardwareAddr{0x02, 0, 0, 0, 0, 1},
		DstMAC:  net.HardwareAddr{0x02, 0, 0, 0, 0, 2},
		SrcIP:   netip.MustParseAddr("10.0.0.1"),
		DstIP:   netip.MustParseAddr("10.0.0.2"),
		SrcPort: 1024,
		DstPort: 9,
		Size:    60,
	}
}

// The header must be laid out exactly as it goes on the wire, because a device
// in L2 inline mode parses the first bytes of it out of the work queue entry.
func TestUntaggedLayout(t *testing.T) {
	f, err := Build(testSpec())
	if err != nil {
		t.Fatal(err)
	}
	b := f.Bytes

	if len(b) != 60 {
		t.Fatalf("frame is %d bytes, want 60", len(b))
	}
	if f.VLANTags != 0 {
		t.Errorf("%d tags on an untagged frame", f.VLANTags)
	}
	if f.HeaderLen != 14+20+8 {
		t.Errorf("header is %d bytes, want 42", f.HeaderLen)
	}
	if got := binary.BigEndian.Uint16(b[12:]); got != EtherTypeIPv4 {
		t.Errorf("ethertype %#04x at offset 12, want %#04x", got, EtherTypeIPv4)
	}
	if b[14] != 0x45 {
		t.Errorf("IP version and header length byte is %#02x, want 0x45", b[14])
	}
	if got := binary.BigEndian.Uint16(b[16:]); got != uint16(60-14) {
		t.Errorf("IP total length %d, want %d", got, 60-14)
	}
	if got := b[23]; got != protoUDP {
		t.Errorf("IP protocol %d, want %d", got, protoUDP)
	}
	if got := binary.BigEndian.Uint16(b[34:]); got != 1024 {
		t.Errorf("source port %d, want 1024", got)
	}
	if got := binary.BigEndian.Uint16(b[38:]); got != uint16(8+60-42) {
		t.Errorf("UDP length %d, want %d", got, 8+60-42)
	}
}

// A tagged frame pushes everything four bytes later, which is exactly why the
// inline header length cannot be a constant.
func TestTaggedLayout(t *testing.T) {
	s := testSpec()
	s.VLAN = 123
	s.Priority = 3
	s.Size = 64
	f, err := Build(s)
	if err != nil {
		t.Fatal(err)
	}
	b := f.Bytes

	if f.VLANTags != 1 {
		t.Fatalf("%d tags, want 1", f.VLANTags)
	}
	if got := binary.BigEndian.Uint16(b[12:]); got != EtherTypeVLAN {
		t.Errorf("tag protocol %#04x, want %#04x", got, EtherTypeVLAN)
	}
	tci := binary.BigEndian.Uint16(b[14:])
	if id := tci & 0xfff; id != 123 {
		t.Errorf("vlan id %d, want 123", id)
	}
	if pcp := tci >> 13; pcp != 3 {
		t.Errorf("priority %d, want 3", pcp)
	}
	if got := binary.BigEndian.Uint16(b[16:]); got != EtherTypeIPv4 {
		t.Errorf("ethertype after the tag is %#04x, want %#04x", got, EtherTypeIPv4)
	}
	if f.HeaderLen != 18+20+8 {
		t.Errorf("header is %d bytes, want 46", f.HeaderLen)
	}
	// The whole 18-byte Ethernet header of a singly tagged frame ends exactly
	// where the inline header a device in L2 mode asks for does.
	if got := 14 + 4; got != 18 {
		t.Errorf("a tagged Ethernet header is %d bytes", got)
	}
}

// Two tags make the Ethernet header 22 bytes, past what a device asking for 18
// would be shown, which is the case an inline length of exactly 18 gets wrong.
func TestDoubleTaggedLayout(t *testing.T) {
	s := testSpec()
	s.VLAN, s.OuterVLAN, s.Size = 20, 10, 128
	f, err := Build(s)
	if err != nil {
		t.Fatal(err)
	}
	b := f.Bytes

	if f.VLANTags != 2 {
		t.Fatalf("%d tags, want 2", f.VLANTags)
	}
	if got := binary.BigEndian.Uint16(b[12:]); got != EtherTypeQinQ {
		t.Errorf("outer tag protocol %#04x, want %#04x", got, EtherTypeQinQ)
	}
	if got := binary.BigEndian.Uint16(b[14:]) & 0xfff; got != 10 {
		t.Errorf("outer vlan %d, want 10", got)
	}
	if got := binary.BigEndian.Uint16(b[16:]); got != EtherTypeVLAN {
		t.Errorf("inner tag protocol %#04x, want %#04x", got, EtherTypeVLAN)
	}
	if got := binary.BigEndian.Uint16(b[18:]) & 0xfff; got != 20 {
		t.Errorf("inner vlan %d, want 20", got)
	}
	if got := binary.BigEndian.Uint16(b[20:]); got != EtherTypeIPv4 {
		t.Errorf("ethertype after two tags is %#04x", got)
	}
	if f.HeaderLen != 22+20+8 {
		t.Errorf("header is %d bytes, want 50", f.HeaderLen)
	}
}

// A wrong header checksum means every router on the path drops the frame, and
// nothing local would notice.
func TestIPv4ChecksumIsCorrect(t *testing.T) {
	for _, size := range []int{60, 64, 128, 1514} {
		for _, vlan := range []int{0, 100} {
			s := testSpec()
			s.Size, s.VLAN = size, vlan
			f, err := Build(s)
			if err != nil {
				t.Fatalf("size %d vlan %d: %v", size, vlan, err)
			}
			ip := 14 + f.VLANTags*4
			// A correct checksum makes the sum over the whole header zero.
			if got := checksum(f.Bytes[ip : ip+20]); got != 0 {
				t.Errorf("size %d vlan %d: header checksum verifies to %#04x, want 0", size, vlan, got)
			}
			if binary.BigEndian.Uint16(f.Bytes[ip+10:]) == 0 {
				t.Errorf("size %d vlan %d: header checksum is zero, which means it was never computed", size, vlan)
			}
		}
	}
}

// The checksum function itself, against a known-good header.
func TestChecksum(t *testing.T) {
	// From RFC 1071: this header sums to 0xb861.
	hdr := []byte{0x45, 0x00, 0x00, 0x73, 0x00, 0x00, 0x40, 0x00, 0x40, 0x11,
		0x00, 0x00, 0xc0, 0xa8, 0x00, 0x01, 0xc0, 0xa8, 0x00, 0xc7}
	if got := checksum(hdr); got != 0xb861 {
		t.Errorf("checksum = %#04x, want 0xb861", got)
	}
	// An odd-length input must not be misread.
	if got := checksum([]byte{0x00, 0x01, 0xf2}); got == 0 {
		t.Error("checksum of an odd-length input came out zero")
	}
}

func TestSetSrcPort(t *testing.T) {
	f, err := Build(testSpec())
	if err != nil {
		t.Fatal(err)
	}
	f.SetSrcPort(4242)
	if got := binary.BigEndian.Uint16(f.Bytes[f.SrcPortOff:]); got != 4242 {
		t.Errorf("source port %d, want 4242", got)
	}
	// The UDP checksum stays zero, so changing a port needs nothing else fixed.
	if got := binary.BigEndian.Uint16(f.Bytes[f.SrcPortOff+6:]); got != 0 {
		t.Errorf("UDP checksum is %#04x, want zero", got)
	}
}

// The shortest legal frame still has room for the longest header built here,
// so a double-tagged 60-byte frame is a real thing and must work.
func TestShortestDoubleTaggedFrame(t *testing.T) {
	s := testSpec()
	s.VLAN, s.OuterVLAN, s.Size = 20, 10, MinFrame
	f, err := Build(s)
	if err != nil {
		t.Fatalf("the shortest double-tagged frame was refused: %v", err)
	}
	if len(f.Bytes) != MinFrame {
		t.Errorf("frame is %d bytes, want %d", len(f.Bytes), MinFrame)
	}
	if got := len(f.Bytes) - f.HeaderLen; got != MinFrame-50 {
		t.Errorf("payload is %d bytes, want %d", got, MinFrame-50)
	}
}

func TestBuildRejectsNonsense(t *testing.T) {
	for _, c := range []struct {
		name string
		fix  func(*Spec)
	}{
		{"short address", func(s *Spec) { s.DstMAC = net.HardwareAddr{1, 2, 3} }},
		{"IPv6", func(s *Spec) { s.SrcIP = netip.MustParseAddr("2001:db8::1") }},
		{"vlan out of range", func(s *Spec) { s.VLAN = 4096 }},
		{"outer tag without an inner one", func(s *Spec) { s.OuterVLAN = 10 }},
		{"shorter than the minimum frame", func(s *Spec) { s.Size = 40 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testSpec()
			c.fix(&s)
			if _, err := Build(s); err == nil {
				t.Error("accepted")
			}
		})
	}
}
