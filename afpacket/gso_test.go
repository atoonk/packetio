//go:build linux

package afpacket

import (
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
	}, lens, offs, nil)
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
	// WithGSO on its own raises the frame size and lowers the frame count, so
	// the default does not quietly reserve a quarter of a gigabyte.
	c := defaults()
	WithGSO()(&c)
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
	c = defaults()
	WithFrames(1024)(&c)
	WithGSO()(&c)
	if c.frames != 1024 {
		t.Errorf("WithGSO overrode an explicit WithFrames: got %d", c.frames)
	}

	// GSO must not be a hole through the rest of validation: the checks after
	// it used to be skipped entirely.
	c = defaults()
	WithGSO()(&c)
	WithFrames(8)(&c)
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
