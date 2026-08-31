package wqe

import (
	"testing"
	"unsafe"
)

func newTestCQ(t *testing.T, entries int) CQ {
	t.Helper()
	cq, err := NewCQ(make([]byte, entries*CQESize), CQESize)
	if err != nil {
		t.Fatalf("NewCQ: %v", err)
	}
	return cq
}

// header assembles the last four bytes of a completion the way the hardware
// does, so the decoders are tested against a value built independently of them.
func header(wqeCounter uint16, opcode, format, owner uint8) uint32 {
	opOwn := opcode<<4 | format<<2 | owner
	return uint32(wqeCounter)<<16 | uint32(opOwn)
}

func writeHeader(entry []byte, wqeCounter uint16, opcode, format, owner uint8) {
	beU32(entry[cqeHeader:], header(wqeCounter, opcode, format, owner))
}

func TestCQEHeaderDecoding(t *testing.T) {
	h := header(0xbeef, CQERespSend, CQEFormatNoData, 1)
	if got := WQECounter(h); got != 0xbeef {
		t.Errorf("WQECounter = %#x, want 0xbeef", got)
	}
	if got := Opcode(h); got != CQERespSend {
		t.Errorf("Opcode = %d, want %d", got, CQERespSend)
	}
	if got := Format(h); got != CQEFormatNoData {
		t.Errorf("Format = %d, want 0", got)
	}
	if got := Owner(h); got != 1 {
		t.Errorf("Owner = %d, want 1", got)
	}

	h = header(0, CQEInvalid, CQEFormatCompressed, 0)
	if Opcode(h) != CQEInvalid || Format(h) != CQEFormatCompressed || Owner(h) != 0 {
		t.Errorf("decoded %#x as opcode %d format %d owner %d", h, Opcode(h), Format(h), Owner(h))
	}
}

// A completion belongs to software when its owner bit matches the parity of the
// lap the consumer index is on. Getting this wrong means either missing every
// other lap or reading entries the hardware has not written, so it is worth
// walking several laps explicitly.
func TestCQEOwnershipAcrossLaps(t *testing.T) {
	const entries = 8
	cq := newTestCQ(t, entries)

	for ci := uint32(0); ci < 4*entries; ci++ {
		wantOwner := uint32(0)
		if (ci/entries)%2 == 1 {
			wantOwner = 1
		}
		if got := cq.ExpectedOwner(ci); got != wantOwner {
			t.Fatalf("ci %d: expected owner %d, want %d", ci, got, wantOwner)
		}

		// The hardware writes the entry with this lap's parity.
		writeHeader(cq.Entry(ci), uint16(ci), CQEReq, CQEFormatNoData, uint8(wantOwner))
		h := getBEU32(cq.Entry(ci)[cqeHeader:])
		if !cq.Owned(ci, h) {
			t.Fatalf("ci %d: freshly written entry not owned", ci)
		}
		// The same bytes read one lap later are stale and must not be taken.
		if cq.Owned(ci+entries, h) {
			t.Fatalf("ci %d: entry from the previous lap accepted", ci)
		}
	}
}

// An untouched queue is filled with the invalid opcode, and must never look
// owned however the parity falls.
func TestCQEInvalidEntryIsNeverOwned(t *testing.T) {
	const entries = 4
	cq := newTestCQ(t, entries)
	for ci := uint32(0); ci < 2*entries; ci++ {
		for _, owner := range []uint8{0, 1} {
			writeHeader(cq.Entry(ci), 0, CQEInvalid, CQEFormatNoData, owner)
			h := getBEU32(cq.Entry(ci)[cqeHeader:])
			if cq.Owned(ci, h) {
				t.Fatalf("ci %d owner %d: invalid entry accepted", ci, owner)
			}
		}
	}
}

func TestCQEEntryAliasesTheBuffer(t *testing.T) {
	const entries = 4
	cq := newTestCQ(t, entries)

	// Entries wrap, and the slice returned must be a window on the buffer, not
	// a copy: this is memory the NIC writes.
	cq.Entry(entries + 1)[0] = 0x5a
	if cq.Bytes()[CQESize+0] != 0x5a {
		t.Fatal("Entry did not alias the buffer at the wrapped index")
	}
	if got := len(cq.Entry(0)); got != CQESize {
		t.Fatalf("entry length %d, want %d", got, CQESize)
	}
	// Appending to an entry must not run into the next one.
	e := cq.Entry(0)
	if cap(e) != CQESize {
		t.Fatalf("entry capacity %d, want %d", cap(e), CQESize)
	}

	// The header pointer must address the last dword of the right entry.
	want := unsafe.Pointer(&cq.Bytes()[2*CQESize+cqeHeader])
	if got := cq.HeaderPtr(2); got != want {
		t.Fatalf("HeaderPtr = %p, want %p", got, want)
	}
	if got := cq.HeaderPtr(entries + 2); got != want {
		t.Fatalf("wrapped HeaderPtr = %p, want %p", got, want)
	}
}

func TestReceiveCompletionFields(t *testing.T) {
	cq := newTestCQ(t, 2)
	e := cq.Entry(0)

	beU32(e[cqeByteCount:], 1514)
	e[cqeHDSIPExt] = CQEL2OK | CQEL3OK | CQEL4OK
	e[cqeL4HdrTypeEtc] = CQEL3HdrTypeIPv4 << 2
	beU16(e[cqeVLANInfo:], 0x0123)
	beU32(e[cqeRXHashResult:], 0xcafebabe)
	beU64(e[cqeTimestamp:], 0x1122334455667788)
	writeHeader(e, 42, CQERespSend, CQEFormatNoData, 0)

	if got := ByteCount(e); got != 1514 {
		t.Errorf("ByteCount = %d, want 1514", got)
	}
	if got := ChecksumFlags(e); got&(CQEL3OK|CQEL4OK) != CQEL3OK|CQEL4OK {
		t.Errorf("ChecksumFlags = %#x, want the L3 and L4 bits set", got)
	}
	if got := L3HeaderType(e); got != CQEL3HdrTypeIPv4 {
		t.Errorf("L3HeaderType = %d, want %d", got, CQEL3HdrTypeIPv4)
	}
	if got := VLANInfo(e); got != 0x0123 {
		t.Errorf("VLANInfo = %#x, want 0x0123", got)
	}
	if got := RXHashResult(e); got != 0xcafebabe {
		t.Errorf("RXHashResult = %#x", got)
	}
	if got := Timestamp(e); got != 0x1122334455667788 {
		t.Errorf("Timestamp = %#x", got)
	}
	if got := WQECounter(getBEU32(e[cqeHeader:])); got != 42 {
		t.Errorf("WQECounter = %d, want 42", got)
	}
}

// A failed completion has a different layout from a good one, and its work
// queue entry counter may name an entry that was never signaled.
func TestErrorCompletionFields(t *testing.T) {
	cq := newTestCQ(t, 2)
	e := cq.Entry(1)

	e[cqeSyndrome] = SyndromeLocalProt
	e[cqeVendorSyndrom] = 0x42
	beU32(e[cqeSWQEOpcodeQPN:], 0x0a012345) // opcode in the top byte, then the queue pair
	writeHeader(e, 0x1000, CQEReqErr, CQEFormatNoData, 1)

	info := Error(e)
	if info.Syndrome != SyndromeLocalProt {
		t.Errorf("Syndrome = %#x, want %#x", info.Syndrome, SyndromeLocalProt)
	}
	if info.VendorSyndrome != 0x42 {
		t.Errorf("VendorSyndrome = %#x, want 0x42", info.VendorSyndrome)
	}
	if info.QPN != 0x012345 {
		t.Errorf("QPN = %#x, want 0x012345", info.QPN)
	}
	if info.WQECounter != 0x1000 {
		t.Errorf("WQECounter = %#x, want 0x1000", info.WQECounter)
	}
	if got := SyndromeString(SyndromeLocalProt); got == "" {
		t.Error("SyndromeString returned nothing")
	}
	if got := OpcodeString(CQEReqErr); got == "" {
		t.Error("OpcodeString returned nothing")
	}
}

func TestNewCQRejectsBadBuffers(t *testing.T) {
	if _, err := NewCQ(make([]byte, 128), 128); err == nil {
		t.Error("128-byte entries accepted")
	}
	if _, err := NewCQ(make([]byte, 100), CQESize); err == nil {
		t.Error("buffer that is not a whole number of entries accepted")
	}
	if _, err := NewCQ(nil, CQESize); err == nil {
		t.Error("empty buffer accepted")
	}
	if _, err := NewCQ(make([]byte, 3*CQESize), CQESize); err == nil {
		t.Error("queue whose entry count is not a power of two accepted")
	}
}

func TestCQEDecodingDoesNotAllocate(t *testing.T) {
	cq := newTestCQ(t, 64)
	writeHeader(cq.Entry(0), 7, CQERespSend, CQEFormatNoData, 0)
	allocs := testing.AllocsPerRun(200, func() {
		e := cq.Entry(0)
		h := getBEU32(e[cqeHeader:])
		_ = cq.Owned(0, h)
		_ = WQECounter(h)
		_ = ByteCount(e)
	})
	if allocs != 0 {
		t.Fatalf("decoding allocated %v times per run, want 0", allocs)
	}
}
