package wqe

import (
	"encoding/binary"
	"fmt"
)

// SQ is a send queue's work queue entry buffer: a power-of-two number of
// 64-byte work queue buffer blocks, shared with the NIC.
//
// The buffer is cyclic and the hardware reads it that way, so a work queue
// entry larger than one block may begin near the end and continue at the
// start. Every write here masks its offset, so wrapping needs no special case
// in the caller.
type SQ struct {
	buf  []byte
	mask uint32 // len(buf)-1, valid because len(buf) is a power of two
	bbs  uint32 // number of work queue buffer blocks
}

// NewSQ wraps a send queue buffer. The buffer must be a whole, power-of-two
// number of work queue buffer blocks, which is what the hardware always
// provides; anything else is a sign that the queue was misread rather than a
// case worth handling.
func NewSQ(buf []byte) (SQ, error) {
	n := len(buf)
	switch {
	case n == 0:
		return SQ{}, fmt.Errorf("wqe: empty send queue buffer")
	case n%WQEBB != 0:
		return SQ{}, fmt.Errorf("wqe: send queue buffer of %d bytes is not a whole number of %d-byte blocks", n, WQEBB)
	case n&(n-1) != 0:
		return SQ{}, fmt.Errorf("wqe: send queue buffer of %d bytes is not a power of two", n)
	}
	return SQ{buf: buf, mask: uint32(n - 1), bbs: uint32(n / WQEBB)}, nil
}

// BBs is how many work queue buffer blocks the queue holds. Producer indices
// are counted in these.
func (q SQ) BBs() uint32 { return q.bbs }

// Bytes returns the underlying buffer.
func (q SQ) Bytes() []byte { return q.buf }

// offset converts a block index to a byte offset, wrapping.
func (q SQ) offset(idx uint32) uint32 { return (idx * WQEBB) & q.mask }

// The buffer length is a multiple of 8 and offsets written here are naturally
// aligned, so an aligned 4- or 8-byte field can never straddle the wrap point.
// Only a byte copy can, and putBytes handles it.

func (q SQ) putU16BE(off uint32, v uint16) {
	binary.BigEndian.PutUint16(q.buf[off&q.mask:], v)
}

func (q SQ) putU32BE(off uint32, v uint32) {
	binary.BigEndian.PutUint32(q.buf[off&q.mask:], v)
}

func (q SQ) putU64BE(off uint32, v uint64) {
	binary.BigEndian.PutUint64(q.buf[off&q.mask:], v)
}

// putBytes copies src to the given byte offset, continuing at the start of the
// buffer if it runs off the end.
func (q SQ) putBytes(off uint32, src []byte) {
	off &= q.mask
	n := copy(q.buf[off:], src)
	if n < len(src) {
		copy(q.buf, src[n:])
	}
}

// segments returns how many octowords an Ethernet segment occupies when it
// carries inlineLen bytes of packet header.
//
// The segment is a 14-byte header followed by the inline bytes, padded up to a
// whole octoword: 16 bytes for no inline header, 32 for the usual 18-byte one,
// 48 for a QinQ header. rdma-core computes this as
// (offsetof(inline_hdr) + inlineLen) & ~0xf, which agrees for the 0 and 18 it
// is the only caller of, and is wrong for anything larger; DPDK rounds up, as
// here.
func ethSegOctowords(inlineLen uint32) uint32 {
	return (EthSegHeaderSize + inlineLen + Octoword - 1) / Octoword
}

// zero clears length bytes at the given offset, wrapping.
func (q SQ) zero(off, length uint32) {
	off &= q.mask
	n := uint32(len(q.buf)) - off
	if n > length {
		n = length
	}
	clear(q.buf[off : off+n])
	if n < length {
		clear(q.buf[:length-n])
	}
}
