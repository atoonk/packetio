//go:build linux

package netstack

import (
	"github.com/atoonk/packetio/netstack/gvisor/pkg/buffer"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/checksum"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/header"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/stack"
)

// Segmentation offload, done in software, the way the kernel does it for a
// NIC without TSO.
//
// With SupportedGSO reporting HostGSOSupported, netstack's TCP sender hands
// the endpoint one packet of up to GSOMaxSize bytes instead of one per MSS:
// one trip through the TCP and IP layers for what would otherwise be 46. The
// IPv4 header carries the total length of that big packet, and the TCP
// checksum field holds only the pseudo-header sum (Linux calls this
// CHECKSUM_PARTIAL). The endpoint owes the wire MSS-sized frames, so it
// splits here: copy the headers once per segment, fix the IPv4 length and
// header checksum, advance the sequence number, keep PSH and FIN for the last
// segment only, and finish the TCP checksum over each segment.
//
// The same partial-checksum contract applies to every packet a host-GSO
// endpoint sends for an established connection — pure ACKs included — so the
// single-frame path finishes checksums too. SYNs and resets from the protocol
// (not an endpoint) arrive with full checksums and GSOOptions unset;
// GSOOptions.NeedsCsum is the discriminator.
//
// Checksums are finished in software on every backend. Where a backend can
// have the NIC finish them (packetio.OffloadTransmitter) that is a later
// addition inside the builder; the segmenter's contract does not change.

// gsoMaxSize is what GSOMaxSize reports: the largest packet the stack may
// hand WritePackets. netstack caps the payload at this minus 161.
const gsoMaxSize = 1 << 16

// maxHeaderTemplate bounds the headers copied per segment: an 802.1Q Ethernet
// header, the largest IPv4 header, and the largest TCP header.
const maxHeaderTemplate = vlanHdrLen + header.IPv4MaximumHeaderSize + header.TCPHeaderMaximumSize

// segments returns how many wire frames pkt becomes: one, unless it is a GSO
// packet with more than one MSS of payload.
func segments(pkt *stack.PacketBuffer) int {
	g := pkt.GSOOptions
	if g.Type == stack.GSONone || g.MSS == 0 {
		return 1
	}
	n := pkt.Data().Size()
	if n <= int(g.MSS) {
		return 1
	}
	return (n + int(g.MSS) - 1) / int(g.MSS)
}

// needsChecksum reports whether the stack left this packet's TCP checksum
// unfinished for the endpoint to complete.
func needsChecksum(pkt *stack.PacketBuffer) bool {
	return pkt.GSOOptions.Type != stack.GSONone && pkt.GSOOptions.NeedsCsum &&
		pkt.NetworkProtocolNumber == header.IPv4ProtocolNumber &&
		pkt.TransportProtocolNumber == header.TCPProtocolNumber
}

// finishTCPChecksum computes the TCP checksum of the IPv4/TCP packet that
// starts at frame[linkLen:], over exactly the bytes the IPv4 total length
// covers (so Ethernet padding is not summed). It ignores whatever the
// checksum field held: a fresh pseudo-header sum is as cheap as adjusting the
// stack's partial one.
func finishTCPChecksum(frame []byte, linkLen int) {
	ip := header.IPv4(frame[linkLen:])
	ipLen := int(ip.HeaderLength())
	tcpLen := int(ip.TotalLength()) - ipLen
	start := linkLen + ipLen
	if tcpLen < header.TCPMinimumSize || start+tcpLen > len(frame) {
		return
	}
	seg := frame[start : start+tcpLen]
	tcp := header.TCP(seg)
	tcp.SetChecksum(0)
	xsum := header.PseudoHeaderChecksum(header.TCPProtocolNumber,
		ip.SourceAddress(), ip.DestinationAddress(), uint16(tcpLen))
	tcp.SetChecksum(^checksum.Checksum(seg, xsum))
}

// payloadCursor reads a packet's payload once, front to back, across however
// many views it spans, without re-walking the list for every segment.
type payloadCursor struct {
	v   *buffer.View
	off int
}

// newPayloadCursor positions a cursor at the first payload byte: past the
// unused reserved header space and past the headers themselves.
func newPayloadCursor(pkt *stack.PacketBuffer) payloadCursor {
	views, skip := pkt.AsViewList()
	c := payloadCursor{v: views.Front()}
	c.skip(skip + pkt.HeaderSize())
	return c
}

func (c *payloadCursor) skip(n int) {
	for c.v != nil && n > 0 {
		rem := c.v.Size() - c.off
		if n < rem {
			c.off += n
			return
		}
		n -= rem
		c.v = c.v.Next()
		c.off = 0
	}
}

// copyTo fills dst from the cursor and advances it, returning the bytes
// copied (fewer than len(dst) only at the end of the payload).
func (c *payloadCursor) copyTo(dst []byte) int {
	n := 0
	for c.v != nil && n < len(dst) {
		m := copy(dst[n:], c.v.AsSlice()[c.off:])
		n += m
		c.off += m
		if c.off >= c.v.Size() {
			c.v = c.v.Next()
			c.off = 0
		}
	}
	return n
}

// segmenter emits the frames of one GSO packet, in sequence order.
type segmenter struct {
	tmpl    [maxHeaderTemplate]byte
	hdrLen  int // link + IPv4 + TCP headers, the template's used length
	linkLen int
	ipLen   int
	tcpLen  int
	mss     int
	left    int // payload bytes not yet emitted
	seg     int // index of the next segment
	nseg    int
	seq0    uint32
	flags   header.TCPFlags
	src     tcpip.Address
	dst     tcpip.Address
	cur     payloadCursor
	// ipCsumBase is the ones-complement sum of the IPv4 header with its
	// total length and checksum fields zero: each frame's header checksum
	// is then one add, not a pass over twenty bytes (RFC 1624).
	ipCsumBase uint16
}

// init prepares s to split pkt, whose link header (linkLen bytes) AddHeader
// has already pushed.
func (s *segmenter) init(pkt *stack.PacketBuffer, linkLen int) {
	link := pkt.LinkHeader().Slice()
	ip := header.IPv4(pkt.NetworkHeader().Slice())
	tcp := header.TCP(pkt.TransportHeader().Slice())
	s.linkLen = linkLen
	s.ipLen = len(ip)
	s.tcpLen = len(tcp)
	s.hdrLen = s.linkLen + s.ipLen + s.tcpLen
	n := copy(s.tmpl[:], link[:linkLen])
	n += copy(s.tmpl[n:], ip)
	copy(s.tmpl[n:], tcp)
	s.mss = int(pkt.GSOOptions.MSS)
	s.left = pkt.Data().Size()
	s.seg = 0
	s.nseg = segments(pkt)
	s.seq0 = tcp.SequenceNumber()
	s.flags = tcp.Flags()
	s.src = ip.SourceAddress()
	s.dst = ip.DestinationAddress()
	s.cur = newPayloadCursor(pkt)
	// The header template's checksum, less its length: see ipCsumBase.
	tip := header.IPv4(s.tmpl[s.linkLen : s.linkLen+s.ipLen])
	tip.SetTotalLength(0)
	tip.SetChecksum(0)
	s.ipCsumBase = tip.CalculateChecksum()
}

// setIPLength writes the frame's IPv4 total length and the header checksum
// that goes with it.
func (s *segmenter) setIPLength(ip header.IPv4, total int) {
	ip.SetTotalLength(uint16(total))
	ip.SetChecksum(^checksum.Combine(s.ipCsumBase, uint16(total)))
}

// done reports whether every segment has been emitted.
func (s *segmenter) done() bool { return s.seg >= s.nseg }

// next writes the next segment into frame and returns its length.
func (s *segmenter) next(frame []byte) int {
	n := s.mss
	if n > s.left {
		n = s.left
	}
	copy(frame, s.tmpl[:s.hdrLen])

	ip := header.IPv4(frame[s.linkLen:])
	s.setIPLength(ip, s.ipLen+s.tcpLen+n)

	tcp := header.TCP(frame[s.linkLen+s.ipLen:])
	tcp.SetSequenceNumber(s.seq0 + uint32(s.seg*s.mss))
	last := s.seg == s.nseg-1
	if last {
		tcp.SetFlags(uint8(s.flags))
	} else {
		tcp.SetFlags(uint8(s.flags &^ (header.TCPFlagPsh | header.TCPFlagFin)))
	}

	got := s.cur.copyTo(frame[s.hdrLen : s.hdrLen+n])
	total := s.hdrLen + got
	if got != n {
		// The payload ran out early: the packet's views disagree with its
		// Data().Size(). Emit what there is with a consistent length rather
		// than a frame whose IP length overstates it.
		s.setIPLength(ip, s.ipLen+s.tcpLen+got)
		s.left = 0
	} else {
		s.left -= n
	}
	s.seg++

	tcp.SetChecksum(0)
	xsum := header.PseudoHeaderChecksum(header.TCPProtocolNumber, s.src, s.dst, uint16(s.tcpLen+got))
	tcp.SetChecksum(^checksum.Checksum(frame[s.linkLen+s.ipLen:total], xsum))

	if total < minFrameLen {
		clear(frame[total:minFrameLen])
		total = minFrameLen
	}
	return total
}

// admit decides how many leading packets of a batch fit a budget of frames
// when each packet must go whole or not at all. It returns the number of
// packets admitted and the frames they need. segs holds segments(pkt) for
// each packet.
func admit(segs []int, budget int) (packets, frames int) {
	for _, s := range segs {
		if frames+s > budget {
			break
		}
		frames += s
		packets++
	}
	return packets, frames
}

// checksumsValid verifies the IPv4 header checksum and, for TCP and UDP, the
// transport checksum of the packet in b (which starts at the IPv4 header and
// may carry trailing Ethernet padding). A fragment passes on the IPv4 check
// alone, and the receive path drops it. Other protocols pass on the IPv4
// check alone too, which is safe only while the stack verifies what it
// takes itself: with GRO on the endpoint claims RX checksum offload and the
// stack then believes every packet was checked here. ICMP is verified by the
// stack regardless; UDP is checked here for the day it is wired up.
func checksumsValid(b []byte) bool {
	if len(b) < header.IPv4MinimumSize {
		return false
	}
	ip := header.IPv4(b)
	if !ip.IsValid(len(b)) || !ip.IsChecksumValid() {
		return false
	}
	total := int(ip.TotalLength())
	if total > len(b) {
		return false
	}
	if ip.FragmentOffset() != 0 || ip.Flags()&header.IPv4FlagMoreFragments != 0 {
		return true
	}
	ipLen := int(ip.HeaderLength())
	seg := b[ipLen:total]
	switch ip.Protocol() {
	case uint8(header.UDPProtocolNumber):
		if len(seg) < header.UDPMinimumSize {
			return false
		}
		udp := header.UDP(seg)
		if udp.Checksum() == 0 {
			return true // no checksum, as IPv4 UDP may
		}
		payload := seg[header.UDPMinimumSize:]
		return udp.IsChecksumValid(ip.SourceAddress(), ip.DestinationAddress(), checksum.Checksum(payload, 0))
	case uint8(header.TCPProtocolNumber):
	default:
		return true
	}
	if len(seg) < header.TCPMinimumSize {
		return false
	}
	tcp := header.TCP(seg)
	off := int(tcp.DataOffset())
	if off < header.TCPMinimumSize || off > len(seg) {
		return false
	}
	payload := seg[off:]
	return tcp.IsChecksumValid(ip.SourceAddress(), ip.DestinationAddress(),
		checksum.Checksum(payload, 0), uint16(len(payload)))
}
