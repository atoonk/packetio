//go:build linux && cgo && dpdk && (amd64 || arm64)

package eal

import (
	"runtime"
	"testing"

	"github.com/atoonk/packetio/dpdk/internal/mbuf"
)

// internal/mbuf indexes a C struct by hand, from constants a person copied out
// of the headers. This is the test that says the copy is still right on the
// machine the code will run on: an offset that moved is not a compile error,
// it is a packet path reading somebody else's field.
func TestMbufLayoutMatchesTheInstalledHeaders(t *testing.T) {
	h := Headers()
	if h.MbufSize != mbuf.Size {
		t.Fatalf("sizeof(struct rte_mbuf) is %d here, but this backend is built for %d",
			h.MbufSize, mbuf.Size)
	}
	if h.Headroom != mbuf.Headroom {
		t.Errorf("RTE_PKTMBUF_HEADROOM is %d here, but this backend reserves %d",
			h.Headroom, mbuf.Headroom)
	}
	ours := mbuf.Offsets()
	for name, want := range h.Offsets {
		got, ok := ours[name]
		if !ok {
			t.Errorf("the headers have %s but this backend has no offset for it", name)
			continue
		}
		if got != want {
			t.Errorf("%s is at %d in the installed headers, but this backend uses %d",
				name, want, got)
		}
	}
	for name := range ours {
		if _, ok := h.Offsets[name]; !ok {
			t.Errorf("this backend uses %s, which is not checked against the headers", name)
		}
	}
}

// A wrong flag value is a NIC asked for the wrong work, which appears as a
// corrupt packet at the far end rather than as an error here.
func TestOffloadFlagsMatchTheInstalledHeaders(t *testing.T) {
	ours := map[string]uint64{
		"TxIPv4": mbuf.TxIPv4, "TxIPv6": mbuf.TxIPv6,
		"TxIPChecksum": mbuf.TxIPChecksum, "TxTCPChecksum": mbuf.TxTCPChecksum,
		"TxUDPChecksum": mbuf.TxUDPChecksum, "TxL4Mask": mbuf.TxL4Mask,
		"TxTCPSeg":         mbuf.TxTCPSeg,
		"RxIPChecksumGood": mbuf.RxIPChecksumGood, "RxL4ChecksumGood": mbuf.RxL4ChecksumGood,
		"RxIPChecksumBad": mbuf.RxIPChecksumBad, "RxL4ChecksumBad": mbuf.RxL4ChecksumBad,
	}
	h := Headers()
	for name, want := range h.Flags {
		got, ok := ours[name]
		if !ok {
			t.Errorf("no constant for %s", name)
			continue
		}
		if got != want {
			t.Errorf("%s is %#x in the installed headers, but this backend uses %#x",
				name, want, got)
		}
	}
	if len(ours) != len(h.Flags) {
		t.Errorf("%d constants here against %d checked", len(ours), len(h.Flags))
	}
}

// SetTxOffloadTSO packs four numbers into one word by hand. The compiler's own
// bitfield layout is the only authority on where each one goes.
func TestTxOffloadPackingMatchesTheBitfield(t *testing.T) {
	b := make([]byte, mbuf.Size)
	h := Headers()
	for _, c := range []struct {
		name string
		set  func()
	}{
		{"l2_len", func() { mbuf.SetTxOffloadTSO(b, 0, 0x7f, 0, 0, 0) }},
		{"l3_len", func() { mbuf.SetTxOffloadTSO(b, 0, 0, 0x1ff, 0, 0) }},
		{"l4_len", func() { mbuf.SetTxOffloadTSO(b, 0, 0, 0, 0xff, 0) }},
		{"tso_segsz", func() { mbuf.SetTxOffloadTSO(b, 0, 0, 0, 0, 0xffff) }},
	} {
		c.set()
		if got, want := mbuf.TxOffload(b, 0), h.TxOffloadMasks[c.name]; got != want {
			t.Errorf("%s packs to %#x, but the compiler puts it at %#x", c.name, got, want)
		}
	}
}

// The mempool object header is cache-line aligned, so it is 64 bytes on x86-64
// and 128 on aarch64. It was hardcoded to 64 once, which built and tested
// perfectly on x86 and then refused to open a device on Graviton with
// "this DPDK puts 128 bytes in front of a mempool object and 64 behind it".
// Anything that derives a frame address from it has to take it from here.
func TestMempoolObjectHeaderIsCacheLineAligned(t *testing.T) {
	h := Headers().ObjHeader
	switch runtime.GOARCH {
	case "amd64":
		if h != 64 {
			t.Errorf("object header is %d on amd64, want 64", h)
		}
	case "arm64":
		if h != 128 {
			t.Errorf("object header is %d on arm64, want 128", h)
		}
	}
	if h <= 0 || h&(h-1) != 0 {
		t.Errorf("object header %d is not a power of two", h)
	}
	if h < mbufObjHdrMin {
		t.Errorf("object header %d is too small to hold an rte_mempool_objhdr", h)
	}
}

// An objhdr is a list entry plus a pool pointer plus a cookie: 24 bytes on any
// 64-bit target, and the aligned-up header can never be smaller than that.
const mbufObjHdrMin = 24
