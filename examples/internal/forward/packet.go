package forward

import "encoding/binary"

// What a router does to one packet, as three plain functions: find the IPv4
// header and check it, look the destination up, and rewrite the packet for the
// next hop. They are an ordinary IPv4 input and rewrite path without the
// graph around them.

const (
	etherTypeIPv4 = 0x0800
	etherTypeVLAN = 0x8100
	etherTypeQinQ = 0x88a8

	ethHeaderLen  = 14
	vlanTagLen    = 4
	ipv4HeaderLen = 20

	// minFrame is the shortest Ethernet frame, without its check sequence.
	minFrame = 60
)

// ErrorCode is why a packet was not forwarded.
type ErrorCode uint8

const (
	ErrNone ErrorCode = iota
	ErrShort
	ErrNotIPv4
	ErrBadVersion
	ErrBadLength
	ErrTTL
	ErrBadChecksum
	ErrNoRoute
	ErrTxFull
	NumErrors
)

var errorNames = [NumErrors]string{
	ErrNone:        "forwarded",
	ErrShort:       "too short",
	ErrNotIPv4:     "not IPv4",
	ErrBadVersion:  "bad version",
	ErrBadLength:   "bad length",
	ErrTTL:         "ttl expired",
	ErrBadChecksum: "bad checksum",
	ErrNoRoute:     "no route",
	ErrTxFull:      "transmit ring full",
}

func (e ErrorCode) String() string { return errorNames[e] }

// Parse finds the IPv4 header in a frame, checks it the way ip4-input does,
// and returns its offset and the destination address. Untagged, tagged and
// double-tagged frames are all accepted; nothing here assumes how long the
// Ethernet header is.
func Parse(pkt []byte, l3OK bool) (l3 int, dst uint32, code ErrorCode) {
	if len(pkt) < ethHeaderLen+ipv4HeaderLen {
		return 0, 0, ErrShort
	}
	et := binary.BigEndian.Uint16(pkt[12:])
	l3 = ethHeaderLen
	for et == etherTypeVLAN || et == etherTypeQinQ {
		if len(pkt) < l3+vlanTagLen+ipv4HeaderLen {
			return 0, 0, ErrShort
		}
		et = binary.BigEndian.Uint16(pkt[l3+2:])
		l3 += vlanTagLen
	}
	if et != etherTypeIPv4 {
		return 0, 0, ErrNotIPv4
	}

	h := pkt[l3:]
	if h[0]>>4 != 4 {
		return 0, 0, ErrBadVersion
	}
	ihl := int(h[0]&0xf) * 4
	if ihl < ipv4HeaderLen || ihl > len(h) {
		return 0, 0, ErrBadLength
	}
	total := int(binary.BigEndian.Uint16(h[2:]))
	if total < ihl || total > len(h) {
		return 0, 0, ErrBadLength
	}
	// A packet arriving with a TTL of 1 has nowhere further to go: after the
	// decrement it would be 0. A router answers with an ICMP time exceeded;
	// this one counts it.
	if h[8] <= 1 {
		return 0, 0, ErrTTL
	}
	// The NIC checked the header on the way in on every packet it delivered;
	// when it says the checksum is right there is nothing to gain by adding it
	// up again.
	if !l3OK && onesComplementSum(h[:ihl]) != 0xffff {
		return 0, 0, ErrBadChecksum
	}
	return l3, binary.BigEndian.Uint32(h[16:]), ErrNone
}

// Rewrite turns the packet in buf into the one to send: the TTL goes down by
// one with the checksum adjusted to match, and the Ethernet header is replaced
// by the adjacency's. The packet's IPv4 header starts at l3, and buf is the
// whole frame, so there is room for the header to grow. It returns the length
// to send, which is the Ethernet header plus the IPv4 total length: what was
// padding on the way in is not forwarded, and what is too short on the way out
// is padded again.
func Rewrite(buf []byte, l3 int, adj *Adjacency) int {
	h := buf[l3:]
	// RFC 1624: the TTL word went down by 0x0100, so the one's-complement
	// checksum goes up by 0x0100, with the carry wrapped around.
	h[8]--
	c := uint32(binary.BigEndian.Uint16(h[10:])) + 0x0100
	if c >= 0xffff {
		c++
	}
	binary.BigEndian.PutUint16(h[10:], uint16(c))
	total := int(binary.BigEndian.Uint16(h[2:]))

	l2 := int(adj.L2Len)
	if l2 != l3 {
		// The new header is not the length of the old one: the IPv4 packet
		// moves to make room. This is the untagged-to-tagged case and its
		// reverse; the measured case writes in place. Growing the header can
		// push a frame-filling packet past the frame, and the NIC wrote the
		// length being trusted here -- so it is checked, and a packet that no
		// longer fits is the caller's to drop (return 0), not an overrun.
		if l2+total > len(buf) {
			return 0
		}
		copy(buf[l2:l2+total], buf[l3:l3+total])
	}
	copy(buf[:l2], adj.L2[:l2])

	n := l2 + total
	if n < minFrame {
		clear(buf[n:minFrame])
		n = minFrame
	}
	return n
}

// onesComplementSum is the Internet checksum's sum, folded to 16 bits and not
// yet complemented: a valid header sums to 0xffff. The 20-byte header is the
// case that matters and gets a path of its own.
func onesComplementSum(b []byte) uint16 {
	if len(b) == ipv4HeaderLen {
		w0 := binary.BigEndian.Uint64(b[0:8])
		w1 := binary.BigEndian.Uint64(b[8:16])
		s := (w0 >> 32) + (w0 & 0xffffffff) + (w1 >> 32) + (w1 & 0xffffffff) + uint64(binary.BigEndian.Uint32(b[16:20]))
		s = (s & 0xffff) + (s >> 16)
		s = (s & 0xffff) + (s >> 16)
		s = (s & 0xffff) + (s >> 16)
		return uint16(s)
	}
	var s uint64
	i := 0
	for ; i+8 <= len(b); i += 8 {
		w := binary.BigEndian.Uint64(b[i : i+8])
		s += (w >> 32) + (w & 0xffffffff)
	}
	if i+4 <= len(b) {
		s += uint64(binary.BigEndian.Uint32(b[i : i+4]))
		i += 4
	}
	if i+2 <= len(b) {
		s += uint64(binary.BigEndian.Uint16(b[i : i+2]))
		i += 2
	}
	if i < len(b) {
		s += uint64(b[i]) << 8
	}
	s = (s & 0xffffffff) + (s >> 32)
	s = (s & 0xffff) + (s >> 16)
	s = (s & 0xffff) + (s >> 16)
	return uint16(s)
}

// ipv4Checksum computes the header checksum from scratch, for building test
// packets and for checking the incremental update against.
func ipv4Checksum(h []byte) uint16 {
	tmp := make([]byte, len(h))
	copy(tmp, h)
	tmp[10], tmp[11] = 0, 0
	return ^onesComplementSum(tmp)
}
