package wqe

import "fmt"

// Byte offsets within a work queue entry. The control segment comes first, then
// the Ethernet segment, then any data segments.
const (
	ctrlOpmodIdxOpcode = 0  // opmod(8) index(16) opcode(8), big-endian
	ctrlQPNDS          = 4  // qpn(24) ds(8), big-endian
	ctrlSignature      = 8  // signature(8) stream(16) fence-completion-solicited(8)
	ctrlFMCESE         = 11 // the completion request lives here
	ctrlImm            = 12 //

	ethRsvd0       = CtrlSegSize + 0  //
	ethCSFlags     = CtrlSegSize + 4  // checksum offload request, then reserved and the segmentation size
	ethRsvd2       = CtrlSegSize + 8  //
	ethInlineHdrSz = CtrlSegSize + 12 // how many inline bytes follow
	ethInlineHdr   = CtrlSegSize + 14 // the inline bytes themselves

	dsegByteCount = 0 //
	dsegLKey      = 4 //
	dsegAddr      = 8 //
)

// Sender builds SEND work queue entries for one send queue.
//
// An entry is not a fixed size. It is a control segment, an Ethernet segment
// carrying however many bytes of the packet are inlined, and a data segment
// pointing at the rest -- and if there is no rest, no data segment at all. A
// data segment describing zero bytes is not the same as no data segment: the
// hardware rejects the entry with a length error.
//
// A Sender is used by the single goroutine that owns its queue.
type Sender struct {
	sq SQ

	qpn       uint32
	lkey      uint32
	csFlags   uint8
	inlineLen uint32

	maxDS  uint32
	maxBBs uint32
}

// SenderConfig describes the fixed parts of every entry a Sender writes.
type SenderConfig struct {
	// QPN is the number of the queue pair that owns the send queue.
	QPN uint32

	// LKey is the local key of the memory region the packet frames live in.
	LKey uint32

	// InlineLen is how many bytes of each packet to copy into the entry rather
	// than point at. It must be at least what the device's inline mode
	// requires: zero when the device asks for none, 18 for L2 mode, more for
	// L2 mode with a QinQ header.
	//
	// Inlining more is allowed and makes the entry larger. Inlining the whole
	// packet spares the NIC a second read of memory to fetch it, at the cost of
	// a longer entry for it to fetch instead.
	InlineLen int

	// CSFlags asks the NIC to compute checksums: EthWQEL3Csum, EthWQEL4Csum,
	// or both. Zero leaves whatever the packet already contains.
	CSFlags uint8
}

// NewSender prepares a Sender. It fails when the configuration would produce an
// entry the hardware cannot describe or the queue cannot hold.
func NewSender(sq SQ, cfg SenderConfig) (*Sender, error) {
	if cfg.InlineLen < 0 {
		return nil, fmt.Errorf("wqe: negative inline length %d", cfg.InlineLen)
	}
	s := &Sender{
		sq:        sq,
		qpn:       cfg.QPN,
		lkey:      cfg.LKey,
		csFlags:   cfg.CSFlags,
		inlineLen: uint32(cfg.InlineLen),
	}

	// The largest an entry can be: everything inlined and a data segment as
	// well. Reserving for that is what keeps a queue from being overrun by a
	// packet that turns out to need more room than the one before it.
	s.maxDS = 1 + ethSegOctowords(s.inlineLen) + 1
	if s.maxDS > MaxDS {
		return nil, fmt.Errorf("wqe: an inline header of %d bytes needs %d octowords, more than the %d a work queue entry may have",
			s.inlineLen, s.maxDS, MaxDS)
	}
	s.maxBBs = (s.maxDS*Octoword + WQEBB - 1) / WQEBB
	if s.maxBBs > sq.bbs {
		return nil, fmt.Errorf("wqe: an entry of %d blocks does not fit a send queue of %d", s.maxBBs, sq.bbs)
	}
	return s, nil
}

// MaxBBs is the most blocks one entry can occupy. An entry may be smaller, so
// this is what to reserve, not what to advance by.
func (s *Sender) MaxBBs() uint32 { return s.maxBBs }

// InlineLen is how many packet bytes an entry inlines at most.
func (s *Sender) InlineLen() int { return int(s.inlineLen) }

// Put writes one SEND entry at block index idx for a packet whose bytes are
// frame and whose device address is addr, and returns how many blocks it
// occupies.
//
// The first InlineLen bytes of the packet are copied into the entry and the
// rest is pointed at, because an mlx5 device in L2 inline mode parses the
// Ethernet header out of the entry itself. A packet no longer than InlineLen is
// carried entirely inside the entry, which then has no data segment.
//
// signaled asks for a completion. Only the last entry of a batch needs one: the
// send queue completes in order, so that completion accounts for everything
// before it, and asking for fewer completions is most of what makes batching
// worth anything.
//
// Put does not check that the queue has room, and it does not publish anything.
// The caller reserves blocks, writes its entries, and then publishes them all
// with one doorbell.
func (s *Sender) Put(idx uint16, frame []byte, addr uint64, signaled bool) uint32 {
	inline := uint32(len(frame))
	if inline > s.inlineLen {
		inline = s.inlineLen
	}
	rest := uint32(len(frame)) - inline

	ethOcts := ethSegOctowords(inline)
	ds := 1 + ethOcts
	if rest > 0 {
		ds++
	}

	off := s.sq.offset(uint32(idx))

	// Control segment.
	s.sq.putU32BE(off+ctrlOpmodIdxOpcode, uint32(idx)<<8|OpcodeSend)
	s.sq.putU32BE(off+ctrlQPNDS, s.qpn<<8|ds)
	var fmce uint32
	if signaled {
		fmce = CtrlCQUpdate
	}
	s.sq.putU32BE(off+ctrlSignature, fmce) // signature and stream stay zero
	s.sq.putU32BE(off+ctrlImm, 0)

	// Ethernet segment: a fixed 14 bytes, then the inline header, then padding
	// to the end of the segment. The padding is written because what is left
	// there otherwise is the previous entry that used this part of the queue.
	s.sq.putU32BE(off+ethRsvd0, 0)
	s.sq.putU32BE(off+ethCSFlags, uint32(s.csFlags)<<24) // and no segmentation
	s.sq.putU32BE(off+ethRsvd2, 0)
	s.sq.putU16BE(off+ethInlineHdrSz, uint16(inline))
	s.sq.putBytes(off+ethInlineHdr, frame[:inline])
	if pad := CtrlSegSize + ethOcts*Octoword - (ethInlineHdr + inline); pad > 0 {
		s.sq.zero(off+ethInlineHdr+inline, pad)
	}

	if rest > 0 {
		dseg := off + CtrlSegSize + ethOcts*Octoword
		s.sq.putU32BE(dseg+dsegByteCount, rest)
		s.sq.putU32BE(dseg+dsegLKey, s.lkey)
		s.sq.putU64BE(dseg+dsegAddr, addr+uint64(inline))
	}

	return (ds*Octoword + WQEBB - 1) / WQEBB
}

// FirstOctoword returns the first eight bytes of the entry at block index idx,
// in host order. This is what a send doorbell writes to the device's user
// access region, so the value is read back out of the queue rather than kept
// alongside it.
func (s *Sender) FirstOctoword(idx uint16) uint64 {
	off := s.sq.offset(uint32(idx))
	_ = s.sq.buf[off+7]
	return nativeU64(s.sq.buf[off:])
}
