package wqe

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// The bytes of a multi-packet entry, spelled out by hand: a control segment, a
// compact sixteen-byte Ethernet segment with no inline header, and then each
// packet behind its own length with the top bit set to say the bytes follow
// rather than an address.
func TestMPWGoldenBytes(t *testing.T) {
	sq := newTestSQ(t, 8)
	s, err := NewMPWSender(sq, MPWConfig{QPN: 0x1234, MaxLen: 256})
	if err != nil {
		t.Fatalf("NewMPWSender: %v", err)
	}

	a, b := testFrame(20), testFrame(18)
	frames := [][]byte{a, b}
	if got := s.Fits(frames); got != 2 {
		t.Fatalf("Fits = %d, want 2", got)
	}
	// Twenty bytes takes 4+20 rounded up to 32, eighteen takes 4+18 rounded up
	// to 32: two octowords each, plus two for the header, is six octowords, so
	// two blocks.
	if bbs := s.Put(1, frames, true); bbs != 2 {
		t.Fatalf("the entry took %d blocks, want 2", bbs)
	}

	want := concat(
		// Control segment: the multi-packet opcode, six octowords.
		"00"+"0001"+"29",
		"001234"+"06",
		"00"+"0000"+"08", // ask for a completion
		"00000000",
		// Ethernet segment, compact: no inline header at all.
		"00000000", "00"+"00", "0000", "00000000", "0000"+"0000",
		// First packet: length 20 with the top bit set, the bytes, padding.
		"80000014", hex.EncodeToString(a), "00000000000000000000000000000000"[:2*(32-4-20)],
		// Second packet: length 18, the bytes, padding.
		"80000012", hex.EncodeToString(b), "00000000000000000000000000000000"[:2*(32-4-18)],
	)
	got := sq.Bytes()[WQEBB : WQEBB+len(want)]
	if !bytes.Equal(got, want) {
		t.Fatalf("entry mismatch\ngot  %s\nwant %s", hex.EncodeToString(got), hex.EncodeToString(want))
	}
}

// How many packets fit is bounded by the sixty octowords an entry may have, and
// a packet too long to copy stops the run rather than being truncated.
func TestMPWFits(t *testing.T) {
	sq := newTestSQ(t, 64)
	s, err := NewMPWSender(sq, MPWConfig{QPN: 1, MaxLen: 60})
	if err != nil {
		t.Fatalf("NewMPWSender: %v", err)
	}
	// A sixty-byte packet takes four octowords, so fourteen of them fit
	// alongside the two-octoword header.
	if got := s.MaxPackets(); got != 14 {
		t.Errorf("MaxPackets = %d, want 14", got)
	}

	many := make([][]byte, 20)
	for i := range many {
		many[i] = testFrame(60)
	}
	if got := s.Fits(many); got != 14 {
		t.Errorf("Fits = %d, want 14", got)
	}

	// A packet longer than the sender will carry ends the run where it is.
	mixed := [][]byte{testFrame(60), testFrame(60), testFrame(61), testFrame(60)}
	if got := s.Fits(mixed); got != 2 {
		t.Errorf("Fits stopped at %d, want 2 (the third is too long)", got)
	}
	// So does an empty one: an entry describing no bytes is a length error.
	if got := s.Fits([][]byte{testFrame(60), nil}); got != 1 {
		t.Errorf("Fits accepted an empty packet: %d", got)
	}
}

// The packets come out in the order they went in, at the offsets their lengths
// imply, whatever mixture of sizes is given.
func TestMPWMixedSizes(t *testing.T) {
	sq := newTestSQ(t, 64)
	s, err := NewMPWSender(sq, MPWConfig{QPN: 7, MaxLen: 256})
	if err != nil {
		t.Fatalf("NewMPWSender: %v", err)
	}

	sizes := []int{60, 14, 200, 1, 64}
	frames := make([][]byte, len(sizes))
	for i, n := range sizes {
		frames[i] = testFrame(n)
	}
	if got := s.Fits(frames); got != len(frames) {
		t.Fatalf("Fits = %d, want %d", got, len(frames))
	}
	s.Put(0, frames, false)

	at := CtrlSegSize + MPWEthSegSize
	for i, f := range frames {
		length := getBEU32(sq.Bytes()[at:])
		if length&MPWInlineFlag == 0 {
			t.Fatalf("packet %d: length %#x does not say the bytes follow", i, length)
		}
		if n := length &^ MPWInlineFlag; n != uint32(len(f)) {
			t.Fatalf("packet %d: length %d, want %d", i, n, len(f))
		}
		if got := sq.Bytes()[at+4 : at+4+len(f)]; !bytes.Equal(got, f) {
			t.Fatalf("packet %d does not match what was given", i)
		}
		at += int(mpwPacketOctowords(uint32(len(f)))) * Octoword
	}
}

// An entry may start near the end of the queue and continue at the start.
func TestMPWWrapsAroundTheRing(t *testing.T) {
	const bbs = 4
	sq := newTestSQ(t, bbs)
	s, err := NewMPWSender(sq, MPWConfig{QPN: 3, MaxLen: 128})
	if err != nil {
		t.Fatalf("NewMPWSender: %v", err)
	}

	frames := [][]byte{testFrame(100), testFrame(100)}
	s.Put(bbs-1, frames, true)

	buf := sq.Bytes()
	head := buf[(bbs-1)*WQEBB:]
	if head[3] != OpcodeEnhancedMPSW {
		t.Errorf("opcode %#x, want %#x", head[3], OpcodeEnhancedMPSW)
	}
	// The first packet begins 32 bytes into the entry, which is 32 bytes before
	// the end of this four-block queue, so its bytes run off the end.
	const at = (bbs-1)*WQEBB + CtrlSegSize + MPWEthSegSize
	if got := getBEU32(buf[at:]) &^ MPWInlineFlag; got != 100 {
		t.Fatalf("first length %d, want 100", got)
	}
	headBytes := bbs*WQEBB - (at + 4)
	if got := buf[at+4:]; !bytes.Equal(got, frames[0][:headBytes]) {
		t.Errorf("the head of the first packet is wrong")
	}
	if got := buf[:100-headBytes]; !bytes.Equal(got, frames[0][headBytes:]) {
		t.Errorf("the tail of the first packet did not wrap to the start")
	}
}

func TestNewMPWSenderRejectsImpossibleConfigurations(t *testing.T) {
	sq := newTestSQ(t, 16)
	if _, err := NewMPWSender(sq, MPWConfig{MaxLen: 0}); err == nil {
		t.Error("accepted packets of no bytes")
	}
	// A packet so long that two of them cannot share an entry is not worth
	// carrying this way.
	if _, err := NewMPWSender(sq, MPWConfig{MaxLen: 60 * Octoword}); err == nil {
		t.Error("accepted a packet that fills an entry by itself")
	}
}

func TestMPWPutDoesNotAllocate(t *testing.T) {
	sq := newTestSQ(t, 256)
	s, err := NewMPWSender(sq, MPWConfig{QPN: 1, MaxLen: 64})
	if err != nil {
		t.Fatalf("NewMPWSender: %v", err)
	}
	frames := make([][]byte, 14)
	for i := range frames {
		frames[i] = testFrame(60)
	}
	allocs := testing.AllocsPerRun(200, func() {
		for i := 0; i < 8; i++ {
			s.Put(uint16(i*16), frames, i == 7)
		}
	})
	if allocs != 0 {
		t.Fatalf("Put allocated %v times per run, want 0", allocs)
	}
}

// The bytes of a pointer entry: the same control and compact Ethernet
// segments, then one sixteen-byte data segment per packet with the top bit of
// the length clear -- fetch from this address, rather than the bytes follow.
func TestMPWPointerGoldenBytes(t *testing.T) {
	sq := newTestSQ(t, 8)
	s, err := NewMPWSender(sq, MPWConfig{QPN: 0x1234, Pointer: true, LKey: 0xabcd0001})
	if err != nil {
		t.Fatalf("NewMPWSender: %v", err)
	}
	if !s.Pointer() {
		t.Fatal("the sender does not say it writes pointer entries")
	}

	a, b := testFrame(60), testFrame(61)
	frames := [][]byte{a, b}
	addrs := []uint64{0x7f0000001000, 0x7f0000002000}
	if got := s.Fits(frames); got != 2 {
		t.Fatalf("Fits = %d, want 2", got)
	}
	// Two octowords of header and one per packet is four: one block.
	if bbs := s.PutPointers(1, frames, addrs, true); bbs != 1 {
		t.Fatalf("the entry took %d blocks, want 1", bbs)
	}

	want := concat(
		// Control segment: the multi-packet opcode, four octowords.
		"00"+"0001"+"29",
		"001234"+"04",
		"00"+"0000"+"08", // ask for a completion
		"00000000",
		// Ethernet segment, compact: no inline header at all.
		"00000000", "00"+"00", "0000", "00000000", "0000"+"0000",
		// First packet: length 60, top bit clear, key, address.
		"0000003c", "abcd0001", "00007f0000001000",
		// Second packet: length 61.
		"0000003d", "abcd0001", "00007f0000002000",
	)
	got := sq.Bytes()[WQEBB : WQEBB+len(want)]
	if !bytes.Equal(got, want) {
		t.Fatalf("entry mismatch\ngot  %s\nwant %s", hex.EncodeToString(got), hex.EncodeToString(want))
	}
}

// Pointer mode is bounded by the entry's octoword budget alone: 58 packets of
// any length, and an empty packet still ends the run.
func TestMPWPointerFits(t *testing.T) {
	sq := newTestSQ(t, 64)
	s, err := NewMPWSender(sq, MPWConfig{QPN: 1, Pointer: true, LKey: 1})
	if err != nil {
		t.Fatalf("NewMPWSender: %v", err)
	}
	if got := s.MaxPackets(); got != MaxDS-2 {
		t.Errorf("MaxPackets = %d, want %d", got, MaxDS-2)
	}
	many := make([][]byte, 100)
	for i := range many {
		many[i] = testFrame(1500) // long packets change nothing in pointer mode
	}
	if got := s.Fits(many); got != MaxDS-2 {
		t.Errorf("Fits = %d, want %d", got, MaxDS-2)
	}
	if got := s.Fits([][]byte{testFrame(60), nil, testFrame(60)}); got != 1 {
		t.Errorf("Fits accepted an empty packet: %d", got)
	}
}
