//go:build linux && cgo && mlx5 && (amd64 || arm64)

package dv

import (
	"testing"

	"github.com/atoonk/packetio/mlx5/internal/wqe"
)

// The packet path writes work queue entries and reads completions by offset,
// with no C involved, so nothing but this test connects those offsets to the
// definitions the device's own library uses. A build against a future
// rdma-core that moved a field fails here rather than on the wire.
func TestLayoutMatchesTheHeaders(t *testing.T) {
	h := Headers()
	const ctrl = wqe.CtrlSegSize

	for _, c := range []struct {
		what      string
		got, want int
	}{
		{"control segment size", wqe.CtrlSegSize, h.CtrlSegSize},
		{"data segment size", wqe.DataSegSize, h.DataSegSize},
		{"completion entry size", wqe.CQESize, h.CQESize},
		{"work queue block", wqe.WQEBB, h.WQEBB},

		// The packet path sizes an Ethernet segment as this header plus the
		// inline bytes, rounded up to an octoword.
		{"Ethernet segment header", wqe.EthSegHeaderSize, h.EthInlineHdr},
		// With the usual 18-byte inline header that comes to 32 bytes, which
		// is what the library's own struct is sized for.
		{"Ethernet segment with an 18-byte inline header",
			wqe.EthSegHeaderSize + wqe.EthL2InlineHeaderSize, h.EthSegSize},

		// Offsets within a whole entry, which is how the packet path addresses
		// them: the control segment, then the Ethernet segment.
		{"completion request byte", 11, h.CtrlFMCESE},
		{"immediate data", 12, h.CtrlImm},
		{"checksum request", 20, ctrl + h.EthCSFlags},
		{"segmentation size", 22, ctrl + h.EthMSS},
		{"inline header length", 28, ctrl + h.EthInlineHdrSz},
		{"inline header", 30, ctrl + h.EthInlineHdr},

		{"completion checksum flags", 28, h.CQEChecksum},
		{"completion header types", 29, h.CQEHdrTypes},
		{"completion byte count", 44, h.CQEByteCount},
		{"completion timestamp", 48, h.CQETimestamp},
		{"completion entry counter", 60, h.CQEWQECounter},
		{"completion opcode and owner", 63, h.CQEOpOwn},
		{"completion hash", 24, h.CQEHash},
		{"completion vlan", 30, h.CQEVLAN},

		{"error syndrome", 55, h.ErrSyndrome},
		{"error vendor syndrome", 54, h.ErrVendorSyndrome},
		{"error queue pair", 56, h.ErrQPN},

		{"send opcode", wqe.OpcodeSend, h.OpcodeSend},
		{"completion request bit", wqe.CtrlCQUpdate, h.CtrlCQUpdate},
		{"send doorbell record", wqe.SndDBR, h.SndDBR},
		{"receive doorbell record", wqe.RcvDBR, h.RcvDBR},
		{"L3 checksum request", wqe.EthWQEL3Csum, h.EthWQEL3Csum},
		{"L4 checksum request", wqe.EthWQEL4Csum, h.EthWQEL4Csum},
		{"good send completion", wqe.CQEReq, h.CQEReq},
		{"good receive completion", wqe.CQERespSend, h.CQERespSend},
		{"failed send completion", wqe.CQEReqErr, h.CQEReqErr},
		{"failed receive completion", wqe.CQERespErr, h.CQERespErr},
		{"empty completion", wqe.CQEInvalid, h.CQEInvalid},
		{"local protection syndrome", wqe.SyndromeLocalProt, h.SyndromeProt},
		{"flushed syndrome", wqe.SyndromeWRFlush, h.SyndromeFlush},
	} {
		if c.got != c.want {
			t.Errorf("%s: this code uses %d, the headers say %d", c.what, c.got, c.want)
		}
	}

	if uint32(wqe.InlineSegFlag) != h.InlineSegFlag {
		t.Errorf("inline segment flag: this code uses %#x, the headers say %#x", uint32(wqe.InlineSegFlag), h.InlineSegFlag)
	}
}

// The numbers the packet path is built around, stated where they can fail.
func TestPacketPathAssumptions(t *testing.T) {
	if wqe.EthL2InlineHeaderSize != 18 {
		t.Errorf("the L2 inline header is %d bytes, want 14 for Ethernet plus 4 for a VLAN tag",
			wqe.EthL2InlineHeaderSize)
	}
	// A send with an 18-byte inline header and one data segment is exactly one
	// block, which is what makes a 64-byte packet cost one block.
	if got := wqe.CtrlSegSize + wqe.EthSegHeaderSize + wqe.EthL2InlineHeaderSize + wqe.DataSegSize; got != wqe.WQEBB {
		t.Errorf("a plain send entry is %d bytes, want %d", got, wqe.WQEBB)
	}
}
