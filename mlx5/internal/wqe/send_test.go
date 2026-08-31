package wqe

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func newTestSQ(t *testing.T, bbs int) SQ {
	t.Helper()
	sq, err := NewSQ(make([]byte, bbs*WQEBB))
	if err != nil {
		t.Fatalf("NewSQ: %v", err)
	}
	return sq
}

func testFrame(n int) []byte {
	f := make([]byte, n)
	for i := range f {
		f[i] = byte(i + 1)
	}
	return f
}

// The whole point of this package is that the bytes are right, so the first
// test spells out every one of them by hand rather than deriving them from the
// code under test.
//
// A SEND entry with the usual 18-byte inline header is exactly one work queue
// buffer block: a 16-byte control segment, a 32-byte Ethernet segment whose
// last 18 bytes are the packet's L2 header, and a 16-byte data segment
// pointing at the rest of the packet.
func TestSendWQEGoldenBytes(t *testing.T) {
	sq := newTestSQ(t, 4)
	s, err := NewSender(sq, SenderConfig{QPN: 0x1234, LKey: 0xdeadbeef, InlineLen: 18})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	if s.MaxBBs() != 1 {
		t.Fatalf("an entry may be %d blocks, want 1", s.MaxBBs())
	}

	frame := testFrame(64)
	if bbs := s.Put(0x0102, frame, 0x1000, true); bbs != 1 {
		t.Fatalf("the entry took %d blocks, want 1", bbs)
	}

	want := concat(
		// Control segment.
		"00"+"0102"+"0a", //   opmod, producer index 0x0102, opcode SEND
		"001234"+"04",    //   queue pair 0x1234, 4 octowords
		"00"+"0000"+"08", //   signature, stream id, ask for a completion
		"00000000",       //   immediate data, unused
		// Ethernet segment.
		"00000000",                     //         reserved
		"00"+"00",                      //         no checksum offload, no software parser flags
		"0000",                         //         no segmentation
		"00000000",                     //         reserved
		"0012",                         //         18 inline bytes follow
		hex.EncodeToString(frame[:18]), // exactly fills the segment: 14 + 18 = 32
		// Data segment: the remaining 46 bytes, at the frame address plus 18.
		"0000002e", "deadbeef", "0000000000001012",
	)
	// Index 0x0102 lands in block 0x0102 modulo the four the queue has.
	off := (0x0102 % 4) * WQEBB
	if got := sq.Bytes()[off : off+WQEBB]; !bytes.Equal(got, want) {
		t.Fatalf("work queue entry mismatch\ngot  %s\nwant %s", hex.EncodeToString(got), hex.EncodeToString(want))
	}
}

// With no inline header the Ethernet segment collapses to a single octoword and
// the whole packet is pointed at, which is the shape enhanced multi-packet
// entries will need later.
func TestSendWQENoInlineHeader(t *testing.T) {
	sq := newTestSQ(t, 4)
	s, err := NewSender(sq, SenderConfig{QPN: 1, LKey: 2, InlineLen: 0})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	frame := testFrame(64)
	if bbs := s.Put(0, frame, 0x2000, false); bbs != 1 {
		t.Fatalf("the entry took %d blocks, want 1", bbs)
	}
	e := sq.Bytes()[:WQEBB]

	if got := getBEU32(e[ctrlQPNDS:]); got != 1<<8|3 {
		t.Errorf("qpn_ds = %#x, want %#x", got, 1<<8|3)
	}
	if e[ctrlFMCESE] != 0 {
		t.Errorf("unsignaled entry asked for a completion")
	}
	if got := getBEU16(e[ethInlineHdrSz:]); got != 0 {
		t.Errorf("inline header size = %d, want 0", got)
	}
	// The data segment sits right after the one-octoword Ethernet segment.
	const dseg = CtrlSegSize + Octoword
	if got := getBEU32(e[dseg+dsegByteCount:]); got != 64 {
		t.Errorf("byte count = %d, want 64", got)
	}
	if got := getBEU64(e[dseg+dsegAddr:]); got != 0x2000 {
		t.Errorf("address = %#x, want 0x2000", got)
	}
}

// A device in L2 inline mode carrying QinQ frames needs 22 inline bytes, which
// no longer fits one block. Nothing may assume 18.
func TestSendWQEQinQInlineHeaderSpansTwoBlocks(t *testing.T) {
	sq := newTestSQ(t, 8)
	s, err := NewSender(sq, SenderConfig{QPN: 7, LKey: 9, InlineLen: 22})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	// 14 bytes of segment header plus 22 inline is 36, three octowords; with
	// the control and data segments that is five, so two blocks.
	frame := testFrame(100)
	if bbs := s.Put(3, frame, 0x4000, false); bbs != 2 {
		t.Fatalf("the entry took %d blocks, want 2", bbs)
	}
	if got := getBEU32(sq.Bytes()[3*WQEBB+ctrlQPNDS:]) & 0xff; got != 5 {
		t.Errorf("ds = %d, want 5", got)
	}
	e := sq.Bytes()[3*WQEBB : 5*WQEBB]

	if got := getBEU16(e[ethInlineHdrSz:]); got != 22 {
		t.Errorf("inline header size = %d, want 22", got)
	}
	if got := e[ethInlineHdr : ethInlineHdr+22]; !bytes.Equal(got, frame[:22]) {
		t.Errorf("inline header = % x, want % x", got, frame[:22])
	}
	const dseg = CtrlSegSize + 3*Octoword
	if got := getBEU32(e[dseg+dsegByteCount:]); got != 100-22 {
		t.Errorf("byte count = %d, want %d", got, 100-22)
	}
	if got := getBEU64(e[dseg+dsegAddr:]); got != 0x4000+22 {
		t.Errorf("address = %#x, want %#x", got, 0x4000+22)
	}
}

// A packet shorter than the inline header rides entirely inside the entry, and
// its data segment must describe nothing at all rather than a stale address.
func TestSendWQEShortFrameIsFullyInline(t *testing.T) {
	sq := newTestSQ(t, 4)
	s, err := NewSender(sq, SenderConfig{QPN: 1, LKey: 0x55, InlineLen: 18})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	// Leave a previous entry behind, so that anything not rewritten shows up.
	s.Put(1, testFrame(64), 0x9000, false)

	frame := testFrame(10)
	if bbs := s.Put(1, frame, 0x8000, false); bbs != 1 {
		t.Fatalf("the entry took %d blocks, want 1", bbs)
	}
	e := sq.Bytes()[WQEBB : 2*WQEBB]

	if got := getBEU16(e[ethInlineHdrSz:]); got != 10 {
		t.Errorf("inline header size = %d, want 10", got)
	}
	if got := e[ethInlineHdr : ethInlineHdr+10]; !bytes.Equal(got, frame) {
		t.Errorf("inline bytes = % x, want % x", got, frame)
	}

	// A packet that fits entirely inside the entry has no data segment. The
	// hardware rejects an entry whose data segment describes zero bytes with a
	// length error, so the segment must be absent, not empty: ds counts the
	// control segment and a two-octoword Ethernet segment and nothing else.
	if got := getBEU32(e[ctrlQPNDS:]) & 0xff; got != 3 {
		t.Errorf("ds = %d, want 3: a control segment and two octowords of Ethernet segment", got)
	}
	// The padding after the inline header is cleared rather than left as
	// whatever the previous entry put there.
	for i := ethInlineHdr + 10; i < CtrlSegSize+2*Octoword; i++ {
		if e[i] != 0 {
			t.Errorf("byte %d of the inline area is %#02x, want zero", i, e[i])
			break
		}
	}
}

// The send queue is cyclic and the hardware reads it that way, so an entry may
// begin in the last block and finish in the first. Here the inline header
// itself straddles the end.
func TestSendWQEWrapsAroundTheRing(t *testing.T) {
	const bbs = 4
	sq := newTestSQ(t, bbs)
	// 14 bytes of segment header plus 40 inline is 54, four octowords; with the
	// control and data segments that is six, so two blocks.
	s, err := NewSender(sq, SenderConfig{QPN: 7, LKey: 9, InlineLen: 40})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	if s.MaxBBs() != 2 {
		t.Fatalf("an entry may be %d blocks, want 2", s.MaxBBs())
	}

	frame := testFrame(100)
	s.Put(bbs-1, frame, 0x4000, true) // starts in the last block

	buf := sq.Bytes()
	head := buf[(bbs-1)*WQEBB:]
	if head[ctrlFMCESE] != CtrlCQUpdate {
		t.Error("completion request lost at the ring end")
	}
	if got := getBEU16(head[ethInlineHdrSz:]); got != 40 {
		t.Errorf("inline header size = %d, want 40", got)
	}

	// The inline header starts 30 bytes into the entry, which is 34 bytes
	// before the end of the buffer, so its last six bytes wrap to the start.
	const inlineAbs = (bbs-1)*WQEBB + ethInlineHdr
	const headBytes = bbs*WQEBB - inlineAbs
	if got := buf[inlineAbs:]; !bytes.Equal(got, frame[:headBytes]) {
		t.Errorf("inline head = % x, want % x", got, frame[:headBytes])
	}
	if got := buf[:40-headBytes]; !bytes.Equal(got, frame[headBytes:40]) {
		t.Errorf("inline tail = % x, want % x", got, frame[headBytes:40])
	}

	// The data segment is four octowords past the control segment, which is
	// 16 bytes into the wrapped part of the buffer.
	const dsegInTail = (bbs-1)*WQEBB + CtrlSegSize + 4*Octoword - bbs*WQEBB
	if got := getBEU32(buf[dsegInTail+dsegByteCount:]); got != 100-40 {
		t.Errorf("byte count = %d, want %d", got, 100-40)
	}
	if got := getBEU64(buf[dsegInTail+dsegAddr:]); got != 0x4000+40 {
		t.Errorf("address = %#x, want %#x", got, 0x4000+40)
	}
	if got := getBEU32(buf[dsegInTail+dsegLKey:]); got != 9 {
		t.Errorf("memory key = %d, want 9", got)
	}
}

// The producer index is written into the middle of a big-endian word, and it
// wraps at 65536 independently of the queue size.
func TestSendWQEProducerIndex(t *testing.T) {
	sq := newTestSQ(t, 8)
	s, err := NewSender(sq, SenderConfig{QPN: 0xabcdef, LKey: 1, InlineLen: 18})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	for _, idx := range []uint16{0, 1, 7, 0x1234, 0xfff8, 0xffff} {
		s.Put(idx, testFrame(64), 0x1000, false)
		e := sq.Bytes()[int(idx%8)*WQEBB:]
		got := getBEU32(e[ctrlOpmodIdxOpcode:])
		want := uint32(idx)<<8 | OpcodeSend
		if got != want {
			t.Errorf("index %#x: opcode word = %#08x, want %#08x", idx, got, want)
		}
		if got := getBEU32(e[ctrlQPNDS:]); got != 0xabcdef<<8|4 {
			t.Errorf("index %#x: qpn_ds = %#x", idx, got)
		}
	}
}

// The doorbell writes the first eight bytes of the entry, read as they lie.
func TestFirstOctoword(t *testing.T) {
	sq := newTestSQ(t, 4)
	s, err := NewSender(sq, SenderConfig{QPN: 0x1234, LKey: 1, InlineLen: 18})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	s.Put(2, testFrame(64), 0x1000, true)

	var want uint64
	for i, b := range sq.Bytes()[2*WQEBB : 2*WQEBB+8] {
		want |= uint64(b) << (8 * i) // little-endian host
	}
	if got := s.FirstOctoword(2); got != want {
		t.Errorf("FirstOctoword = %#x, want %#x", got, want)
	}
}

func TestNewSenderRejectsImpossibleConfigurations(t *testing.T) {
	sq := newTestSQ(t, 4)
	if _, err := NewSender(sq, SenderConfig{InlineLen: -1}); err == nil {
		t.Error("negative inline length accepted")
	}
	// 60 octowords is the hardware limit; an inline header that large cannot
	// leave room for the control and data segments.
	if _, err := NewSender(sq, SenderConfig{InlineLen: 60 * Octoword}); err == nil {
		t.Error("oversized inline header accepted")
	}
	// An entry larger than the whole queue.
	small := newTestSQ(t, 1)
	if _, err := NewSender(small, SenderConfig{InlineLen: 22}); err == nil {
		t.Error("entry larger than the queue accepted")
	}
}

func TestNewSQRejectsBadBuffers(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
	}{
		{"empty", 0},
		{"not a whole block", 100},
		{"not a power of two", 3 * WQEBB},
	} {
		if _, err := NewSQ(make([]byte, tc.n)); err == nil {
			t.Errorf("%s buffer accepted", tc.name)
		}
	}
}

// Writing an entry is on the packet path and must not allocate.
func TestPutDoesNotAllocate(t *testing.T) {
	sq := newTestSQ(t, 64)
	s, err := NewSender(sq, SenderConfig{QPN: 1, LKey: 1, InlineLen: 18})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	frame := testFrame(64)
	allocs := testing.AllocsPerRun(200, func() {
		for i := 0; i < 32; i++ {
			s.Put(uint16(i), frame, 0x1000+uint64(i)*2048, i == 31)
		}
	})
	if allocs != 0 {
		t.Fatalf("Put allocated %v times per run, want 0", allocs)
	}
}

func concat(parts ...string) []byte {
	var b []byte
	for _, p := range parts {
		d, err := hex.DecodeString(p)
		if err != nil {
			panic(err)
		}
		b = append(b, d...)
	}
	return b
}
