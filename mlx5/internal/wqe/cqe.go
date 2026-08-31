package wqe

import (
	"fmt"
	"math/bits"
	"unsafe"
)

// Byte offsets within a 64-byte completion queue entry.
const (
	cqeRXHashResult  = 24 // also flags and remote queue pair number
	cqeHDSIPExt      = 28 // checksum results
	cqeL4HdrTypeEtc  = 29 // parsed header types
	cqeVLANInfo      = 30 //
	cqeByteCount     = 44 // bytes received
	cqeTimestamp     = 48 //
	cqeVendorSyndrom = 54 // error entries only
	cqeSyndrome      = 55 // error entries only
	cqeSWQEOpcodeQPN = 56 // error entries only
	cqeHeader        = 60 // wqe_counter(16) signature(8) op_own(8)
)

// CQ is a completion queue buffer, shared with the NIC.
//
// The hardware does not tell software how many entries are ready. Instead each
// entry carries an owner bit which the hardware flips on every lap of the
// queue, so an entry belongs to software when its owner bit differs from the
// bit of the previous lap. That is why the consumer index here is a free
// running counter rather than an index into the buffer: its bit above the
// queue size is the lap parity.
type CQ struct {
	buf     []byte
	entries uint32
	mask    uint32 // entries-1
	log2    uint32
}

// NewCQ wraps a completion queue buffer holding entries of cqeSize bytes.
//
// Only 64-byte entries are supported. A device can be asked for 128-byte
// entries, where the part decoded here sits in the second half of each one;
// this package rejects that outright rather than silently decoding padding.
func NewCQ(buf []byte, cqeSize int) (CQ, error) {
	if cqeSize != CQESize {
		return CQ{}, fmt.Errorf("wqe: completion queue entry size %d, want %d", cqeSize, CQESize)
	}
	if len(buf)%CQESize != 0 || len(buf) == 0 {
		return CQ{}, fmt.Errorf("wqe: completion queue buffer of %d bytes is not a whole number of %d-byte entries", len(buf), CQESize)
	}
	n := uint32(len(buf) / CQESize)
	if n&(n-1) != 0 {
		return CQ{}, fmt.Errorf("wqe: completion queue of %d entries is not a power of two", n)
	}
	return CQ{buf: buf, entries: n, mask: n - 1, log2: uint32(bits.TrailingZeros32(n))}, nil
}

// Entries is how many completions the queue holds.
func (c CQ) Entries() uint32 { return c.entries }

// Bytes returns the underlying buffer.
func (c CQ) Bytes() []byte { return c.buf }

// Entry returns the 64 bytes of the completion at consumer index ci.
func (c CQ) Entry(ci uint32) []byte {
	off := (ci & c.mask) * CQESize
	return c.buf[off : off+CQESize : off+CQESize]
}

// HeaderPtr is the address of the last four bytes of the completion at ci: the
// work queue entry counter, the signature and the opcode-and-owner byte. This
// is the word to load, with a barrier, before anything else in the entry is
// trusted.
func (c CQ) HeaderPtr(ci uint32) unsafe.Pointer {
	off := (ci&c.mask)*CQESize + cqeHeader
	return unsafe.Pointer(&c.buf[off])
}

// ExpectedOwner is the owner bit a completion at ci carries once the hardware
// has written it. It is the parity of the lap the consumer index is on.
func (c CQ) ExpectedOwner(ci uint32) uint32 { return (ci >> c.log2) & 1 }

// Owned reports whether the completion at ci has been written by the hardware,
// given the header word read from it. An entry left over from an earlier lap
// still has the previous parity, and one the hardware has never touched reads
// as the invalid opcode.
func (c CQ) Owned(ci uint32, header uint32) bool {
	return Owner(header) == c.ExpectedOwner(ci) && Opcode(header) != CQEInvalid
}

// The header word is the big-endian dword at the end of a completion, so the
// work queue entry counter lands in its top half and the opcode-and-owner byte
// in its bottom.

// Owner is the lap parity bit the hardware wrote.
func Owner(header uint32) uint32 { return header & 1 }

// Format says whether the completion stands for itself or for several
// compressed ones. Anything but CQEFormatNoData needs decoding this package
// does not yet do.
func Format(header uint32) uint8 { return uint8(header>>2) & 3 }

// Opcode says what kind of completion this is: a send finished, a packet
// arrived, or something failed.
func Opcode(header uint32) uint8 { return uint8(header>>4) & 0xf }

// WQECounter is the work queue entry the completion refers to, counted in
// blocks for a send queue and in entries for a receive queue. It wraps at
// 65536, so it identifies a slot, not a position in time.
func WQECounter(header uint32) uint16 { return uint16(header >> 16) }

// ByteCount is how many bytes were received, for a receive completion.
func ByteCount(entry []byte) uint32 { return getBEU32(entry[cqeByteCount:]) }

// ChecksumFlags returns the byte holding the CQEL2OK, CQEL3OK and CQEL4OK
// results of the NIC's parsing.
func ChecksumFlags(entry []byte) uint8 { return entry[cqeHDSIPExt] }

// L3HeaderType says what the NIC made of the packet's network header:
// CQEL3HdrTypeIPv4, CQEL3HdrTypeIPv6 or CQEL3HdrTypeNone.
func L3HeaderType(entry []byte) uint8 { return (entry[cqeL4HdrTypeEtc] >> 2) & 3 }

// VLANInfo is the tag control information of a stripped VLAN tag. It is only
// meaningful when the receive queue was asked to strip tags, which packetio
// does not do.
func VLANInfo(entry []byte) uint16 { return getBEU16(entry[cqeVLANInfo:]) }

// RXHashResult is the receive-side scaling hash the NIC computed.
func RXHashResult(entry []byte) uint32 { return getBEU32(entry[cqeRXHashResult:]) }

// Timestamp is the NIC's clock reading for the packet.
func Timestamp(entry []byte) uint64 { return getBEU64(entry[cqeTimestamp:]) }

// ErrorInfo describes a failed completion.
type ErrorInfo struct {
	// Syndrome says what went wrong: see the CQESyndrome constants.
	Syndrome uint8

	// VendorSyndrome is the device-specific detail behind it.
	VendorSyndrome uint8

	// QPN is the queue pair the failure belongs to, and WQECounter the entry
	// that failed.
	QPN        uint32
	WQECounter uint16
}

// Error decodes a failed completion. Only call it for CQEReqErr and
// CQERespErr, whose layout differs from a good completion's.
func Error(entry []byte) ErrorInfo {
	return ErrorInfo{
		Syndrome:       entry[cqeSyndrome],
		VendorSyndrome: entry[cqeVendorSyndrom],
		QPN:            getBEU32(entry[cqeSWQEOpcodeQPN:]) & 0xffffff,
		WQECounter:     getBEU16(entry[cqeHeader:]),
	}
}

// Completion syndromes, the reason a work queue entry failed.
const (
	SyndromeLocalLength    = 0x01 // the entry described more or fewer bytes than the packet had
	SyndromeLocalQPOp      = 0x02 // the entry itself was malformed
	SyndromeLocalProt      = 0x04 // the address or memory key was not one this queue may use
	SyndromeWRFlush        = 0x05 // the queue went to the error state and this entry was flushed
	SyndromeMWBind         = 0x06 //
	SyndromeBadResp        = 0x10 //
	SyndromeLocalAccess    = 0x11 //
	SyndromeRemoteInvalReq = 0x12 //
	SyndromeRemoteAccess   = 0x13 //
	SyndromeRemoteOp       = 0x14 //
	SyndromeTransportRetry = 0x15 //
	SyndromeRNRRetry       = 0x16 //
	SyndromeRemoteAborted  = 0x22 //
	VendorSyndromeODPFault = 0x93 //
)

// SyndromeString names a syndrome for an error message.
func SyndromeString(s uint8) string {
	switch s {
	case SyndromeLocalLength:
		return "local length error"
	case SyndromeLocalQPOp:
		return "local queue pair operation error"
	case SyndromeLocalProt:
		return "local protection error"
	case SyndromeWRFlush:
		return "flushed because the queue is in the error state"
	case SyndromeMWBind:
		return "memory window bind error"
	case SyndromeBadResp:
		return "bad response"
	case SyndromeLocalAccess:
		return "local access error"
	case SyndromeRemoteInvalReq:
		return "remote invalid request"
	case SyndromeRemoteAccess:
		return "remote access error"
	case SyndromeRemoteOp:
		return "remote operation error"
	case SyndromeTransportRetry:
		return "transport retry counter exceeded"
	case SyndromeRNRRetry:
		return "receiver-not-ready retry counter exceeded"
	case SyndromeRemoteAborted:
		return "remote aborted"
	default:
		return fmt.Sprintf("syndrome 0x%02x", s)
	}
}

// OpcodeString names a completion opcode for an error message.
func OpcodeString(op uint8) string {
	switch op {
	case CQEReq:
		return "send completed"
	case CQERespSend:
		return "packet received"
	case CQEReqErr:
		return "send failed"
	case CQERespErr:
		return "receive failed"
	case CQEInvalid:
		return "invalid"
	case CQENoPacket:
		return "no packet"
	case CQEResizeCQ:
		return "queue resized"
	case CQESigErr:
		return "signature error"
	default:
		return fmt.Sprintf("opcode %d", op)
	}
}
