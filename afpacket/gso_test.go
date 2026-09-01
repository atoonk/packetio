//go:build linux

package afpacket

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/atoonk/packetio"
)

// vnetRing returns a synthetic ring in PACKET_VNET_HDR mode.
func vnetRing(blocks uint32) *ring {
	r := newTestRing(blocks)
	r.vnet = true
	return r
}

// armVnet lays one frame with a virtio header in front of it, exactly where the
// kernel puts it: immediately before the frame data.
func armVnet(t *testing.T, r *ring, blk uint32, o packetio.Offload, fr ringFrame) {
	t.Helper()
	armBlock(t, r, blk, 0, fr)
	mac := fr.macOff
	if mac == 0 {
		mac = testMacOff
	}
	d := blk*r.blockSize + testFirstOff + uint32(mac)
	o.Marshal(r.mem[d-packetio.OffloadHdrLen : d])
}

func drainOffload(r *ring, max, bufLen int) ([][]byte, []packetio.Offload, int) {
	bufs := make([][]byte, 0, max)
	lens := make([]int, max)
	offs := make([]packetio.Offload, max)
	n := r.read(max, func(i int) []byte {
		for len(bufs) <= i {
			bufs = append(bufs, make([]byte, bufLen))
		}
		return bufs[i]
	}, lens, offs, nil, nil)
	out := make([][]byte, n)
	for i := 0; i < n; i++ {
		out[i] = bufs[i][:lens[i]]
	}
	return out, offs[:n], n
}

func TestOffloadRoundTrip(t *testing.T) {
	want := packetio.Offload{
		Flags:     packetio.OffloadNeedsCsum,
		GSOType:   packetio.OffloadGSOTCPv4,
		HdrLen:    54,
		GSOSize:   1448,
		CsumStart: 34,
		CsumOff:   16,
	}
	var b [packetio.OffloadHdrLen]byte
	want.Marshal(b[:])
	if got := packetio.UnmarshalOffload(b[:]); got != want {
		t.Errorf("round trip: got %+v, want %+v", got, want)
	}
	// The wire layout is little-endian and fixed; a byte order slip here would
	// make every MSS wrong by a factor of 256.
	if b[4] != 1448&0xff || b[5] != 1448>>8 {
		t.Errorf("gso_size bytes are %02x %02x, want little-endian 1448", b[4], b[5])
	}
	if !want.Segmented() {
		t.Error("a TCPv4 super-frame should report as segmented")
	}
	if (packetio.Offload{}).Segmented() {
		t.Error("the zero Offload is an ordinary frame")
	}
	// ECN is or'd into the type and must not hide the type.
	ecn := packetio.Offload{GSOType: packetio.OffloadGSOTCPv4 | packetio.OffloadGSOECN}
	if !ecn.Segmented() {
		t.Error("ECN must not stop a super-frame reading as segmented")
	}
}

func TestRingReportsOffload(t *testing.T) {
	r := vnetRing(4)
	want := packetio.Offload{GSOType: packetio.OffloadGSOTCPv4, HdrLen: 54, GSOSize: 1448}
	armVnet(t, r, 0, want, ringFrame{data: eth(1, 200)})

	_, offs, n := drainOffload(r, 4, 4096)
	if n != 1 {
		t.Fatalf("read %d, want 1", n)
	}
	if offs[0] != want {
		t.Errorf("got %+v, want %+v", offs[0], want)
	}
}

func TestRingShiftsCsumStartForReinsertedVLAN(t *testing.T) {
	r := vnetRing(4)
	// The kernel stripped the tag, so its csum_start counts from a frame with
	// no tag in it. Putting the tag back moves the L4 header four bytes along,
	// and the offsets have to move with it or the checksum lands in the wrong
	// place.
	in := packetio.Offload{Flags: packetio.OffloadNeedsCsum, CsumStart: 34, CsumOff: 16, HdrLen: 54}
	armVnet(t, r, 0, in, ringFrame{data: eth(1, 200), tpid: 0x8100, tci: 2053})

	_, offs, n := drainOffload(r, 4, 4096)
	if n != 1 {
		t.Fatalf("read %d, want 1", n)
	}
	if offs[0].CsumStart != 38 {
		t.Errorf("csum_start = %d, want 38 after a 4-byte tag went back in", offs[0].CsumStart)
	}
	if offs[0].HdrLen != 58 {
		t.Errorf("hdr_len = %d, want 58", offs[0].HdrLen)
	}
	if offs[0].CsumOff != 16 {
		t.Errorf("csum_off = %d, want it unchanged: it is relative to csum_start", offs[0].CsumOff)
	}
}

func TestRingDoesNotShiftOffsetsWithoutPartialChecksum(t *testing.T) {
	r := vnetRing(4)
	// No NEEDS_CSUM: the offsets mean nothing, so moving them would invent
	// information rather than preserve it.
	in := packetio.Offload{GSOType: packetio.OffloadGSOTCPv4, CsumStart: 34}
	armVnet(t, r, 0, in, ringFrame{data: eth(1, 100), tpid: 0x8100, tci: 7})
	_, offs, n := drainOffload(r, 4, 4096)
	if n != 1 {
		t.Fatalf("read %d, want 1", n)
	}
	if offs[0].CsumStart != 34 {
		t.Errorf("csum_start = %d, want it left alone at 34", offs[0].CsumStart)
	}
}

func TestRingRejectsMacOffsetWithNoRoomForVirtioHeader(t *testing.T) {
	r := vnetRing(4)
	// tp_mac smaller than the virtio header would make reading that header
	// underflow out of the block.
	armVnet(t, r, 0, packetio.Offload{}, ringFrame{data: eth(1, 60), macOff: 4})
	if _, _, n := drainOffload(r, 4, 4096); n != 0 {
		t.Errorf("read %d with no room for the virtio header, want 0", n)
	}
	if r.oversize.Load() == 0 {
		t.Error("it should have been counted")
	}
}

func TestPlainRingReportsNoOffload(t *testing.T) {
	r := newTestRing(4) // vnet false
	armBlock(t, r, 0, 0, ringFrame{data: eth(1, 60)})
	_, offs, n := drainOffload(r, 4, 2048)
	if n != 1 {
		t.Fatalf("read %d, want 1", n)
	}
	if (offs[0] != packetio.Offload{}) {
		t.Errorf("a plain ring reported %+v, want the zero Offload", offs[0])
	}
}

// udpFrame builds Ethernet/IPv4/UDP with the checksum field holding the real
// pseudo-header partial, which is what NEEDS_CSUM means. pad grows the frame
// past the IP payload, as the sender does for anything under 60 bytes.
//
// Writing a real partial is the whole point: an earlier version of this helper
// left the field zero, and a completion that counted the partial twice passed
// the test anyway.
func udpFrame(payload, pad int) (frame []byte, csumStart, csumOff int) {
	f := make([]byte, 14+20+8+payload+pad)
	f[12], f[13] = 0x08, 0x00
	f[14] = 0x45
	binary.BigEndian.PutUint16(f[16:], uint16(20+8+payload)) // IP total_length
	f[22] = 64
	f[23] = 17 // UDP
	copy(f[26:30], []byte{192, 168, 0, 1})
	copy(f[30:34], []byte{192, 168, 0, 2})
	binary.BigEndian.PutUint16(f[34:], 1234)
	binary.BigEndian.PutUint16(f[36:], 5678)
	binary.BigEndian.PutUint16(f[38:], uint16(8+payload)) // UDP length
	for i := 42; i < 42+payload; i++ {
		f[i] = byte(i)
	}
	// The pseudo-header partial the kernel leaves in the field: source and
	// destination address, protocol, and the L4 length.
	var ps uint32
	for i := 26; i < 34; i += 2 {
		ps += uint32(binary.BigEndian.Uint16(f[i:]))
	}
	ps += uint32(17) + uint32(8+payload)
	for ps>>16 != 0 {
		ps = ps&0xffff + ps>>16
	}
	binary.BigEndian.PutUint16(f[40:], uint16(ps))
	return f, 34, 6
}

// udpChecksum computes the UDP checksum from scratch, independently of
// completeL4, so the two can be compared.
func udpChecksum(f []byte) uint16 {
	l4 := f[34 : 14+int(binary.BigEndian.Uint16(f[16:]))]
	var sum uint32
	for i := 26; i < 34; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(f[i:]))
	}
	sum += uint32(17) + uint32(len(l4))
	for i := 0; i+1 < len(l4); i += 2 {
		if i == 6 {
			continue // the checksum field itself
		}
		sum += uint32(binary.BigEndian.Uint16(l4[i:]))
	}
	if len(l4)%2 == 1 {
		sum += uint32(l4[len(l4)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	if c := ^uint16(sum); c != 0 {
		return c
	}
	return 0xffff
}

func TestCompleteL4MatchesAnIndependentChecksum(t *testing.T) {
	// The bug this exists for: seeding the sum with the checksum field and
	// then summing the field again counts the pseudo-header partial twice.
	// With a zero partial that is invisible, so every case here has a real one.
	for _, tc := range []struct{ payload, pad int }{
		{100, 0}, {1, 0}, {2, 0}, {17, 0}, {0, 0},
		{4, 14}, // a runt padded to the 60-byte Ethernet minimum
		{6, 20}, // padding must not be summed
	} {
		t.Run(fmt.Sprintf("payload=%d/pad=%d", tc.payload, tc.pad), func(t *testing.T) {
			f, cs, co := udpFrame(tc.payload, tc.pad)
			want := udpChecksum(f)
			if !completeL4(f, cs, co) {
				t.Fatal("refused a well-formed frame")
			}
			if got := binary.BigEndian.Uint16(f[cs+co:]); got != want {
				t.Errorf("completeL4 wrote %#04x, an independent sum says %#04x", got, want)
			}
		})
	}
}

func TestCompleteL4ProducesAValidChecksum(t *testing.T) {
	f, cs, co := udpFrame(100, 0)
	if !completeL4(f, cs, co) {
		t.Fatal("completeL4 refused a well-formed frame")
	}
	// A correct checksum makes the ones-complement sum over the L4 region come
	// out to all ones.
	end := 14 + int(binary.BigEndian.Uint16(f[16:]))
	var sum uint32
	for i := cs; i+1 < end; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(f[i:]))
	}
	// Plus the pseudo-header, which the partial in the field already carried.
	for i := 26; i < 34; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(f[i:]))
	}
	sum += uint32(17) + uint32(end-cs)
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	if uint16(sum) != 0xffff {
		t.Errorf("checked sum is %04x, want ffff", uint16(sum))
	}
}

func TestCompleteL4RejectsOutOfRangeOffsets(t *testing.T) {
	f, _, _ := udpFrame(20, 0)
	for _, tc := range []struct {
		name               string
		csumStart, csumOff int
	}{
		{"start past the frame", len(f) + 8, 6},
		{"offset past the frame", 34, len(f)},
		{"negative start", -1, 6},
		{"negative offset", 34, -1},
		{"field straddles the end", len(f) - 1, 0},
		{"huge offset that could wrap", 34, 1 << 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := append([]byte(nil), f...)
			if completeL4(cp, tc.csumStart, tc.csumOff) {
				t.Error("accepted an out-of-range pair")
			}
			// And it must not have written anything.
			for i := range cp {
				if cp[i] != f[i] {
					t.Fatalf("byte %d changed despite being refused", i)
				}
			}
		})
	}
}

func TestCompleteL4NeverWritesAZeroChecksum(t *testing.T) {
	// A zero UDP checksum means "not computed", so the convention is to send
	// all ones instead. Search for a payload that sums to zero.
	for n := 0; n < 300; n++ {
		f, cs, co := udpFrame(n, 0)
		cp := append([]byte(nil), f...)
		if !completeL4(cp, cs, co) {
			t.Fatalf("payload %d: refused", n)
		}
		if binary.BigEndian.Uint16(cp[cs+co:]) == 0 {
			t.Errorf("payload %d: wrote a zero checksum", n)
		}
	}
}

func TestGSOConfiguration(t *testing.T) {
	// Sizing is settled once all the options have run, exactly as Open does
	// it, so that the result cannot depend on the order they were listed in.
	build := func(opts ...Option) config {
		c := defaults()
		for _, o := range opts {
			o(&c)
		}
		c.resolve()
		return c
	}

	// WithGSO on its own raises the frame size and lowers the frame count, so
	// the default does not quietly reserve a quarter of a gigabyte.
	c := build(WithGSO())
	if c.frameSize < 1<<16 {
		t.Errorf("WithGSO left the frame size at %d", c.frameSize)
	}
	if got := int64(c.frames) * int64(c.frameSize); got > 64<<20 {
		t.Errorf("WithGSO alone reserves %d MiB; that is too much for a default", got>>20)
	}
	if err := c.validate(); err != nil {
		t.Errorf("WithGSO alone should validate: %v", err)
	}

	// An explicit frame count still wins.
	c = build(WithFrames(1024), WithGSO())
	if c.frames != 1024 {
		t.Errorf("WithGSO overrode an explicit WithFrames: got %d", c.frames)
	}

	// GSO must not be a hole through the rest of validation: the checks after
	// it used to be skipped entirely.
	c = build(WithGSO(), WithFrames(8))
	if err := c.validate(); err == nil {
		t.Error("WithGSO let a too-small frame count through validation")
	}
}

// The optional interfaces must actually be satisfied, or a caller's type
// assertion silently takes the slow path.
var (
	_ packetio.OffloadReceiver    = (*RxQueue)(nil)
	_ packetio.OffloadTransmitter = (*TxQueue)(nil)
)

// A super-frame's checksum field holds the pseudo-header partial, and the
// segmenter needs it: every segment's checksum is computed from it once the
// frame is cut up. Completing it here writes a sum over the whole super-frame
// into that field, so each segment goes out with a checksum for bytes it does
// not contain and the far end drops all of them, with nothing counting the
// loss. Reported from a forwarder integrating this backend.
func TestSegmentedFramesKeepTheirPartial(t *testing.T) {
	const csumStart, csumOff = 34, 16
	const partial = 0xb1ce

	build := func() []byte {
		f := make([]byte, 14+20+20+60000)
		binary.BigEndian.PutUint16(f[12:], 0x0800)
		f[14] = 0x45
		binary.BigEndian.PutUint16(f[16:], uint16(len(f)-14))
		f[23] = 6 // TCP
		binary.BigEndian.PutUint16(f[csumStart+csumOff:], partial)
		return f
	}

	// A GRO'd super-frame: both flags set, exactly as the kernel delivers it.
	seg := build()
	offs := []packetio.Offload{{
		Flags: packetio.OffloadNeedsCsum, GSOType: packetio.OffloadGSOTCPv4,
		HdrLen: 54, GSOSize: 1448, CsumStart: csumStart, CsumOff: csumOff,
	}}
	if bad := completeOffloads(offs, nil, func(int) []byte { return seg }); bad != 0 {
		t.Errorf("a segmented frame was counted as a failure %d times", bad)
	}
	if got := binary.BigEndian.Uint16(seg[csumStart+csumOff:]); got != partial {
		t.Errorf("the partial was overwritten: %#04x, want %#04x left alone", got, partial)
	}
	if offs[0].Flags&packetio.OffloadNeedsCsum == 0 {
		t.Error("a segmented frame still needs its checksum finished, so the flag must stay")
	}

	// The same frame without a GSO type is an ordinary packet and must be
	// completed, and then must stop asking to be.
	plain := build()
	offs = []packetio.Offload{{
		Flags: packetio.OffloadNeedsCsum, CsumStart: csumStart, CsumOff: csumOff,
	}}
	if bad := completeOffloads(offs, nil, func(int) []byte { return plain }); bad != 0 {
		t.Fatalf("an ordinary frame was refused %d times", bad)
	}
	if got := binary.BigEndian.Uint16(plain[csumStart+csumOff:]); got == partial {
		t.Error("an ordinary frame's checksum was left partial")
	}
	if offs[0].Flags&packetio.OffloadNeedsCsum != 0 {
		t.Error("a completed frame still asks to be completed: a forwarder passing " +
			"this back to the kernel would have it done twice")
	}
}

// A length field that cannot express the frame it arrived in is not a bound.
// Above 65535 the kernel writes the low bits, which name a range unrelated to
// the packet; summing to it produces a confident wrong answer over an
// arbitrary slice.
func TestOversizeLengthFieldIsNotTrusted(t *testing.T) {
	// 65620 bytes: tot_len wraps to 70, which would end the sum at byte 84.
	f := make([]byte, 65620)
	binary.BigEndian.PutUint16(f[12:], 0x0800)
	f[14] = 0x45
	binary.BigEndian.PutUint16(f[16:], uint16(len(f)-14))
	f[23] = 6
	for i := 34; i < len(f); i++ {
		f[i] = byte(i)
	}
	binary.BigEndian.PutUint16(f[50:], 0) // the checksum field, cleared

	if !completeL4(f, 34, 16) {
		t.Fatal("completeL4 refused a frame it should have summed")
	}
	got := binary.BigEndian.Uint16(f[50:])

	// What it must be: the sum of the whole frame from csumStart, since the
	// length field is unusable. The frame is restored first, because the call
	// above wrote its answer into the middle of the range being summed.
	binary.BigEndian.PutUint16(f[50:], 0)
	want := onesComplement(f[34:])
	if want == 0 {
		want = 0xffff // the convention completeL4 applies, mirrored here
	}
	if got != want {
		t.Errorf("summed to a wrapped length field: got %#04x, want %#04x over the whole frame",
			got, want)
	}
}

// The sum must stop at the end of the IP payload, not the end of the frame:
// a sender pads a short frame to the Ethernet minimum, and that padding is not
// part of the checksum. Finding where the payload ends means finding the IP
// header, and on a tagged link the type is not at byte 12 -- the tag the
// kernel stripped has been put back in front of it. Reading the type there
// finds neither IPv4 nor IPv6, turns the bound off, and sums the padding.
//
// It only shows when the padding is not zero and not all ones, both of which
// are invisible to a ones-complement sum, which is what makes it the kind of
// bug that survives a test suite.
func TestPaddingIsNotSummedThroughAnyTagging(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tags  []uint16 // TPIDs to insert, outermost first
		proto uint16
	}{
		{"untagged IPv4", nil, 0x0800},
		{"802.1Q IPv4", []uint16{0x8100}, 0x0800},
		{"802.1ad QinQ IPv4", []uint16{0x88a8, 0x8100}, 0x0800},
		{"802.1Q IPv6", []uint16{0x8100}, 0x86dd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l3 := 12 + 4*len(tc.tags)
			hdr := l3 + 2
			ipLen := 20
			if tc.proto == 0x86dd {
				ipLen = 40
			}
			const udp, payload = 8, 4
			// Padding exists only to reach the Ethernet minimum: 46 bytes
			// after the type. A frame already longer than that carries no
			// padding, and trailing bytes on one would not be padding but a
			// length field lying, which is a different case (and refused).
			used := hdr + ipLen + udp + payload
			minFrame := hdr + 46
			f := make([]byte, max(used, minFrame))
			for i, tp := range tc.tags {
				binary.BigEndian.PutUint16(f[12+4*i:], tp)
				binary.BigEndian.PutUint16(f[14+4*i:], uint16(7+i))
			}
			binary.BigEndian.PutUint16(f[l3:], tc.proto)
			if tc.proto == 0x0800 {
				f[hdr] = 0x45
				binary.BigEndian.PutUint16(f[hdr+2:], uint16(ipLen+udp+payload))
				f[hdr+9] = 17
			} else {
				f[hdr] = 0x60
				binary.BigEndian.PutUint16(f[hdr+4:], uint16(udp+payload))
				f[hdr+6] = 17
			}
			csumStart := hdr + ipLen
			const csumOff = 6
			for i := csumStart + udp; i < csumStart+udp+payload; i++ {
				f[i] = 0xaa
			}
			// Padding a sender left behind, and neither zeroes nor all-ones:
			// adding 0x0000 changes nothing, and adding 0xffff changes
			// nothing either once the end-around carry folds back in, so
			// only some other value can catch this.
			for i := used; i < len(f); i++ {
				f[i] = 0x5a
			}
			binary.BigEndian.PutUint16(f[csumStart+csumOff:], 0x1234)

			cp := append([]byte(nil), f...)
			if !completeL4(cp, csumStart, csumOff) {
				t.Fatal("completeL4 refused a well-formed frame")
			}
			got := binary.BigEndian.Uint16(cp[csumStart+csumOff:])
			want := onesComplement(f[csumStart:used])
			if want == 0 {
				want = 0xffff
			}
			if got != want {
				t.Errorf("summed %#04x, want %#04x: the padding after byte %d was included",
					got, want, used)
			}
		})
	}
}

// A length field that stops short excludes the checksum field itself from the
// summed range, so the pseudo-header partial being completed is never counted
// and the answer is confidently wrong -- and, since completion "succeeded",
// the frame is then marked complete and nothing downstream can tell. A sender
// can put any value in that field, so it is only believed where it could be
// telling the truth: stopping short of the frame is padding, and padding only
// exists to reach the Ethernet minimum.
func TestUnderstatedLengthIsNotTrusted(t *testing.T) {
	for _, totLen := range []int{0, 20, 24, 26, 27, 30} {
		f := make([]byte, 200)
		binary.BigEndian.PutUint16(f[12:], 0x0800)
		f[14] = 0x45
		f[23] = 17 // UDP
		const csumStart, csumOff = 34, 6
		for i := 34; i < len(f); i++ {
			f[i] = byte(i)
		}
		binary.BigEndian.PutUint16(f[16:], uint16(totLen))
		binary.BigEndian.PutUint16(f[csumStart+csumOff:], 0xabcd)

		cp := append([]byte(nil), f...)
		if !completeL4(cp, csumStart, csumOff) {
			t.Fatalf("tot_len=%d: refused a frame it should have summed", totLen)
		}
		got := binary.BigEndian.Uint16(cp[csumStart+csumOff:])

		// The frame is 200 bytes, far past the Ethernet minimum, so there is
		// no padding and the whole frame is the range.
		binary.BigEndian.PutUint16(f[csumStart+csumOff:], 0xabcd)
		want := onesComplement(f[csumStart:])
		if want == 0 {
			want = 0xffff
		}
		if got != want {
			t.Errorf("tot_len=%d: summed %#04x, want %#04x over the whole frame",
				totLen, got, want)
		}
	}
}

// A packet larger than one frame is laid across several, every one but the
// last marked as continuing, and the pieces concatenate back into what
// arrived. Without WithMultiBuffer the same packet is counted oversize and
// dropped, which is what a caller that cannot read a chain needs.
func TestRingChainsAPacketAcrossFrames(t *testing.T) {
	const bufLen = 100
	pkt := eth(1, 350) // more than three buffers' worth

	drainChained := func(r *ring, max int) ([][]byte, []bool, int) {
		bufs := make([][]byte, 0, max)
		lens := make([]int, max)
		cont := make([]bool, max)
		n := r.read(max, func(i int) []byte {
			for len(bufs) <= i {
				bufs = append(bufs, make([]byte, bufLen))
			}
			return bufs[i]
		}, lens, nil, nil, cont)
		out := make([][]byte, n)
		for i := 0; i < n; i++ {
			out[i] = bufs[i][:lens[i]]
		}
		return out, cont[:n], n
	}

	t.Run("chaining on", func(t *testing.T) {
		r := newTestRing(4)
		r.chain = true
		armBlock(t, r, 0, 0, ringFrame{data: pkt})

		pieces, cont, n := drainChained(r, 8)
		if n < 4 {
			t.Fatalf("a %d-byte packet came back in %d buffers of %d", len(pkt), n, bufLen)
		}
		var whole []byte
		for i, p := range pieces {
			whole = append(whole, p...)
			last := i == n-1
			if cont[i] == last {
				t.Errorf("buffer %d of %d: continued=%v, want %v", i, n, cont[i], !last)
			}
		}
		if !bytes.Equal(whole, pkt) {
			t.Errorf("the pieces concatenate to %d bytes, want the %d that arrived", len(whole), len(pkt))
		}
	})

	t.Run("chaining off drops it, as before", func(t *testing.T) {
		r := newTestRing(4)
		armBlock(t, r, 0, 0, ringFrame{data: pkt})
		if _, _, n := drainChained(r, 8); n != 0 {
			t.Errorf("delivered %d buffers of a packet that does not fit one", n)
		}
		if got := r.oversize.Load(); got != 1 {
			t.Errorf("oversize counted %d, want 1", got)
		}
	})

	t.Run("a batch too small for the packet drops it rather than stalling", func(t *testing.T) {
		r := newTestRing(4)
		r.chain = true
		armBlock(t, r, 0, 0, ringFrame{data: pkt})

		// Two slots can never hold a 350-byte packet in 100-byte buffers, and
		// a caller with a fixed batch size would ask again forever. It is
		// counted and stepped over, as a packet too big for one frame is when
		// chaining is off -- never delivered in pieces.
		if _, _, n := drainChained(r, 2); n != 0 {
			t.Fatalf("delivered %d pieces of a packet that does not fit the batch", n)
		}
		if got := r.oversize.Load(); got != 1 {
			t.Errorf("oversize counted %d, want 1: a dropped packet has to be visible", got)
		}
		// And the queue keeps working rather than wedging on it.
		armBlock(t, r, 1, 0, ringFrame{data: eth(2, 50)})
		if _, _, n := drainChained(r, 8); n != 1 {
			t.Errorf("the ring delivered %d packets after the drop, want the next one", n)
		}
	})

	t.Run("a chain that does not fit the room left is kept for next time", func(t *testing.T) {
		r := newTestRing(4)
		r.chain = true
		// A small packet, then one needing four slots. A batch of three takes
		// the small one and must leave the chain whole rather than splitting
		// it across two calls.
		armBlock(t, r, 0, 0, ringFrame{data: eth(3, 50)}, ringFrame{data: pkt})

		_, _, n := drainChained(r, 3)
		if n != 1 {
			t.Fatalf("took %d slots, want just the small packet", n)
		}
		if got := r.oversize.Load(); got != 0 {
			t.Errorf("oversize counted %d: the chain was dropped, not kept", got)
		}
		if _, _, n := drainChained(r, 8); n < 4 {
			t.Errorf("the chain was lost: got %d buffers on the retry", n)
		}
	})
}

// Only the first slot of a chain carries offload metadata. The others must be
// zero, not whatever the previous call left in the scratch: completeOffloads
// walks every slot, and a stale NeedsCsum on a continuation would have it
// write two bytes into the middle of a packet.
func TestChainContinuationsCarryNoStaleOffload(t *testing.T) {
	const bufLen = 100
	r := newTestRing(8)
	r.chain = true
	r.vnet = true

	lens := make([]int, 32)
	cont := make([]bool, 32)
	offs := make([]packetio.Offload, 32)
	bufs := make([][]byte, 32)
	dst := func(i int) []byte {
		for len(bufs) <= i {
			bufs = append(bufs, make([]byte, bufLen))
		}
		if bufs[i] == nil {
			bufs[i] = make([]byte, bufLen)
		}
		return bufs[i]
	}

	// Dirty the scratch, exactly as a previous call would have.
	for i := range offs {
		offs[i] = packetio.Offload{Flags: packetio.OffloadNeedsCsum, CsumStart: 34, CsumOff: 16}
	}

	armVnet(t, r, 0, packetio.Offload{}, ringFrame{data: eth(1, 350)})
	n := r.read(32, dst, lens, offs, nil, cont)
	if n < 3 {
		t.Fatalf("read %d slots, want a chain", n)
	}
	for i := 1; i < n; i++ {
		if offs[i] != (packetio.Offload{}) {
			t.Errorf("continuation slot %d of %d carries %+v; a chain's metadata is the first slot's only",
				i, n, offs[i])
		}
	}
}

// A packet spread over several frames cannot have its checksum finished here:
// any one frame holds a fraction of the bytes the sum covers. Completing it
// from a fragment writes a confident wrong answer and then clears the flag
// that said it was unfinished, so nothing downstream can tell. It is left
// alone, exactly as a super-frame is.
func TestChainedPacketsKeepTheirPartial(t *testing.T) {
	const partial = 0x1234
	mk := func() []byte {
		f := make([]byte, 200)
		binary.BigEndian.PutUint16(f[12:], 0x0800)
		f[14] = 0x45
		binary.BigEndian.PutUint16(f[16:], 186)
		f[23] = 6
		binary.BigEndian.PutUint16(f[50:], partial)
		return f
	}
	offs := []packetio.Offload{
		{Flags: packetio.OffloadNeedsCsum, CsumStart: 34, CsumOff: 16},
		{}, {},
	}
	// cont says slots 0 and 1 continue: one packet across three frames.
	cont := []bool{true, true, false}
	frames := [][]byte{mk(), mk(), mk()}

	if bad := completeOffloads(offs, cont, func(i int) []byte { return frames[i] }); bad != 0 {
		t.Errorf("a chained packet was counted unfinishable %d times", bad)
	}
	if got := binary.BigEndian.Uint16(frames[0][50:]); got != partial {
		t.Errorf("the head frame's partial became %#04x; it must be left for whoever "+
			"puts the packet back together", got)
	}
	if offs[0].Flags&packetio.OffloadNeedsCsum == 0 {
		t.Error("the flag was cleared, so nothing downstream can tell the sum is unfinished")
	}

	// The same frame on its own is still completed.
	solo := []packetio.Offload{{Flags: packetio.OffloadNeedsCsum, CsumStart: 34, CsumOff: 16}}
	one := mk()
	if bad := completeOffloads(solo, []bool{false}, func(int) []byte { return one }); bad != 0 {
		t.Fatalf("an unchained frame was refused %d times", bad)
	}
	if got := binary.BigEndian.Uint16(one[50:]); got == partial {
		t.Error("an unchained frame was left partial")
	}
}

// A packet too big for the caller's whole batch is dropped, and the slots the
// abandoned attempt touched must not stay marked. They used to: spread marked
// each slot as it filled it and only unmarked the last one on success, so the
// marks outlived a failure. The next packet was then delivered into slot 0 by
// the single-frame path -- which writes lens and offs but never cont -- and
// arrived whole, claiming to continue into a packet that was never delivered.
// A caller reassembling chains joins it to whatever comes next.
func TestDroppedChainDoesNotMarkTheNextPacket(t *testing.T) {
	r := newTestRing(4)
	r.chain = true
	// 350 bytes needs four 100-byte slots; the batch below has two.
	armBlock(t, r, 0, 0, ringFrame{data: eth(1, 350)}, ringFrame{data: eth(2, 60)})

	lens := make([]int, 8)
	cont := make([]bool, 8)
	bufs := make([][]byte, 8)
	for i := range bufs {
		bufs[i] = make([]byte, 100)
	}

	n := r.read(2, func(i int) []byte { return bufs[i] }, lens, nil, nil, cont)
	if n != 1 {
		t.Fatalf("read %d slots, want the one packet that fits", n)
	}
	if r.oversize.Load() != 1 {
		t.Errorf("the undeliverable packet was not counted: oversize=%d", r.oversize.Load())
	}
	if cont[0] {
		t.Errorf("a whole %d-byte packet says it continues into the next slot, "+
			"left over from the chain that was dropped before it", lens[0])
	}
}
