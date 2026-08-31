//go:build linux

package afpacket

import (
	"bytes"
	"testing"
	"unsafe"
)

// A synthetic TPACKET_V3 ring. The reader only ever touches ring.mem, which is
// an ordinary byte slice (Mmap returns one), so a test can lay out block
// descriptors and frame headers by hand and drive the real block walk with no
// socket, no NIC and no root. It exists because the alternative is a receive
// path only reachable from root-gated tests that never run in CI.

// testMacOff is the frame-relative offset of the packet bytes: past the
// 48-byte tpacket3_hdr and 8-aligned.
const testMacOff = 64

// testFirstOff is where the first frame header goes, block-relative. It must
// clear the 48-byte block descriptor at the start of the block: laying a frame
// header at offset 0 overlays tp_snaplen on numPkts and tp_len on
// offsetToFirst, which makes the ring lie to itself in ways no kernel would.
const testFirstOff = 64

// ringFrame describes one frame to lay into a block. A zero override means
// "compute the correct value"; setting one is how a test forges the
// kernel-written cursor fields the reader is supposed to distrust.
type ringFrame struct {
	data []byte // packet bytes as the kernel stored them, VLAN tag stripped
	tpid uint16 // stripped 802.1Q tag; 0 means the frame arrived untagged
	tci  uint16

	macOff  uint16 // override tp_mac
	snaplen uint32 // override tp_snaplen
	next    uint32 // override tp_next_offset
}

func newTestRing(blocks uint32) *ring {
	return &ring{
		mem:       make([]byte, int(rxBlockSize)*int(blocks)),
		blockSize: rxBlockSize,
		blockNr:   blocks,
	}
}

// armBlock lays frames into block blk and marks it ready for userspace, exactly
// as the kernel would. A firstOff of 0 means testFirstOff; pass an explicit one
// only to forge the kernel's own cursor.
func armBlock(t *testing.T, r *ring, blk, firstOff uint32, frames ...ringFrame) {
	t.Helper()
	if firstOff == 0 {
		firstOff = testFirstOff
	}
	blockOff := blk * r.blockSize
	bd := r.blockDesc(blk)
	*bd = tpacketBlockDesc{version: 1, numPkts: uint32(len(frames)), offsetToFirst: firstOff}

	off := firstOff
	for i, fr := range frames {
		mac := fr.macOff
		if mac == 0 {
			mac = testMacOff
		}
		snap := fr.snaplen
		if snap == 0 {
			snap = uint32(len(fr.data))
		}
		stride := (uint32(mac) + uint32(len(fr.data)) + 7) &^ 7
		next := fr.next
		if next == 0 && i < len(frames)-1 {
			next = stride
		}
		tph := r.frameHdr(blockOff + off)
		*tph = tpacket3Hdr{
			tpNextOffset: next,
			tpSnaplen:    snap,
			tpLen:        snap,
			tpMac:        mac,
		}
		if fr.tpid != 0 {
			tph.tpStatus |= tpStatusVlanValid
			tph.tpVlanTpid = fr.tpid
			tph.tpVlanTci = uint32(fr.tci)
		}
		copy(r.mem[blockOff+off+uint32(mac):], fr.data)
		off += stride
	}
	bd.blockStatus = tpStatusUser
}

// drain reads up to max packets into fresh buffers of size bufLen.
func drain(r *ring, max, bufLen int) ([][]byte, int) {
	bufs := make([][]byte, 0, max)
	lens := make([]int, max)
	n := r.read(max, func(i int) []byte {
		for len(bufs) <= i {
			bufs = append(bufs, make([]byte, bufLen))
		}
		return bufs[i]
	}, lens, nil)
	out := make([][]byte, n)
	for i := 0; i < n; i++ {
		out[i] = bufs[i][:lens[i]]
	}
	return out, n
}

// eth builds a frame with a recognisable header and n bytes of payload.
func eth(tag byte, n int) []byte {
	b := make([]byte, 14+n)
	for i := 0; i < 6; i++ {
		b[i] = 0xaa
		b[6+i] = 0xbb
	}
	b[12], b[13] = 0x08, 0x00
	for i := range b[14:] {
		b[14+i] = tag
	}
	return b
}

func TestRingLayoutMatchesKernelStructs(t *testing.T) {
	// The kernel writes these; a wrong size means every field after it is read
	// from the wrong offset, which is the kind of bug that looks like garbage
	// packets rather than a compile error.
	if got := unsafe.Sizeof(tpacket3Hdr{}); got != 48 {
		t.Errorf("sizeof(tpacket3_hdr) = %d, want 48", got)
	}
	if got := unsafe.Sizeof(tpacketReq3{}); got != 28 {
		t.Errorf("sizeof(tpacket_req3) = %d, want 28", got)
	}
	if got := unsafe.Offsetof(tpacketBlockDesc{}.blockStatus); got != 8 {
		t.Errorf("offsetof(block_status) = %d, want 8", got)
	}
	if got := unsafe.Offsetof(tpacket3Hdr{}.tpMac); got != 24 {
		t.Errorf("offsetof(tp_mac) = %d, want 24", got)
	}
}

func TestRingReadsFramesInOrder(t *testing.T) {
	r := newTestRing(4)
	armBlock(t, r, 0, 0,
		ringFrame{data: eth(1, 50)},
		ringFrame{data: eth(2, 60)},
		ringFrame{data: eth(3, 70)},
	)
	got, n := drain(r, 8, 2048)
	if n != 3 {
		t.Fatalf("read %d packets, want 3", n)
	}
	for i, want := range [][]byte{eth(1, 50), eth(2, 60), eth(3, 70)} {
		if !bytes.Equal(got[i], want) {
			t.Errorf("packet %d: got %d bytes, want %d", i, len(got[i]), len(want))
		}
	}
}

func TestRingRetiresBlockAndAdvances(t *testing.T) {
	r := newTestRing(4)
	armBlock(t, r, 0, 0, ringFrame{data: eth(1, 20)})
	armBlock(t, r, 1, 0, ringFrame{data: eth(2, 20)})

	if _, n := drain(r, 8, 2048); n != 1 {
		t.Fatalf("first block: read %d, want 1", n)
	}
	if st := r.blockDesc(0).blockStatus; st != tpStatusKernel {
		t.Errorf("block 0 status = %d, want handed back to the kernel", st)
	}
	if r.block != 1 {
		t.Errorf("cursor at block %d, want 1", r.block)
	}
	if _, n := drain(r, 8, 2048); n != 1 {
		t.Errorf("second block: read %d, want 1", n)
	}
}

func TestRingResumesPartlyDrainedBlock(t *testing.T) {
	r := newTestRing(4)
	armBlock(t, r, 0, 0,
		ringFrame{data: eth(1, 20)},
		ringFrame{data: eth(2, 20)},
		ringFrame{data: eth(3, 20)},
	)
	// Take one, leaving two: the block must not be retired or re-read from the
	// start, or the same packets arrive twice.
	if _, n := drain(r, 1, 2048); n != 1 {
		t.Fatalf("read %d, want 1", n)
	}
	if !r.pending {
		t.Error("a partly drained block should be pending")
	}
	if st := r.blockDesc(0).blockStatus; st != tpStatusUser {
		t.Error("a partly drained block must not be handed back")
	}
	got, n := drain(r, 8, 2048)
	if n != 2 {
		t.Fatalf("read %d, want the remaining 2", n)
	}
	if !bytes.Equal(got[0], eth(2, 20)) {
		t.Error("resumed at the wrong frame")
	}
}

func TestRingReinsertsStrippedVLANTag(t *testing.T) {
	r := newTestRing(4)
	// The kernel hands up the frame with the tag removed and its value in the
	// header; what the caller gets must look like the wire again.
	payload := eth(7, 40)
	armBlock(t, r, 0, 0, ringFrame{data: payload, tpid: 0x8100, tci: 2053})
	got, n := drain(r, 8, 2048)
	if n != 1 {
		t.Fatalf("read %d, want 1", n)
	}
	if len(got[0]) != len(payload)+4 {
		t.Fatalf("got %d bytes, want %d with the tag put back", len(got[0]), len(payload)+4)
	}
	if !bytes.Equal(got[0][:12], payload[:12]) {
		t.Error("the MAC addresses moved")
	}
	if got[0][12] != 0x81 || got[0][13] != 0x00 {
		t.Errorf("tpid = %02x%02x, want 8100", got[0][12], got[0][13])
	}
	if tci := uint16(got[0][14])<<8 | uint16(got[0][15]); tci != 2053 {
		t.Errorf("tci = %d, want 2053", tci)
	}
	if !bytes.Equal(got[0][16:], payload[12:]) {
		t.Error("the rest of the frame did not survive the tag insertion")
	}
}

func TestRingDefaultsMissingTPID(t *testing.T) {
	r := newTestRing(4)
	// tp_vlan_tpid of 0 with the valid bit set means an ordinary 802.1Q tag.
	frames := []ringFrame{{data: eth(1, 40), tci: 100}}
	frames[0].tpid = 0
	armBlock(t, r, 0, 0, frames...)
	// armBlock only sets the valid bit when tpid != 0, so set it by hand.
	tph := r.frameHdr(testFirstOff)
	tph.tpStatus |= tpStatusVlanValid
	tph.tpVlanTci = 100

	got, n := drain(r, 8, 2048)
	if n != 1 {
		t.Fatalf("read %d, want 1", n)
	}
	if got[0][12] != 0x81 || got[0][13] != 0x00 {
		t.Errorf("tpid = %02x%02x, want the 8100 default", got[0][12], got[0][13])
	}
}

func TestRingSkipsFrameTooBigForTheBuffer(t *testing.T) {
	r := newTestRing(4)
	armBlock(t, r, 0, 0,
		ringFrame{data: eth(1, 20)},   // fits
		ringFrame{data: eth(2, 1500)}, // does not
		ringFrame{data: eth(3, 20)},   // fits
	)
	got, n := drain(r, 8, 256)
	if n != 2 {
		t.Fatalf("read %d, want the 2 that fit", n)
	}
	// The critical property: the packets that came back must be the ones that
	// fit, in order, with their own lengths. A skipped frame that shifted the
	// pairing would show up here as packet 1 carrying frame 3's contents at
	// frame 2's length.
	if !bytes.Equal(got[0], eth(1, 20)) {
		t.Error("first packet is not the first frame that fit")
	}
	if !bytes.Equal(got[1], eth(3, 20)) {
		t.Error("second packet is not the third frame; the skip shifted the pairing")
	}
	if r.oversize.Load() != 1 {
		t.Errorf("oversize = %d, want 1", r.oversize.Load())
	}
}

func TestRingStopsWhenNoBufferIsOffered(t *testing.T) {
	r := newTestRing(4)
	armBlock(t, r, 0, 0,
		ringFrame{data: eth(1, 20)},
		ringFrame{data: eth(2, 20)},
	)
	lens := make([]int, 4)
	buf := make([]byte, 2048)
	n := r.read(4, func(i int) []byte {
		if i == 1 {
			return nil // the pool has run dry
		}
		return buf
	}, lens, nil)
	if n != 1 {
		t.Fatalf("read %d, want 1 before the pool ran dry", n)
	}
	if !r.pending {
		t.Error("the block must stay pending so the rest is not lost")
	}
	if st := r.blockDesc(0).blockStatus; st != tpStatusUser {
		t.Error("the block must not be handed back with frames still in it")
	}
}

func TestRingIgnoresBlockTheKernelStillOwns(t *testing.T) {
	r := newTestRing(4)
	armBlock(t, r, 0, 0, ringFrame{data: eth(1, 20)})
	r.blockDesc(0).blockStatus = tpStatusKernel
	if _, n := drain(r, 8, 2048); n != 0 {
		t.Errorf("read %d from a block the kernel owns, want 0", n)
	}
}

// The rest of these forge the kernel-written cursor fields. Every one of them
// would, without its check, walk the unchecked struct read past the mapping or
// into a block the kernel still owns.

func TestRingRejectsSnaplenPastTheBlock(t *testing.T) {
	r := newTestRing(4)
	armBlock(t, r, 0, 0, ringFrame{data: eth(1, 20), snaplen: rxBlockSize * 4})
	if _, n := drain(r, 8, 2048); n != 0 {
		t.Errorf("read %d with a snaplen past the block, want 0", n)
	}
	if r.oversize.Load() == 0 {
		t.Error("an out-of-block frame should be counted")
	}
}

func TestRingRejectsSnaplenThatWouldWrap(t *testing.T) {
	r := newTestRing(4)
	// tp_mac + tp_snaplen would overflow a uint32 and pass a naive check. This
	// is the one that got through an earlier version.
	armBlock(t, r, 0, 0, ringFrame{data: eth(1, 20), snaplen: ^uint32(0) - 8})
	if _, n := drain(r, 8, 2048); n != 0 {
		t.Errorf("read %d with a wrapping snaplen, want 0", n)
	}
}

func TestRingRejectsMacOffsetPastTheBlock(t *testing.T) {
	r := newTestRing(4)
	armBlock(t, r, 0, 0, ringFrame{data: eth(1, 20), macOff: uint16(rxBlockSize - 8)})
	if _, n := drain(r, 8, 2048); n != 0 {
		t.Errorf("read %d with tp_mac past the block, want 0", n)
	}
}

func TestRingRejectsHeaderStraddlingTheBlockEnd(t *testing.T) {
	r := newTestRing(4)
	// offset_to_first_pkt leaves no room for even the frame header.
	armBlock(t, r, 0, rxBlockSize-16, ringFrame{data: eth(1, 20)})
	if _, n := drain(r, 8, 2048); n != 0 {
		t.Errorf("read %d with a header past the block end, want 0", n)
	}
	if r.oversize.Load() == 0 {
		t.Error("a header that does not fit should be counted")
	}
}

func TestRingStopsOnZeroNextOffset(t *testing.T) {
	r := newTestRing(4)
	// numPkts claims three, but the first frame says it is the last. The
	// kernel's own "last frame" marker wins, or the walk reads whatever
	// happens to be in the block next.
	armBlock(t, r, 0, 0,
		ringFrame{data: eth(1, 20)},
		ringFrame{data: eth(2, 20)},
		ringFrame{data: eth(3, 20)},
	)
	// armBlock cannot tell "next: 0" from "work it out", so forge it here.
	r.frameHdr(testFirstOff).tpNextOffset = 0
	r.blockDesc(0).numPkts = 3
	_, n := drain(r, 8, 2048)
	if n != 1 {
		t.Errorf("read %d, want 1: a zero tp_next_offset ends the block", n)
	}
	if r.pending {
		t.Error("the block should have been retired, not left pending")
	}
}

func TestRingHandlesEmptyBlock(t *testing.T) {
	r := newTestRing(4)
	armBlock(t, r, 0, 0)
	if _, n := drain(r, 8, 2048); n != 0 {
		t.Errorf("read %d from an empty block, want 0", n)
	}
	if r.block != 1 {
		t.Error("an empty block should still be retired and stepped past")
	}
}

func TestRingWrapsAtTheLastBlock(t *testing.T) {
	const blocks = 3
	r := newTestRing(blocks)
	for b := uint32(0); b < blocks; b++ {
		armBlock(t, r, b, 0, ringFrame{data: eth(byte(b+1), 20)})
	}
	for i := 0; i < blocks; i++ {
		if _, n := drain(r, 8, 2048); n != 1 {
			t.Fatalf("block %d: read %d, want 1", i, n)
		}
	}
	if r.block != 0 {
		t.Errorf("cursor at block %d after a full lap, want 0", r.block)
	}
}
