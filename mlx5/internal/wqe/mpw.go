package wqe

import "fmt"

// MPWSender builds enhanced multi-packet SEND entries: one entry carrying
// several packets.
//
// The packets are copied into the entry rather than pointed at. That is the
// point of it: a packet the NIC has to fetch separately costs a round trip
// across the bus, and at small frame sizes those round trips, not bytes, are
// what the card runs out of. Copying costs the processor a little more and the
// card a great deal less.
//
// Two conditions have to hold, and the caller must have checked both: the
// device reports that it can do this at all, and it requires no inline
// Ethernet header. A device that insists on parsing an L2 header out of the
// entry cannot be given an entry that has no room for one.
type MPWSender struct {
	sq SQ

	qpn     uint32
	lkey    uint32
	csFlags uint8
	pointer bool

	maxPackets int
	maxLen     int
}

// MPWConfig describes the fixed parts of every entry an MPWSender writes.
type MPWConfig struct {
	// QPN is the number of the queue pair that owns the send queue.
	QPN uint32

	// CSFlags asks the NIC to compute checksums.
	CSFlags uint8

	// MaxLen is the longest packet that may be carried by copying. A longer
	// one has to go in an ordinary entry, because copying it would cost more
	// than the fetch it saves. It is meaningless in pointer mode, where no
	// packet is copied at all.
	MaxLen int

	// Pointer switches the sender from copying packets into the entry to
	// pointing at them: each packet becomes one sixteen-byte data segment
	// naming its address, and the NIC fetches the bytes itself.
	//
	// Which mode wins is a property of the card, and on a ConnectX-6 Dx it is
	// not close: the device drains pointer entries at three times the rate of
	// copied ones, whatever the doorbell pattern -- measured, not reasoned,
	// against DPDK's mlx5 driver doing the same thing on the same port. The
	// comment on MPWSender about copying sparing the card a fetch describes
	// the trade truthfully and still loses, because the card's entry
	// processing, not its fetch bandwidth, is what runs out first.
	Pointer bool

	// LKey is the memory key the pointer data segments carry. Only pointer
	// mode reads it.
	LKey uint32
}

// NewMPWSender prepares a multi-packet sender.
func NewMPWSender(sq SQ, cfg MPWConfig) (*MPWSender, error) {
	s := &MPWSender{
		sq:      sq,
		qpn:     cfg.QPN,
		lkey:    cfg.LKey,
		csFlags: cfg.CSFlags,
		pointer: cfg.Pointer,
		maxLen:  cfg.MaxLen,
	}
	if cfg.Pointer {
		// A packet is one data segment whatever its length, so the entry's
		// octoword budget is the only bound -- capped so that one full entry
		// still fits the send queue, which matters only for the very small
		// queues tests build.
		s.maxPackets = MaxDS - 2
		if max := int(sq.bbs) * WQEBB / Octoword; s.maxPackets > max-2 {
			s.maxPackets = max - 2
		}
		if s.maxPackets < 2 {
			return nil, fmt.Errorf("wqe: only %d packets fit an entry of a %d-block send queue; there is no point", s.maxPackets, sq.bbs)
		}
		return s, nil
	}
	if cfg.MaxLen <= 0 {
		return nil, fmt.Errorf("wqe: a longest multi-packet packet of %d bytes", cfg.MaxLen)
	}
	perPacket := mpwPacketOctowords(uint32(cfg.MaxLen))
	if 2+perPacket > MaxDS {
		return nil, fmt.Errorf("wqe: a packet of %d bytes does not fit a multi-packet entry", cfg.MaxLen)
	}
	// How many of the largest packets fit in one entry, which is what the
	// caller must not exceed in a single call.
	s.maxPackets = int((MaxDS - 2) / perPacket)
	if s.maxPackets < 2 {
		return nil, fmt.Errorf("wqe: only %d packets of %d bytes fit one entry; there is no point", s.maxPackets, cfg.MaxLen)
	}
	if max := int(sq.bbs) * WQEBB / Octoword; 2+perPacket > uint32(max) {
		return nil, fmt.Errorf("wqe: an entry does not fit a send queue of %d blocks", sq.bbs)
	}
	return s, nil
}

// MaxPackets is the most packets one entry can carry, at the longest packet
// length configured.
func (s *MPWSender) MaxPackets() int { return s.maxPackets }

// MaxLen is the longest packet this sender will carry.
func (s *MPWSender) MaxLen() int { return s.maxLen }

// Fits reports how many of the given frames can go in one entry: as many as
// fit the entry's octoword budget, stopping at the first one too long to carry
// this way.
func (s *MPWSender) Fits(frames [][]byte) int {
	if s.pointer {
		// One octoword each, so the count is the only question -- but an
		// empty packet is still refused: a data segment describing no bytes
		// is a length error, exactly as for an ordinary entry.
		n := len(frames)
		if n > s.maxPackets {
			n = s.maxPackets
		}
		for i := 0; i < n; i++ {
			if len(frames[i]) == 0 {
				return i
			}
		}
		return n
	}
	ds := uint32(2)
	for i, f := range frames {
		if len(f) == 0 || len(f) > s.maxLen {
			return i
		}
		need := mpwPacketOctowords(uint32(len(f)))
		if ds+need > MaxDS {
			return i
		}
		ds += need
	}
	return len(frames)
}

// Put writes one entry at block index idx carrying every frame given, and
// returns how many blocks it occupies.
//
// The caller must have used Fits to choose the frames: Put does not check that
// they fit, any more than the single-packet builder checks that the queue has
// room.
//
// signaled asks for a completion, and only the last entry of a batch needs one.
func (s *MPWSender) Put(idx uint16, frames [][]byte, signaled bool) uint32 {
	off := s.sq.offset(uint32(idx))

	ds := uint32(2)
	for _, f := range frames {
		ds += mpwPacketOctowords(uint32(len(f)))
	}

	// Control segment, as for an ordinary send but with the multi-packet
	// opcode.
	s.sq.putU32BE(off+ctrlOpmodIdxOpcode, uint32(idx)<<8|OpcodeEnhancedMPSW)
	s.sq.putU32BE(off+ctrlQPNDS, s.qpn<<8|ds)
	var fmce uint32
	if signaled {
		fmce = CtrlCQUpdate
	}
	s.sq.putU32BE(off+ctrlSignature, fmce)
	s.sq.putU32BE(off+ctrlImm, 0)

	// The compact Ethernet segment: the same fields as an ordinary one up to
	// the inline header length, which is zero because the packets that follow
	// carry their own headers.
	s.sq.putU32BE(off+ethRsvd0, 0)
	s.sq.putU32BE(off+ethCSFlags, uint32(s.csFlags)<<24)
	s.sq.putU32BE(off+ethRsvd2, 0)
	s.sq.putU32BE(off+ethInlineHdrSz, 0) // and the two bytes of inline header

	// Then the packets, each behind its length, each padded to an octoword.
	at := off + CtrlSegSize + MPWEthSegSize
	for _, f := range frames {
		n := uint32(len(f))
		s.sq.putU32BE(at, n|MPWInlineFlag)
		s.sq.putBytes(at+4, f)
		size := mpwPacketOctowords(n) * Octoword
		if pad := size - 4 - n; pad > 0 {
			s.sq.zero(at+4+n, pad)
		}
		at += size
	}

	return (ds*Octoword + WQEBB - 1) / WQEBB
}

// Pointer reports whether this sender writes pointer entries.
func (s *MPWSender) Pointer() bool { return s.pointer }

// PutPointers writes one entry at block index idx pointing at every frame
// given, and returns how many blocks it occupies. addrs[i] is the device
// address of frames[i]; the two must be the same length, chosen with Fits.
//
// This is the entry DPDK's mlx5 driver calls eMPW without inlining, and it is
// how that driver reaches three times this backend's old single-queue rate on
// the same card: sixteen bytes written per packet, nothing copied, and up to
// 58 packets behind one control segment.
func (s *MPWSender) PutPointers(idx uint16, frames [][]byte, addrs []uint64, signaled bool) uint32 {
	off := s.sq.offset(uint32(idx))
	ds := uint32(2 + len(frames))

	// Control segment, as for the copying form.
	s.sq.putU32BE(off+ctrlOpmodIdxOpcode, uint32(idx)<<8|OpcodeEnhancedMPSW)
	s.sq.putU32BE(off+ctrlQPNDS, s.qpn<<8|ds)
	var fmce uint32
	if signaled {
		fmce = CtrlCQUpdate
	}
	s.sq.putU32BE(off+ctrlSignature, fmce)
	s.sq.putU32BE(off+ctrlImm, 0)

	// The compact Ethernet segment: no inline header, the packets carry their
	// own.
	s.sq.putU32BE(off+ethRsvd0, 0)
	s.sq.putU32BE(off+ethCSFlags, uint32(s.csFlags)<<24)
	s.sq.putU32BE(off+ethRsvd2, 0)
	s.sq.putU32BE(off+ethInlineHdrSz, 0)

	// Then one data segment per packet: its length with the top bit clear --
	// which is what says "fetch from this address" rather than "the bytes
	// follow" -- the memory key, and the address.
	at := off + CtrlSegSize + MPWEthSegSize
	for i, f := range frames {
		s.sq.putU32BE(at+dsegByteCount, uint32(len(f)))
		s.sq.putU32BE(at+dsegLKey, s.lkey)
		s.sq.putU64BE(at+dsegAddr, addrs[i])
		at += Octoword
	}

	return (ds*Octoword + WQEBB - 1) / WQEBB
}

// FirstOctoword returns the first eight bytes of the entry at block index idx,
// which is what a doorbell writes to the device.
func (s *MPWSender) FirstOctoword(idx uint16) uint64 {
	off := s.sq.offset(uint32(idx))
	_ = s.sq.buf[off+7]
	return nativeU64(s.sq.buf[off:])
}

// mpwPacketOctowords is how much of an entry one copied packet takes: its
// four-byte length and its bytes, rounded up to a whole octoword.
func mpwPacketOctowords(length uint32) uint32 {
	return (4 + length + Octoword - 1) / Octoword
}
