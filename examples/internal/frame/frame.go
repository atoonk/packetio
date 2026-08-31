// Package frame builds the Ethernet frames the examples transmit.
//
// It exists so the examples stay small: what is being measured is the packet
// path, not packet construction, and a generator that spends its time here is
// measuring the wrong thing. A frame is built once and copied.
package frame

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
)

// Ethernet and IP constants used here.
const (
	EtherTypeIPv4 = 0x0800
	EtherTypeVLAN = 0x8100
	EtherTypeQinQ = 0x88a8

	// MinFrame is the shortest Ethernet frame, not counting the four-byte
	// frame check sequence the NIC appends.
	MinFrame = 60

	// FCS is what the NIC adds after the bytes handed to it, so a "64-byte
	// frame" is 60 bytes of ours.
	FCS = 4

	ethHeader  = 14
	vlanTag    = 4
	ipv4Header = 20
	udpHeader  = 8

	protoUDP = 17
)

// Spec describes a frame to build.
type Spec struct {
	// SrcMAC and DstMAC are the Ethernet addresses. DstMAC must be the address
	// the far end will accept: with no switch learning involved, sending to a
	// made-up address usually means the frames are never seen again.
	SrcMAC, DstMAC net.HardwareAddr

	// VLAN is the tag to carry, or zero for untagged. Outer, if there are two.
	VLAN int

	// OuterVLAN, when set, makes the frame double tagged: OuterVLAN outside,
	// VLAN inside.
	OuterVLAN int

	// Priority is the priority code point in the tags.
	Priority int

	// SrcIP, DstIP, SrcPort and DstPort are the UDP flow.
	SrcIP, DstIP     netip.Addr
	SrcPort, DstPort uint16

	// Size is the whole frame length in bytes, not counting the frame check
	// sequence. 60 is the shortest legal frame and corresponds to what is
	// usually called a 64-byte packet.
	Size int
}

// Frame is a built frame and the offsets of the fields worth changing between
// packets, so a generator can vary a flow without rebuilding anything.
type Frame struct {
	// Bytes is the frame, ready to transmit.
	Bytes []byte

	// SrcPortOff and DstPortOff are where the UDP ports are, and SrcIPOff and
	// DstIPOff where the addresses are. Changing an address means fixing two
	// checksums; changing a port means fixing one, or leaving it zero.
	SrcIPOff, DstIPOff     int
	SrcPortOff, DstPortOff int

	// VLANTags is how many tags the frame carries, which is what decides how
	// much of it a device in L2 inline mode must be shown.
	VLANTags int

	// HeaderLen is everything before the UDP payload.
	HeaderLen int
}

// Build lays out a frame.
func Build(s Spec) (*Frame, error) {
	if len(s.SrcMAC) != 6 || len(s.DstMAC) != 6 {
		return nil, fmt.Errorf("frame: source and destination addresses must both be six bytes")
	}
	if !s.SrcIP.Is4() || !s.DstIP.Is4() {
		return nil, fmt.Errorf("frame: only IPv4 is built here, got %s to %s", s.SrcIP, s.DstIP)
	}
	if s.VLAN < 0 || s.VLAN > 4095 || s.OuterVLAN < 0 || s.OuterVLAN > 4095 {
		return nil, fmt.Errorf("frame: a VLAN id must be between 0 and 4095")
	}
	if s.OuterVLAN != 0 && s.VLAN == 0 {
		return nil, fmt.Errorf("frame: an outer VLAN needs an inner one")
	}

	tags := 0
	if s.VLAN != 0 {
		tags++
	}
	if s.OuterVLAN != 0 {
		tags++
	}

	hdr := ethHeader + tags*vlanTag + ipv4Header + udpHeader
	size := s.Size
	if size == 0 {
		size = MinFrame
	}
	if size < MinFrame {
		return nil, fmt.Errorf("frame: %d bytes is shorter than the %d-byte minimum", size, MinFrame)
	}
	// Unreachable while the longest header built here is the 50 bytes of a
	// double-tagged UDP packet, which fits the 60-byte minimum. It is here so
	// that adding IPv6 or a third tag fails loudly rather than silently
	// writing a negative-length payload.
	if size < hdr {
		return nil, fmt.Errorf("frame: %d bytes cannot hold a %d-byte header", size, hdr)
	}

	b := make([]byte, size)
	f := &Frame{Bytes: b, VLANTags: tags, HeaderLen: hdr}

	off := 0
	copy(b[off:], s.DstMAC)
	off += 6
	copy(b[off:], s.SrcMAC)
	off += 6

	// Tags are written outermost first, each one saying what follows it.
	if s.OuterVLAN != 0 {
		binary.BigEndian.PutUint16(b[off:], EtherTypeQinQ)
		binary.BigEndian.PutUint16(b[off+2:], uint16(s.Priority&7)<<13|uint16(s.OuterVLAN))
		off += 4
	}
	if s.VLAN != 0 {
		binary.BigEndian.PutUint16(b[off:], EtherTypeVLAN)
		binary.BigEndian.PutUint16(b[off+2:], uint16(s.Priority&7)<<13|uint16(s.VLAN))
		off += 4
	}
	binary.BigEndian.PutUint16(b[off:], EtherTypeIPv4)
	off += 2

	ip := off
	udpLen := size - hdr + udpHeader
	b[ip] = 0x45 // version 4, five 32-bit words of header
	b[ip+1] = 0  // no differentiated services
	binary.BigEndian.PutUint16(b[ip+2:], uint16(ipv4Header+udpLen))
	binary.BigEndian.PutUint16(b[ip+4:], 0)      // identification
	binary.BigEndian.PutUint16(b[ip+6:], 0x4000) // do not fragment
	b[ip+8] = 64                                 // time to live
	b[ip+9] = protoUDP
	src, dst := s.SrcIP.As4(), s.DstIP.As4()
	copy(b[ip+12:], src[:])
	copy(b[ip+16:], dst[:])
	binary.BigEndian.PutUint16(b[ip+10:], checksum(b[ip:ip+ipv4Header]))
	f.SrcIPOff, f.DstIPOff = ip+12, ip+16

	udp := ip + ipv4Header
	binary.BigEndian.PutUint16(b[udp:], s.SrcPort)
	binary.BigEndian.PutUint16(b[udp+2:], s.DstPort)
	binary.BigEndian.PutUint16(b[udp+4:], uint16(udpLen))
	f.SrcPortOff, f.DstPortOff = udp, udp+2

	// A recognisable payload, so a frame caught on the far end can be told
	// apart from anything else on the wire.
	payload := b[udp+udpHeader:]
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	copy(payload, "packetio")

	// A UDP checksum of zero means "not computed", which is legal for IPv4 and
	// is what a generator wants: no receiver checks it, and computing one per
	// packet would be work in the wrong place.
	binary.BigEndian.PutUint16(b[udp+6:], 0)
	return f, nil
}

// SetSrcPort changes the source port of a built frame in place. The UDP
// checksum stays zero, so nothing else has to be fixed. It is how a generator
// spreads traffic across a receiver's queues.
func (f *Frame) SetSrcPort(p uint16) {
	binary.BigEndian.PutUint16(f.Bytes[f.SrcPortOff:], p)
}

// checksum is the internet checksum of b: the one's complement of the one's
// complement sum of its 16-bit words.
func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}
