// Package mocknic is a software model of the part of an mlx5 NIC that a send
// or receive queue talks to.
//
// It reads the same doorbell records, parses the same work queue entries and
// writes the same completions as the hardware, over the same memory, so the
// ring code under test cannot tell the difference. That is the point: the rules
// about who owns a frame, when an entry may be reused and when a completion may
// be believed are exactly the rules that are impossible to test against real
// hardware, because breaking them produces silently corrupted packets rather
// than an error.
//
// The model is strict. Anything the hardware would be entitled to treat as
// undefined is recorded as a violation rather than tolerated.
package mocknic

import (
	"encoding/binary"
	"fmt"

	"github.com/atoonk/packetio/mlx5/internal/wqe"
)

// Offsets into a work queue entry the model reads directly.
const (
	ctrlFMCESE     = 11                   // fence, completion and solicited-event bits
	ethInlineHdrSz = wqe.CtrlSegSize + 12 // how many inline bytes follow
	ethInlineHdr   = wqe.CtrlSegSize + 14 // the inline bytes themselves
)

// Packet is one packet the model transmitted.
type Packet struct {
	// Bytes is the packet as it would go on the wire, reassembled from the
	// bytes inlined in the work queue entry and the bytes pointed at.
	Bytes []byte

	// Addr is the device address of the frame it came from, and Block the
	// index of the work queue entry that described it.
	Addr  uint64
	Block uint32

	// Inlined is how many of the bytes were carried in the entry itself.
	Inlined int
}

// TxConfig is the queue memory the model watches. It mirrors what a driver gets
// from mlx5dv_init_obj.
type TxConfig struct {
	SQ, CQ           []byte
	SQDbrec, CQDbrec *uint32
	UAR              *uint64
	Region           []byte
	RegionVA         uint64
	LKey, QPN        uint32
}

// TxNIC models the transmit half of a queue pair.
type TxNIC struct {
	cfg TxConfig
	cq  wqe.CQ

	blocks   uint32 // blocks in the send queue
	consumed uint32 // blocks the model has read
	done     uint32 // blocks it has completed
	cqPi     uint32 // completions written

	// FailAfter makes the model report a failure on the packet with this
	// one-based number, and then flush everything behind it the way hardware
	// does when a queue pair drops into the error state. Zero never fails.
	FailAfter int

	// Syndrome is the failure reason reported. Zero means a local protection
	// error, the usual consequence of a bad address or memory key.
	Syndrome uint8

	// BlocksPerPoll limits how much the model reads in one call, so a test can
	// leave work outstanding. Zero reads everything available; negative reads
	// nothing, holding the model still.
	BlocksPerPoll int

	// Discard keeps the model from recording the packets it transmits, so that
	// a test measuring the driver's allocations is not measuring the model's.
	// Everything is still parsed and checked.
	Discard bool

	scratch []byte

	failed bool
	stop   bool // end this poll after the entry just processed

	// Packets is everything transmitted, in order, unless Discard is set.
	Packets []Packet

	// sent counts everything transmitted whether or not it was recorded.
	sent int

	// Violations is everything the driver did that hardware would be entitled
	// to treat as undefined.
	Violations []string
}

// NewTx prepares a model over the given queue memory.
func NewTx(cfg TxConfig) (*TxNIC, error) {
	cq, err := wqe.NewCQ(cfg.CQ, wqe.CQESize)
	if err != nil {
		return nil, err
	}
	if len(cfg.SQ)%wqe.WQEBB != 0 || len(cfg.SQ) == 0 {
		return nil, fmt.Errorf("mocknic: send queue of %d bytes", len(cfg.SQ))
	}
	n := &TxNIC{cfg: cfg, cq: cq, blocks: uint32(len(cfg.SQ) / wqe.WQEBB)}
	// Hardware leaves an untouched completion queue reading as invalid, which
	// is what stops software believing the zeroes it starts with.
	for i := uint32(0); i < cq.Entries(); i++ {
		e := cq.Entry(i)
		e[63] = wqe.CQEInvalid << 4
	}
	return n, nil
}

// Poll does what the NIC does when a doorbell arrives: read the producer index,
// process the entries software has added, transmit their packets and write
// completions for the entries that asked for one.
func (n *TxNIC) Poll() {
	pi := n.producerIndex()
	if pi == n.consumed {
		return
	}

	// Software may not run further ahead than the queue holds; doing so would
	// overwrite entries the NIC has not read.
	if pi-n.consumed > n.blocks-(n.consumed-n.done) {
		n.violate("producer index %d runs %d blocks past the %d the queue has free",
			pi, pi-n.consumed, n.blocks-(n.consumed-n.done))
		return
	}

	if n.BlocksPerPoll < 0 {
		return // the model is held still on purpose
	}
	limit := pi
	if n.BlocksPerPoll > 0 && pi-n.consumed > uint32(n.BlocksPerPoll) {
		limit = n.consumed + uint32(n.BlocksPerPoll)
	}

	for n.consumed != limit {
		if n.stop {
			n.stop = false
			return
		}
		if n.failed {
			// A failed queue pair flushes what is left rather than sending it.
			n.flushOne()
			continue
		}
		n.processOne()
	}
}

// producerIndex reads the send queue's doorbell record and extends the 16-bit
// index the hardware sees back to the full counter the model tracks.
func (n *TxNIC) producerIndex() uint32 {
	var b [4]byte
	binary.NativeEndian.PutUint32(b[:], *n.cfg.SQDbrec)
	low := binary.BigEndian.Uint32(b[:])
	if low>>16 != 0 {
		n.violate("doorbell record holds %#x, which is not a 16-bit block index; is it byte-swapped?", low)
	}
	return n.consumed + uint32(uint16(low)-uint16(n.consumed))
}

func (n *TxNIC) processOne() {
	block := n.consumed
	off := (block & (n.blocks - 1)) * wqe.WQEBB
	e := n.cfg.SQ

	opcode := e[off+3]
	if opcode != wqe.OpcodeSend && opcode != wqe.OpcodeEnhancedMPSW {
		n.violate("block %d has opcode %#x, want SEND or enhanced multi-packet", block, opcode)
		n.consumed = n.done // give up rather than walk garbage
		return
	}
	idx := uint32(e[off+1])<<8 | uint32(e[off+2])
	if idx != block&0xffff {
		n.violate("block %d carries producer index %d", block, idx)
	}
	qpnDS := binary.BigEndian.Uint32(e[off+4:])
	if qpn := qpnDS >> 8; qpn != n.cfg.QPN {
		n.violate("block %d names queue pair %d, want %d", block, qpn, n.cfg.QPN)
	}
	ds := qpnDS & 0xff
	if ds == 0 || ds > wqe.MaxDS {
		n.violate("block %d has ds %d", block, ds)
		n.consumed = n.done
		return
	}
	blocks := (ds*wqe.Octoword + wqe.WQEBB - 1) / wqe.WQEBB

	if opcode == wqe.OpcodeEnhancedMPSW {
		n.processMultiPacket(block, off, ds, blocks)
		return
	}

	inlineLen := uint32(binary.BigEndian.Uint16(e[off+ethInlineHdrSz:]))
	ethOcts := (wqe.EthSegHeaderSize + inlineLen + wqe.Octoword - 1) / wqe.Octoword
	hasDseg := ds == 1+ethOcts+1
	if !hasDseg && ds != 1+ethOcts {
		n.violate("block %d has ds %d for a %d-byte inline header, want %d or %d",
			block, ds, inlineLen, 1+ethOcts, 1+ethOcts+1)
		n.consumed = n.done
		return
	}

	// Reassemble the packet the way the NIC would: the inline bytes first, then
	// whatever the data segment points at.
	pkt := Packet{Block: block, Inlined: int(inlineLen)}
	buf := append(n.scratch[:0], n.readSQ(off+ethInlineHdr, inlineLen)...)

	if hasDseg {
		dsegOff := off + wqe.CtrlSegSize + ethOcts*wqe.Octoword
		count := binary.BigEndian.Uint32(n.readSQ(dsegOff, 4))
		lkey := binary.BigEndian.Uint32(n.readSQ(dsegOff+4, 4))
		addr := binary.BigEndian.Uint64(n.readSQ(dsegOff+8, 8))
		pkt.Addr = addr

		// A data segment describing no bytes is not the same as no data
		// segment. A ConnectX-6 Dx answers one with a local length error and
		// puts the queue pair into the error state, so the model treats it the
		// same way rather than quietly transmitting a short packet.
		if count == 0 {
			n.violate("block %d has a data segment describing no bytes; a packet carried entirely inline must have no data segment at all", block)
			n.failWith(block, wqe.SyndromeLocalLength)
			return
		}
		if lkey != n.cfg.LKey {
			n.violate("block %d uses memory key %#x, want %#x", block, lkey, n.cfg.LKey)
		}
		lo, hi := n.cfg.RegionVA, n.cfg.RegionVA+uint64(len(n.cfg.Region))
		if addr < lo || addr+uint64(count) > hi {
			n.violate("block %d points at [%#x,%#x), outside the registered region [%#x,%#x)",
				block, addr, addr+uint64(count), lo, hi)
		} else {
			start := addr - n.cfg.RegionVA
			buf = append(buf, n.cfg.Region[start:start+uint64(count)]...)
		}
	}

	// A packet of no bytes at all is not a packet.
	if len(buf) == 0 {
		n.violate("block %d describes a packet of no bytes", block)
		n.failWith(block, wqe.SyndromeLocalLength)
		return
	}

	n.scratch = buf
	if !n.Discard {
		pkt.Bytes = append([]byte(nil), buf...)
		n.Packets = append(n.Packets, pkt)
	}
	n.sent++
	n.consumed += blocks

	if n.FailAfter > 0 && n.sent == n.FailAfter {
		// The entry that failed is reported straight away, whether or not it
		// asked for a completion. Everything behind it is flushed afterwards,
		// which on hardware is a separate wave of completions; the model ends
		// this poll here so a test can see the driver's behaviour in between.
		n.failed = true
		syndrome := n.Syndrome
		if syndrome == 0 {
			syndrome = wqe.SyndromeLocalProt
		}
		n.writeCompletion(block, wqe.CQEReqErr, syndrome)
		n.done = n.consumed
		n.stop = true
		return
	}

	if e[off+ctrlFMCESE]&wqe.CtrlCQUpdate != 0 {
		n.writeCompletion(block, wqe.CQEReq, 0)
	}
	n.done = n.consumed
}

// failWith puts the queue pair into the error state and reports the entry that
// did it, which is what the hardware does when it cannot make sense of one.
func (n *TxNIC) failWith(block uint32, syndrome uint8) {
	n.failed = true
	n.writeCompletion(block, wqe.CQEReqErr, syndrome)
	n.consumed += 1
	n.done = n.consumed
	n.stop = true
}

// processMultiPacket reads an entry carrying several packets, each copied in
// behind its own length.
func (n *TxNIC) processMultiPacket(block, off, ds, blocks uint32) {
	// The Ethernet segment of such an entry is the compact form, with no room
	// for an inline header and none expected: every packet carries its own.
	if hdr := binary.BigEndian.Uint16(n.readSQ(off+ethInlineHdrSz, 2)); hdr != 0 {
		n.violate("block %d is a multi-packet entry claiming %d inline header bytes", block, hdr)
		n.failWith(block, wqe.SyndromeLocalLength)
		return
	}

	at := off + wqe.CtrlSegSize + wqe.MPWEthSegSize
	end := off + ds*wqe.Octoword
	packets := 0
	for at < end {
		length := binary.BigEndian.Uint32(n.readSQ(at, 4))
		if length&wqe.MPWInlineFlag == 0 {
			// The top bit clear means a pointer data segment: sixteen bytes
			// naming a length, a memory key and an address, exactly as in an
			// ordinary entry. The hardware accepts inline and pointer
			// segments in one entry, so the model does too.
			if at+wqe.Octoword > end {
				n.violate("block %d packet %d is a pointer segment running past the end of its entry", block, packets)
				n.failWith(block, wqe.SyndromeLocalLength)
				return
			}
			count := length
			lkey := binary.BigEndian.Uint32(n.readSQ(at+4, 4))
			addr := binary.BigEndian.Uint64(n.readSQ(at+8, 8))
			if count == 0 {
				n.violate("block %d packet %d is a pointer segment describing no bytes", block, packets)
				n.failWith(block, wqe.SyndromeLocalLength)
				return
			}
			if lkey != n.cfg.LKey {
				n.violate("block %d packet %d uses memory key %#x, want %#x", block, packets, lkey, n.cfg.LKey)
			}
			lo, hi := n.cfg.RegionVA, n.cfg.RegionVA+uint64(len(n.cfg.Region))
			if addr < lo || addr+uint64(count) > hi {
				n.violate("block %d packet %d points at [%#x,%#x), outside the registered region [%#x,%#x)",
					block, packets, addr, addr+uint64(count), lo, hi)
			} else if !n.Discard {
				start := addr - n.cfg.RegionVA
				n.Packets = append(n.Packets, Packet{
					Bytes: append([]byte(nil), n.cfg.Region[start:start+uint64(count)]...),
					Block: block,
					Addr:  addr,
				})
			}
			n.sent++
			packets++
			at += wqe.Octoword

			if n.FailAfter > 0 && n.sent == n.FailAfter {
				n.consumed += blocks
				n.failed = true
				syndrome := n.Syndrome
				if syndrome == 0 {
					syndrome = wqe.SyndromeLocalProt
				}
				n.writeCompletion(block, wqe.CQEReqErr, syndrome)
				n.done = n.consumed
				n.stop = true
				return
			}
			continue
		}
		size := length &^ wqe.MPWInlineFlag
		if size == 0 {
			n.violate("block %d packet %d describes no bytes", block, packets)
			n.failWith(block, wqe.SyndromeLocalLength)
			return
		}
		if at+4+size > end {
			n.violate("block %d packet %d runs %d bytes past the end of its entry",
				block, packets, at+4+size-end)
			n.failWith(block, wqe.SyndromeLocalLength)
			return
		}

		buf := append(n.scratch[:0], n.readSQ(at+4, size)...)
		n.scratch = buf
		if !n.Discard {
			n.Packets = append(n.Packets, Packet{
				Bytes:   append([]byte(nil), buf...),
				Block:   block,
				Inlined: int(size),
			})
		}
		n.sent++
		packets++

		at += (4 + size + wqe.Octoword - 1) / wqe.Octoword * wqe.Octoword

		if n.FailAfter > 0 && n.sent == n.FailAfter {
			n.consumed += blocks
			n.failed = true
			syndrome := n.Syndrome
			if syndrome == 0 {
				syndrome = wqe.SyndromeLocalProt
			}
			n.writeCompletion(block, wqe.CQEReqErr, syndrome)
			n.done = n.consumed
			n.stop = true
			return
		}
	}
	if packets == 0 {
		n.violate("block %d is a multi-packet entry carrying nothing", block)
		n.failWith(block, wqe.SyndromeLocalLength)
		return
	}

	n.consumed += blocks
	// One entry, one completion: what makes this worth doing is that the
	// bookkeeping for several packets rides on a single report.
	if n.cfg.SQ[off+ctrlFMCESE]&wqe.CtrlCQUpdate != 0 {
		n.writeCompletion(block, wqe.CQEReq, 0)
	}
	n.done = n.consumed
}

// flushOne discards one entry from a failed queue pair and reports it, which is
// how the frames of a queue that has died find their way back to software.
func (n *TxNIC) flushOne() {
	block := n.consumed
	off := (block & (n.blocks - 1)) * wqe.WQEBB
	ds := binary.BigEndian.Uint32(n.cfg.SQ[off+4:]) & 0xff
	if ds == 0 || ds > wqe.MaxDS {
		ds = 4
	}
	blocks := (ds*wqe.Octoword + wqe.WQEBB - 1) / wqe.WQEBB
	n.consumed += blocks
	n.done = n.consumed
	n.writeCompletion(block, wqe.CQEReqErr, wqe.SyndromeWRFlush)
}

// readSQ reads from the send queue, following the wrap the hardware follows.
func (n *TxNIC) readSQ(off, length uint32) []byte {
	size := uint32(len(n.cfg.SQ))
	off &= size - 1
	if off+length <= size {
		return n.cfg.SQ[off : off+length]
	}
	b := make([]byte, 0, length)
	b = append(b, n.cfg.SQ[off:]...)
	return append(b, n.cfg.SQ[:length-(size-off)]...)
}

// writeCompletion posts one completion, refusing to overwrite one software has
// not released.
func (n *TxNIC) writeCompletion(block uint32, opcode, syndrome uint8) {
	if n.cqPi-n.consumerIndex() >= n.cq.Entries() {
		n.violate("completion queue overrun: software has released only up to %d of %d written",
			n.consumerIndex(), n.cqPi)
		return
	}
	e := n.cq.Entry(n.cqPi)
	for i := range e {
		e[i] = 0
	}
	if opcode == wqe.CQEReqErr {
		e[55] = syndrome
		binary.BigEndian.PutUint32(e[56:], n.cfg.QPN)
	}
	binary.BigEndian.PutUint16(e[60:], uint16(block))
	e[63] = opcode<<4 | uint8(n.cq.ExpectedOwner(n.cqPi))
	n.cqPi++
}

// consumerIndex reads how far software says it has read the completion queue.
func (n *TxNIC) consumerIndex() uint32 {
	var b [4]byte
	binary.NativeEndian.PutUint32(b[:], *n.cfg.CQDbrec)
	low := binary.BigEndian.Uint32(b[:]) & 0xffffff
	// Extend the 24-bit index software published back to the full counter.
	return n.cqPi - ((n.cqPi - low) & 0xffffff)
}

// Sent is how many packets the model has transmitted, recorded or not.
func (n *TxNIC) Sent() int { return n.sent }

// CQBytes is the completion queue buffer, for a test that wants to plant an
// entry the hardware would never write.
func (n *TxNIC) CQBytes() []byte { return n.cfg.CQ }

// Failed reports whether the model has put the queue pair into the error state.
func (n *TxNIC) Failed() bool { return n.failed }

// Outstanding is how many blocks software has published that the model has not
// yet read, which a test can use to leave work in flight deliberately.
func (n *TxNIC) Outstanding() uint32 { return n.producerIndex() - n.consumed }

func (n *TxNIC) violate(format string, args ...any) {
	n.Violations = append(n.Violations, fmt.Sprintf(format, args...))
}

// RxConfig is the receive queue memory the model watches.
type RxConfig struct {
	RQ, CQ           []byte
	RQDbrec, CQDbrec *uint32
	Region           []byte
	RegionVA         uint64
	LKey             uint32
}

// RxNIC models the receive half of a queue: it takes the buffers software has
// posted and writes packets into them, exactly as far as the buffer says it
// may.
type RxNIC struct {
	cfg RxConfig
	cq  wqe.CQ

	entries uint32
	mask    uint32
	ci      uint32 // buffers consumed
	cqPi    uint32 // completions written

	// FailEvery makes every nth packet arrive as a failure rather than a
	// packet, which is how a truncated or corrupted frame is reported. Zero
	// never fails.
	FailEvery int
	delivered int

	// TicksPerPacket is how far the model's clock advances between
	// completions. Zero leaves it at one tick, so time still moves: a
	// stationary clock is not something hardware does, and a test that
	// depended on one would be testing the model, not the ring.
	TicksPerPacket uint64
	clock          uint64

	Violations []string
}

const rxStride = 16

// NewRx prepares a receive model.
func NewRx(cfg RxConfig) (*RxNIC, error) {
	cq, err := wqe.NewCQ(cfg.CQ, wqe.CQESize)
	if err != nil {
		return nil, err
	}
	if len(cfg.RQ)%rxStride != 0 || len(cfg.RQ) == 0 {
		return nil, fmt.Errorf("mocknic: a receive queue of %d bytes", len(cfg.RQ))
	}
	n := &RxNIC{cfg: cfg, cq: cq, entries: uint32(len(cfg.RQ) / rxStride)}
	n.mask = n.entries - 1
	for i := uint32(0); i < cq.Entries(); i++ {
		cq.Entry(i)[63] = wqe.CQEInvalid << 4
	}
	return n, nil
}

// Deliver writes packets into the buffers software has posted, one per buffer,
// and reports each with a completion. It stops when it runs out of packets or
// of buffers, and returns how many it delivered.
func (n *RxNIC) Deliver(packets [][]byte) int {
	posted := n.postedCount()
	sent := 0
	for _, p := range packets {
		if uint32(sent) >= posted {
			break // no buffer to put it in; the packet is lost, as on a real port
		}
		slot := n.ci & n.mask
		off := slot * rxStride
		capacity := binary.BigEndian.Uint32(n.cfg.RQ[off:])
		lkey := binary.BigEndian.Uint32(n.cfg.RQ[off+4:])
		addr := binary.BigEndian.Uint64(n.cfg.RQ[off+8:])

		if lkey != n.cfg.LKey {
			n.violate("buffer %d uses memory key %#x, want %#x", slot, lkey, n.cfg.LKey)
			return sent
		}
		lo, hi := n.cfg.RegionVA, n.cfg.RegionVA+uint64(len(n.cfg.Region))
		if addr < lo || addr+uint64(capacity) > hi {
			n.violate("buffer %d is [%#x,%#x), outside the registered region [%#x,%#x)",
				slot, addr, addr+uint64(capacity), lo, hi)
			return sent
		}
		if uint32(len(p)) > capacity {
			n.violate("a packet of %d bytes does not fit a buffer of %d", len(p), capacity)
			return sent
		}

		copy(n.cfg.Region[addr-n.cfg.RegionVA:], p)
		n.delivered++
		n.ci++
		sent++

		if n.FailEvery > 0 && n.delivered%n.FailEvery == 0 {
			n.writeCompletion(slot, wqe.CQERespErr, 0)
		} else {
			n.writeCompletion(slot, wqe.CQERespSend, uint32(len(p)))
		}
	}
	return sent
}

// postedCount reads the receive doorbell record and works out how many buffers
// are available that the model has not already used.
func (n *RxNIC) postedCount() uint32 {
	var b [4]byte
	binary.NativeEndian.PutUint32(b[:], *n.cfg.RQDbrec)
	low := binary.BigEndian.Uint32(b[:])
	if low>>16 != 0 {
		n.violate("the receive doorbell record holds %#x, which is not a 16-bit index; is it byte-swapped?", low)
	}
	pi := n.ci + uint32(uint16(low)-uint16(n.ci))
	if pi-n.ci > n.entries {
		n.violate("software says %d buffers are posted, more than the %d the queue holds", pi-n.ci, n.entries)
		return 0
	}
	return pi - n.ci
}

func (n *RxNIC) writeCompletion(slot uint32, opcode uint8, length uint32) {
	if n.cqPi-n.cqConsumerIndex() >= n.cq.Entries() {
		n.violate("completion queue overrun: software has released only up to %d of %d written",
			n.cqConsumerIndex(), n.cqPi)
		return
	}
	e := n.cq.Entry(n.cqPi)
	for i := range e {
		e[i] = 0
	}
	binary.BigEndian.PutUint32(e[44:], length)
	// A real device stamps every completion from a free-running clock. The
	// model advances one so that a test reading timestamps sees time pass
	// rather than the zeroes the wipe above would otherwise leave -- a
	// timestamp test against a constant zero passes while proving nothing.
	if n.TicksPerPacket == 0 {
		n.TicksPerPacket = 1
	}
	n.clock += n.TicksPerPacket
	binary.BigEndian.PutUint64(e[48:], n.clock)
	e[28] = wqe.CQEL2OK | wqe.CQEL3OK | wqe.CQEL4OK
	binary.BigEndian.PutUint16(e[60:], uint16(slot))
	e[63] = opcode<<4 | uint8(n.cq.ExpectedOwner(n.cqPi))
	n.cqPi++
}

func (n *RxNIC) cqConsumerIndex() uint32 {
	var b [4]byte
	binary.NativeEndian.PutUint32(b[:], *n.cfg.CQDbrec)
	low := binary.BigEndian.Uint32(b[:]) & 0xffffff
	return n.cqPi - ((n.cqPi - low) & 0xffffff)
}

// CQBytes is the completion queue buffer, for a test that wants to plant an
// entry the hardware would never write.
func (n *RxNIC) CQBytes() []byte { return n.cfg.CQ }

func (n *RxNIC) violate(format string, args ...any) {
	n.Violations = append(n.Violations, fmt.Sprintf(format, args...))
}
