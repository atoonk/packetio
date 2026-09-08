//go:build linux

package netstack

import (
	"bytes"
	"testing"

	"github.com/atoonk/packetio/netstack/gvisor/pkg/buffer"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/checksum"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/header"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/stack"
)

const (
	testVLAN = 2131
	testMSS  = 1448 // 1500 MTU, timestamps on
)

var (
	testSrc = tcpip.AddrFrom4([4]byte{192, 168, 0, 1})
	testDst = tcpip.AddrFrom4([4]byte{192, 168, 0, 3})
	srcMAC  = [6]byte{0x3c, 0xec, 0xef, 0xb4, 0xc4, 0x3e}
	dstMAC  = [6]byte{0x3c, 0xec, 0xef, 0xb4, 0xc5, 0x5e}
)

// gsoPacket builds a PacketBuffer the way netstack hands one to a host-GSO
// endpoint: headers pushed into reserved space, the IPv4 total length and
// checksum covering the whole packet, the TCP checksum holding only the
// pseudo-header sum, and GSOOptions set. flags is what the sender put on it.
func gsoPacket(t *testing.T, payload []byte, flags header.TCPFlags, gso bool) *stack.PacketBuffer {
	t.Helper()
	const seq, ack = 1000, 5000
	const tcpLen = header.TCPMinimumSize + 12 // with a timestamp option
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		ReserveHeaderBytes: vlanHdrLen + header.IPv4MinimumSize + tcpLen,
		Payload:            buffer.MakeWithData(payload),
	})
	pkt.NetworkProtocolNumber = header.IPv4ProtocolNumber
	pkt.TransportProtocolNumber = header.TCPProtocolNumber

	tcp := header.TCP(pkt.TransportHeader().Push(tcpLen))
	tcp.Encode(&header.TCPFields{
		SrcPort: 8080, DstPort: 43210, SeqNum: seq, AckNum: ack,
		DataOffset: tcpLen, Flags: flags, WindowSize: 1024,
	})
	// A timestamp option: kind 8, length 10, padded with two NOPs in front.
	copy(tcp[header.TCPMinimumSize:], []byte{1, 1, 8, 10, 0, 0, 0, 1, 0, 0, 0, 2})

	total := header.IPv4MinimumSize + tcpLen + len(payload)
	ip := header.IPv4(pkt.NetworkHeader().Push(header.IPv4MinimumSize))
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(total), TTL: 64, Protocol: uint8(header.TCPProtocolNumber),
		SrcAddr: testSrc, DstAddr: testDst, Flags: header.IPv4FlagDontFragment,
	})
	ip.SetChecksum(^ip.CalculateChecksum())

	if gso {
		pkt.GSOOptions = stack.GSO{Type: stack.GSOTCPv4, NeedsCsum: true, CsumOffset: header.TCPChecksumOffset,
			MSS: testMSS, L3HdrLen: header.IPv4MinimumSize, MaxSize: gsoMaxSize}
		// CHECKSUM_PARTIAL: the pseudo-header sum for the big length, unfolded.
		tcp.SetChecksum(header.PseudoHeaderChecksum(header.TCPProtocolNumber, testSrc, testDst, uint16(tcpLen+len(payload))))
	} else {
		xsum := header.PseudoHeaderChecksum(header.TCPProtocolNumber, testSrc, testDst, uint16(tcpLen+len(payload)))
		xsum = checksum.Checksum(payload, xsum)
		tcp.SetChecksum(^tcp.CalculateChecksum(xsum))
	}
	putHeader(pkt.LinkHeader().Push(vlanHdrLen), dstMAC, srcMAC, testVLAN, etherTypeIPv4)
	return pkt
}

// checkFrame parses one wire frame and returns its TCP payload, failing the
// test on any malformed header or bad checksum.
func checkFrame(t *testing.T, frame []byte, wantSeq uint32, wantFlags header.TCPFlags) []byte {
	t.Helper()
	hdr, et, ok := parseFrame(frame, testVLAN)
	if !ok || et != etherTypeIPv4 {
		t.Fatalf("frame not a VLAN %d IPv4 frame: hdr %d et %#x ok %v", testVLAN, hdr, et, ok)
	}
	ip := header.IPv4(frame[hdr:])
	if !ip.IsValid(len(frame) - hdr) {
		t.Fatalf("invalid IPv4 header")
	}
	if !ip.IsChecksumValid() {
		t.Fatalf("bad IPv4 checksum")
	}
	if int(ip.TotalLength()) > len(frame)-hdr {
		t.Fatalf("IPv4 total length %d exceeds frame %d", ip.TotalLength(), len(frame)-hdr)
	}
	seg := frame[hdr+int(ip.HeaderLength()) : hdr+int(ip.TotalLength())]
	tcp := header.TCP(seg)
	payload := seg[tcp.DataOffset():]
	if !tcp.IsChecksumValid(ip.SourceAddress(), ip.DestinationAddress(), checksum.Checksum(payload, 0), uint16(len(payload))) {
		t.Fatalf("bad TCP checksum on seq %d", tcp.SequenceNumber())
	}
	if tcp.SequenceNumber() != wantSeq {
		t.Fatalf("seq %d, want %d", tcp.SequenceNumber(), wantSeq)
	}
	if tcp.Flags() != wantFlags {
		t.Fatalf("seq %d: flags %v, want %v", wantSeq, tcp.Flags(), wantFlags)
	}
	if tcp.AckNumber() != 5000 || tcp.WindowSize() != 1024 {
		t.Fatalf("ack/window not carried over")
	}
	return payload
}

func TestSegmenterSplits(t *testing.T) {
	for _, size := range []int{1, testMSS, testMSS + 1, 2 * testMSS, 46 * testMSS, 65375} {
		payload := make([]byte, size)
		for i := range payload {
			payload[i] = byte(i*7 + i>>8)
		}
		flags := header.TCPFlagAck | header.TCPFlagPsh | header.TCPFlagFin
		pkt := gsoPacket(t, payload, flags, true)
		want := (size + testMSS - 1) / testMSS
		if got := segments(pkt); got != want {
			t.Fatalf("size %d: segments = %d, want %d", size, got, want)
		}

		var s segmenter
		var frame [2048]byte
		var got []byte
		if want == 1 {
			// Single-frame path: plain copy plus a finished checksum.
			n := copyPacket(frame[:], pkt)
			if !needsChecksum(pkt) {
				t.Fatalf("size %d: GSO packet should need its checksum finished", size)
			}
			finishTCPChecksum(frame[:n], vlanHdrLen)
			got = append(got, checkFrame(t, frame[:n], 1000, flags)...)
		} else {
			s.init(pkt, vlanHdrLen)
			for k := 0; !s.done(); k++ {
				n := s.next(frame[:])
				if n > vlanHdrLen+1500 {
					t.Fatalf("size %d seg %d: frame %d bytes exceeds MTU", size, k, n)
				}
				wantFlags := flags
				if k != want-1 {
					wantFlags &^= header.TCPFlagPsh | header.TCPFlagFin
				}
				got = append(got, checkFrame(t, frame[:n], uint32(1000+k*testMSS), wantFlags)...)
			}
			if s.seg != want {
				t.Fatalf("size %d: emitted %d segments, want %d", size, s.seg, want)
			}
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("size %d: reassembled payload differs (got %d bytes, want %d)", size, len(got), len(payload))
		}
		pkt.DecRef()
	}
}

func TestNonGSOPacketKeepsChecksum(t *testing.T) {
	// A packet from a path that does not use GSO (SYN, protocol reset)
	// arrives with a full checksum and no GSOOptions: it must be sent as is.
	pkt := gsoPacket(t, []byte("hello"), header.TCPFlagAck|header.TCPFlagPsh, false)
	if needsChecksum(pkt) {
		t.Fatal("non-GSO packet reported as needing a checksum")
	}
	var frame [2048]byte
	n := copyPacket(frame[:], pkt)
	checkFrame(t, frame[:n], 1000, header.TCPFlagAck|header.TCPFlagPsh)
	pkt.DecRef()
}

func TestAdmitWholePacketsOnly(t *testing.T) {
	segs := []int{1, 46, 1, 3}
	cases := []struct{ budget, pkts, frames int }{
		{0, 0, 0}, {1, 1, 1}, {46, 1, 1}, {47, 2, 47}, {48, 3, 48}, {50, 3, 48}, {51, 4, 51}, {1000, 4, 51},
	}
	for _, c := range cases {
		p, f := admit(segs, c.budget)
		if p != c.pkts || f != c.frames {
			t.Errorf("budget %d: admit = (%d, %d), want (%d, %d)", c.budget, p, f, c.pkts, c.frames)
		}
	}
}

func TestPayloadCursorSpansViews(t *testing.T) {
	// Payload spread over several views must read back contiguously.
	var b buffer.Buffer
	var want []byte
	for i := 0; i < 5; i++ {
		chunk := bytes.Repeat([]byte{byte('a' + i)}, 700+i)
		b.Append(buffer.NewViewWithData(chunk))
		want = append(want, chunk...)
	}
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{ReserveHeaderBytes: 64, Payload: b})
	pkt.TransportHeader().Push(20)
	pkt.NetworkHeader().Push(20)
	pkt.LinkHeader().Push(18)
	c := newPayloadCursor(pkt)
	var got []byte
	buf := make([]byte, 1000)
	for {
		n := c.copyTo(buf)
		if n == 0 {
			break
		}
		got = append(got, buf[:n]...)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("cursor read %d bytes, want %d; equal=%v", len(got), len(want), bytes.Equal(got, want))
	}
	pkt.DecRef()
}
