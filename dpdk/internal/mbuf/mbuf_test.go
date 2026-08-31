package mbuf

import (
	"encoding/binary"
	"testing"
)

// The layout the M0 spike measured on the rig, and the one the backend builds:
// 2048-byte frames, a 64-byte mempool object header, a 128-byte mbuf with no
// private area, and DPDK's 128 bytes of headroom.
func testLayout() Layout {
	return Layout{FrameSize: 2048, ObjHeader: 64, MbufSize: Size, Headroom: 128}
}

func TestLayoutArithmetic(t *testing.T) {
	l := testLayout()
	if err := l.Validate(); err != nil {
		t.Fatalf("the measured layout does not validate: %v", err)
	}
	// Frame 3 starts at 6144; its mbuf, buffer and packet follow at fixed
	// offsets. These are the numbers the spike read out of a live mempool.
	const frame = 3 * 2048
	if got, want := l.MbufOffset(frame), frame+64; got != want {
		t.Errorf("MbufOffset = %d, want %d", got, want)
	}
	if got, want := l.BufferOffset(frame), frame+192; got != want {
		t.Errorf("BufferOffset = %d, want %d", got, want)
	}
	if got, want := l.DataStart(), 320; got != want {
		t.Errorf("DataStart = %d, want %d", got, want)
	}
	// The buffer must end exactly at the end of the frame, or the NIC would be
	// told it may write into the next one.
	if got := l.BufferOffset(frame) + l.BufferLen(); got != frame+l.FrameSize {
		t.Errorf("the buffer ends at %d, want the frame end %d", got, frame+l.FrameSize)
	}
	if got, want := l.MaxPacket(), 2048-320; got != want {
		t.Errorf("MaxPacket = %d, want %d", got, want)
	}
}

// Finding the frame from an address inside it is what every Free and Recycle
// does, so it has to be right at both ends of a frame and cost a mask.
func TestFrameOf(t *testing.T) {
	l := testLayout()
	for _, c := range []struct{ off, want uint64 }{
		{0, 0},
		{319, 0},
		{320, 0},
		{2047, 0},
		{2048, 2048},
		{2048 + 320, 2048},
		{4095, 2048},
		{10 * 2048, 10 * 2048},
		{10*2048 + 1999, 10 * 2048},
	} {
		if got := l.FrameOf(c.off); got != c.want {
			t.Errorf("FrameOf(%d) = %d, want %d", c.off, got, c.want)
		}
	}
}

func TestLayoutRefusesTheImpossible(t *testing.T) {
	for _, c := range []struct {
		name string
		l    Layout
	}{
		{"frame size is not a power of two", Layout{FrameSize: 1500, ObjHeader: 64, MbufSize: Size, Headroom: 128}},
		{"no frame at all", Layout{FrameSize: 0, ObjHeader: 64, MbufSize: Size, Headroom: 128}},
		{"an mbuf smaller than rte_mbuf", Layout{FrameSize: 2048, ObjHeader: 64, MbufSize: 64, Headroom: 128}},
		{"no room for a packet", Layout{FrameSize: 256, ObjHeader: 64, MbufSize: Size, Headroom: 128}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.l.Validate(); err == nil {
				t.Error("accepted")
			}
		})
	}
}

// The whole point of this package: every field lands where DPDK expects it, and
// reading it back gives what was written.
func TestFieldsRoundTrip(t *testing.T) {
	l := testLayout()
	b := make([]byte, 4*l.FrameSize)
	m := l.MbufOffset(l.FrameSize) // frame 1's mbuf

	SetBuffer(b, m, 0xdeadbeef00, 0xdeadbeef00, uint16(l.BufferLen()))
	if got := BufAddr(b, m); got != 0xdeadbeef00 {
		t.Errorf("BufAddr = %#x", got)
	}
	if got, want := BufLen(b, m), uint16(l.BufferLen()); got != want {
		t.Errorf("BufLen = %d, want %d", got, want)
	}

	PrepareTx(b, m, 0x1234, 128, 60)
	if got := DataOff(b, m); got != 128 {
		t.Errorf("DataOff = %d, want 128", got)
	}
	if got := DataLen(b, m); got != 60 {
		t.Errorf("DataLen = %d, want 60", got)
	}
	if got := PktLen(b, m); got != 60 {
		t.Errorf("PktLen = %d, want 60", got)
	}
	if got := NbSegs(b, m); got != 1 {
		t.Errorf("NbSegs = %d, want 1", got)
	}
	if got := Pool(b, m); got != 0x1234 {
		t.Errorf("Pool = %#x, want 0x1234", got)
	}
	// refcnt has to be 1, or the PMD will not free the mbuf when it is done
	// and the frame never comes back.
	if got := binary.NativeEndian.Uint16(b[m+offRefcnt:]); got != 1 {
		t.Errorf("refcnt = %d, want 1", got)
	}
	// next must be nil, or the PMD follows a stale pointer into whatever the
	// frame held last time.
	if got := binary.NativeEndian.Uint64(b[m+offNext:]); got != 0 {
		t.Errorf("next = %#x, want 0", got)
	}

	SetPool(b, m, 0x5678)
	if got := Pool(b, m); got != 0x5678 {
		t.Errorf("after SetPool, Pool = %#x", got)
	}
	SetOlFlags(b, m, RxIPChecksumGood|RxL4ChecksumGood)
	if got := OlFlags(b, m); got != RxIPChecksumGood|RxL4ChecksumGood {
		t.Errorf("OlFlags = %#x", got)
	}
	SetTxOffload(b, m, 18, 20)
	if got := TxOffload(b, m); got != 18|20<<7 {
		t.Errorf("TxOffload = %#x, want %#x", got, 18|20<<7)
	}
}

// Reset is what Fill runs before giving a frame back to the PMD, and it must
// clear everything the last packet left behind. A stale pkt_len or ol_flags is
// how a received packet ends up reported as the wrong length.
func TestResetClearsTheLastPacket(t *testing.T) {
	l := testLayout()
	b := make([]byte, l.FrameSize)
	m := l.MbufOffset(0)

	PrepareTx(b, m, 0x99, 200, 1400)
	SetOlFlags(b, m, 0xffff)
	SetTxOffload(b, m, 14, 20)

	Reset(b, m, uint16(l.Headroom))
	if got := DataOff(b, m); got != 128 {
		t.Errorf("DataOff = %d, want the headroom 128", got)
	}
	for _, c := range []struct {
		name string
		got  uint64
	}{
		{"ol_flags", OlFlags(b, m)},
		{"pkt_len", uint64(PktLen(b, m))},
		{"data_len", uint64(DataLen(b, m))},
		{"tx_offload", TxOffload(b, m)},
	} {
		if c.got != 0 {
			t.Errorf("%s survived Reset: %#x", c.name, c.got)
		}
	}
	if got := NbSegs(b, m); got != 1 {
		t.Errorf("NbSegs = %d, want 1", got)
	}
}

// Writing one frame's mbuf must not touch its neighbours. Every frame's fields
// are written by a different worker in the real thing.
func TestMbufsDoNotOverlap(t *testing.T) {
	l := testLayout()
	b := make([]byte, 3*l.FrameSize)
	mid := l.MbufOffset(l.FrameSize)

	SetBuffer(b, mid, ^uint64(0), ^uint64(0), 0xffff)
	PrepareTx(b, mid, ^uint64(0), 0xffff, 0xffffffff)
	SetOlFlags(b, mid, ^uint64(0))
	SetTxOffload(b, mid, 0x7f, 0x1ff)

	for i, v := range b {
		inMbuf := i >= mid && i < mid+Size
		if !inMbuf && v != 0 {
			t.Fatalf("byte %d outside the mbuf at %d was written", i, mid)
		}
	}
}

// The offsets are a copy of what the installed headers say. Nothing here can
// check that without DPDK, but the internal consistency can be checked: no
// field may overlap another, and none may run past the mbuf.
func TestOffsetsAreConsistent(t *testing.T) {
	sizes := map[string]int{
		"buf_addr": 8, "buf_iova": 8, "data_off": 2, "refcnt": 2, "nb_segs": 2,
		"port": 2, "ol_flags": 8, "pkt_len": 4, "data_len": 2, "vlan_tci": 2,
		"buf_len": 2, "pool": 8, "next": 8, "tx_offload": 8,
	}
	occupied := make(map[int]string)
	for name, off := range Offsets() {
		if name == "rearm_data" {
			continue // deliberately an alias for data_off's word
		}
		n, ok := sizes[name]
		if !ok {
			t.Fatalf("no size recorded for %s", name)
		}
		if off < 0 || off+n > Size {
			t.Errorf("%s at %d+%d runs past the %d-byte mbuf", name, off, n, Size)
			continue
		}
		for i := off; i < off+n; i++ {
			if other, clash := occupied[i]; clash {
				t.Errorf("%s overlaps %s at byte %d", name, other, i)
			}
			occupied[i] = name
		}
	}
	// rearm_data must cover exactly data_off, refcnt, nb_segs and port, since
	// Reset and PrepareTx write all four with one store.
	o := Offsets()
	if o["rearm_data"] != o["data_off"] || o["port"]+2 != o["rearm_data"]+8 {
		t.Errorf("rearm_data at %d does not cover data_off..port (%d..%d)",
			o["rearm_data"], o["data_off"], o["port"]+2)
	}
}

// The rearm word is one store standing in for four fields, so its packing has
// to match the individual offsets exactly.
func TestRearmPackingMatchesTheFields(t *testing.T) {
	b := make([]byte, Size)
	binary.NativeEndian.PutUint64(b[offRearm:], rearm(0x1111, 0x2222, 0x3333, 0x4444))
	for _, c := range []struct {
		name string
		off  int
		want uint16
	}{
		{"data_off", offDataOff, 0x1111},
		{"refcnt", offRefcnt, 0x2222},
		{"nb_segs", offNbSegs, 0x3333},
		{"port", offPort, 0x4444},
	} {
		if got := binary.NativeEndian.Uint16(b[c.off:]); got != c.want {
			t.Errorf("%s = %#x, want %#x", c.name, got, c.want)
		}
	}
}
