//go:build linux

package netstack

import (
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/checksum"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/header"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/stack"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/stack/gro"
)

// fakeDispatcher records what reaches the stack.
type fakeDispatcher struct {
	pkts  int
	bytes int
}

func (f *fakeDispatcher) DeliverNetworkPacket(_ tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
	f.pkts++
	f.bytes += pkt.Data().Size()
}
func (f *fakeDispatcher) DeliverLinkPacket(tcpip.NetworkProtocolNumber, *stack.PacketBuffer) {}

// wireSegment builds a VLAN-tagged IPv4/TCP frame as a Linux peer would send
// it in a bulk stream: full checksums, a timestamp option, PSH only if asked.
func wireSegment(seq uint32, payload []byte, psh bool) []byte {
	return wireFrame(seq, payload, psh, true)
}

// wireFrame is wireSegment with the timestamp option optional: without it a
// segment can be short enough to need padding on the wire.
func wireFrame(seq uint32, payload []byte, psh, timestamps bool) []byte {
	tcpLen := header.TCPMinimumSize
	if timestamps {
		tcpLen += 12
	}
	f := make([]byte, vlanHdrLen+header.IPv4MinimumSize+tcpLen+len(payload))
	putHeader(f, srcMAC, dstMAC, testVLAN, etherTypeIPv4)
	ip := header.IPv4(f[vlanHdrLen:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(header.IPv4MinimumSize + tcpLen + len(payload)), TTL: 64,
		Protocol: uint8(header.TCPProtocolNumber), SrcAddr: testDst, DstAddr: testSrc,
		Flags: header.IPv4FlagDontFragment,
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	tcp := header.TCP(f[vlanHdrLen+header.IPv4MinimumSize:])
	flags := header.TCPFlagAck
	if psh {
		flags |= header.TCPFlagPsh
	}
	tcp.Encode(&header.TCPFields{SrcPort: 43210, DstPort: 8080, SeqNum: seq, AckNum: 77,
		DataOffset: uint8(tcpLen), Flags: flags, WindowSize: 500})
	if timestamps {
		copy(tcp[header.TCPMinimumSize:], []byte{1, 1, 8, 10, 0, 0, 0, 9, 0, 0, 0, 3})
	}
	copy(tcp[tcpLen:], payload)
	xsum := header.PseudoHeaderChecksum(header.TCPProtocolNumber, testDst, testSrc, uint16(tcpLen+len(payload)))
	xsum = checksum.Checksum(payload, xsum)
	tcp.SetChecksum(^tcp.CalculateChecksum(xsum))
	return f
}

// testEndpoint is an endpoint with no device under it, enough for deliver:
// the MAC is srcMAC and the address testSrc, so wireSegment's frames are
// addressed to it.
func testEndpoint() *endpoint {
	e := &endpoint{cfg: Config{VLAN: testVLAN}, linkLen: vlanHdrLen, neigh: newNeighbors(),
		addr: testSrc.As4(), prefix: netip.PrefixFrom(netip.AddrFrom4(testSrc.As4()), 24).Masked()}
	copy(e.mac[:], srcMAC[:])
	return e
}

func TestGROMergesABurst(t *testing.T) {
	e := testEndpoint()
	var delivered atomic.Uint64
	fd := &fakeDispatcher{}
	g := &gro.GRO{Dispatcher: countingDispatcher{d: fd, n: &delivered}}
	g.Init(true)

	payload := make([]byte, testMSS)
	const n = 10
	for i := 0; i < n; i++ {
		e.deliver(0, wireSegment(uint32(1+i*testMSS), payload, i == n-1), fd, g)
	}
	g.Flush()

	if fd.pkts != 1 {
		t.Fatalf("GRO delivered %d packets for %d contiguous segments, want 1 (merge ratio broken)", fd.pkts, n)
	}
	wantBytes := header.IPv4MinimumSize + header.TCPMinimumSize + 12 + n*testMSS
	if fd.bytes != wantBytes {
		t.Fatalf("merged packet is %d bytes, want %d", fd.bytes, wantBytes)
	}
}

// TestGRODropsPadding is a short segment padded to the Ethernet minimum on
// the wire, arriving in a burst after a full one: GRO must merge the
// packet, not the padding. A peer without TCP timestamps pads every ACK,
// which is how a wire-level detail would have become bytes in the stream.
func TestGRODropsPadding(t *testing.T) {
	e := testEndpoint()
	var delivered atomic.Uint64
	fd := &fakeDispatcher{}
	g := &gro.GRO{Dispatcher: countingDispatcher{d: fd, n: &delivered}}
	g.Init(true)

	// No timestamp option: the short segment is then under the minimum
	// frame and padded on the wire, as a peer without timestamps pads
	// every ACK.
	first := wireFrame(1, make([]byte, 1000), false, false)
	short := wireFrame(1001, []byte{7}, true, false)
	if len(short) >= minFrameLen {
		t.Fatalf("the short frame is %d bytes; the test needs one under %d", len(short), minFrameLen)
	}
	padded := make([]byte, minFrameLen)
	copy(padded, short)
	e.deliver(0, first, fd, g)
	e.deliver(0, padded, fd, g)
	g.Flush()

	if fd.pkts != 1 {
		t.Fatalf("GRO delivered %d packets, want the two merged", fd.pkts)
	}
	want := header.IPv4MinimumSize + header.TCPMinimumSize + 1000 + 1
	if fd.bytes != want {
		t.Fatalf("merged packet is %d bytes, want %d: padding was merged into the stream", fd.bytes, want)
	}
	if e.Stats().Rx != 2 {
		t.Fatalf("stats: %+v", e.Stats())
	}
}

// TestFragmentsDropped: a fragment's TCP checksum cannot be verified before
// reassembly, and the stack would take the reassembled packet as verified;
// the endpoint does not accept fragments.
func TestFragmentsDropped(t *testing.T) {
	for _, g := range []bool{true, false} {
		e := testEndpoint()
		e.cfg.NoGRO = !g
		fd := &fakeDispatcher{}
		var gr *gro.GRO
		if g {
			var n atomic.Uint64
			gr = &gro.GRO{Dispatcher: countingDispatcher{d: fd, n: &n}}
			gr.Init(true)
		}
		f := wireSegment(1, make([]byte, 100), true)
		ip := header.IPv4(f[vlanHdrLen:])
		ip.SetFlagsFragmentOffset(header.IPv4FlagMoreFragments, 0)
		ip.SetChecksum(0)
		ip.SetChecksum(^ip.CalculateChecksum())
		e.deliver(0, f, fd, gr)
		if gr != nil {
			gr.Flush()
		}
		if st := e.Stats(); fd.pkts != 0 || st.Fragments != 1 || st.Rx != 0 {
			t.Fatalf("gro %v: delivered %d, stats %+v", g, fd.pkts, st)
		}
	}
}

// TestLearnsOnlyFromFramesAddressedToUs: the neighbor table takes a
// (source IP, source MAC) pair only from a frame sent to this stack's MAC
// and address by a source on its prefix, and never over a pinned entry.
func TestLearnsOnlyFromFramesAddressedToUs(t *testing.T) {
	peer := testDst.As4()
	cases := []struct {
		name  string
		edit  func(f []byte)
		learn bool
	}{
		{"addressed to us", func([]byte) {}, true},
		{"another host's MAC", func(f []byte) { f[0] ^= 1 }, false},
		{"broadcast", func(f []byte) { copy(f[0:6], broadcastMAC[:]) }, false},
		{"another host's address", func(f []byte) { f[vlanHdrLen+19] ^= 1 }, false},
		{"source off the prefix", func(f []byte) { f[vlanHdrLen+13] ^= 1 }, false},
		{"bad TCP checksum", func(f []byte) { f[len(f)-1] ^= 1 }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := testEndpoint()
			e.cfg.NoGRO = true
			f := wireSegment(1, make([]byte, 100), true)
			c.edit(f)
			if c.name != "bad TCP checksum" {
				ip := header.IPv4(f[vlanHdrLen:])
				ip.SetChecksum(0)
				ip.SetChecksum(^ip.CalculateChecksum())
				tcp := header.TCP(f[vlanHdrLen+header.IPv4MinimumSize:])
				tcp.SetChecksum(0)
				xsum := header.PseudoHeaderChecksum(header.TCPProtocolNumber, ip.SourceAddress(), ip.DestinationAddress(), uint16(len(tcp)))
				tcp.SetChecksum(^tcp.CalculateChecksum(checksum.Checksum(tcp[header.TCPMinimumSize+12:], xsum)))
			}
			e.deliver(0, f, &fakeDispatcher{}, nil)
			src := [4]byte(f[vlanHdrLen+12 : vlanHdrLen+16])
			if _, ok := e.neigh.lookup(src); ok != c.learn {
				t.Fatalf("learned %v, want %v (stats %+v)", ok, c.learn, e.Stats())
			}
		})
	}

	e := testEndpoint()
	e.cfg.NoGRO = true
	pinned := [6]byte{9, 9, 9, 9, 9, 9}
	e.AddNeighbor(peer, pinned)
	e.deliver(0, wireSegment(1, make([]byte, 100), true), &fakeDispatcher{}, nil)
	if got, _ := e.neigh.lookup(peer); got != pinned {
		t.Fatalf("a frame from %v overwrote the pinned MAC: %v", peer, got)
	}
}
