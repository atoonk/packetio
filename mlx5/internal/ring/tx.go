// Package ring drives an mlx5 send or receive queue: it decides what goes in
// the queue, tracks who owns each frame, and reads completions back.
//
// It is deliberately free of cgo and of any dependency on a real device. What
// it needs is a few slices and pointers describing queue memory, which on
// hardware come from mlx5dv_init_obj and in tests come from the mocknic
// package. That is what makes the ownership rules testable without a NIC.
package ring

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"sync/atomic"
	"unsafe"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/mlx5/internal/arch"
	"github.com/atoonk/packetio/mlx5/internal/wqe"
)

// TxConfig describes the memory of one send queue and its completion queue.
// On hardware every field comes from mlx5dv_init_obj; in tests they are
// ordinary Go allocations.
type TxConfig struct {
	// SQ is the send queue's work queue entry buffer, and CQ its completion
	// queue's entry buffer.
	SQ []byte
	CQ []byte

	// SQDbrec is the send queue's doorbell record word, and CQDbrec the
	// completion queue's. Each is one 32-bit word of host memory the NIC reads.
	SQDbrec *uint32
	CQDbrec *uint32

	// UAR is the eight bytes of the device's user access region that this
	// queue's doorbell is written to.
	UAR unsafe.Pointer

	// QPN is the queue pair number and LKey the memory key of the region.
	QPN  uint32
	LKey uint32

	// Region is the frame memory, and RegionVA the address the NIC knows it
	// by. Frame descriptors carry offsets into Region; the NIC is given
	// RegionVA plus the offset.
	Region   []byte
	RegionVA uint64

	// FrameSize is the size of one frame in Region, which must be a power of
	// two. A descriptor may not name bytes past the end of the frame it starts
	// in: the NIC would fetch whatever follows, so a length that runs over is
	// another frame's contents on the wire.
	FrameSize int

	// InlineLen is how many bytes of each packet to copy into its work queue
	// entry rather than point at, which must be at least what the device's
	// inline mode requires.
	InlineLen int

	// CSFlags asks the NIC to compute checksums.
	CSFlags uint8

	// MultiPacket carries several packets in one work queue entry, copied in
	// rather than pointed at. It is what lets a device transmit small packets
	// at line rate, and it may only be used where the device reports it can do
	// it and requires no inline Ethernet header.
	MultiPacket bool

	// MultiPacketMaxLen is the longest packet carried that way. A longer one
	// goes in an ordinary entry, because copying it costs more than the fetch
	// it saves. Pointer mode ignores it: a pointed-at packet is sixteen bytes
	// of entry whatever its length.
	MultiPacketMaxLen int

	// MultiPacketPointer points at the packets instead of copying them in:
	// each becomes one data segment, up to 58 behind one control segment.
	// On the cards measured this is what multi-packet entries should be --
	// the device drains pointer entries three times faster than copied ones
	// -- and copying remains only as the explicitly-asked-for mode.
	MultiPacketPointer bool

	// SignalEvery asks for a completion at least every this many packets.
	// Zero means once per batch, which is the cheapest and is enough: the send
	// queue completes in order, so one completion accounts for everything
	// before it.
	SignalEvery int
}

// TxStats counts what a send ring has done. The fields are atomic because a
// monitoring goroutine reads them while the queue's own goroutine writes them.
//
// They are not added to on every batch. A locked add is a serialising
// instruction, and half a dozen of them per batch of a few packets cost more
// than everything else in the transmit path put together -- measured at 37% of
// the processor before this was changed. The queue counts in ordinary
// variables it alone touches and publishes them here now and then, which is
// accurate to a fraction of a millisecond and free the rest of the time.
type TxStats struct {
	Packets     atomic.Uint64
	Bytes       atomic.Uint64
	Completed   atomic.Uint64
	Batches     atomic.Uint64
	Completions atomic.Uint64
	RingFull    atomic.Uint64
	Errors      atomic.Uint64

	// BadDesc counts descriptors refused because they did not name bytes
	// inside one frame. It is never non-zero without a bug above this layer.
	BadDesc atomic.Uint64
}

// counters are the same numbers as they are kept between publications: plain
// variables belonging to the one goroutine that drives the queue.
type counters struct {
	packets     uint64
	bytes       uint64
	completed   uint64
	batches     uint64
	completions uint64
	ringFull    uint64
	errors      uint64
	badDesc     uint64

	sincePublish uint32
}

// publishEvery is how many batches may pass before the counters are made
// visible. It is a power of two so the test is a mask, and small enough that a
// reader sampling once a second sees numbers that are microseconds old.
const publishEvery = 256

// publish makes the queue's counts visible to anyone reading Stats.
func (t *Tx) publish() {
	c := &t.c
	c.sincePublish = 0
	t.Stats.Packets.Store(c.packets)
	t.Stats.Bytes.Store(c.bytes)
	t.Stats.Completed.Store(c.completed)
	t.Stats.Batches.Store(c.batches)
	t.Stats.Completions.Store(c.completions)
	t.Stats.RingFull.Store(c.ringFull)
	t.Stats.Errors.Store(c.errors)
	t.Stats.BadDesc.Store(c.badDesc)
}

// tick counts one batch and publishes the counters when enough have passed.
func (t *Tx) tick() {
	t.c.sincePublish++
	if t.c.sincePublish >= publishEvery {
		t.publish()
	}
}

// Sync makes the queue's counters current. It must be called from the goroutine
// that drives the queue, and it is only needed by a caller that wants an exact
// reading at a particular moment rather than one a fraction of a millisecond
// old.
//
// The queue also publishes by itself every few hundred batches, whenever a
// batch is refused, and whenever it falls idle -- so a queue that has stopped
// always reports the truth without anyone asking.
func (t *Tx) Sync() { t.publish() }

// slot records where one posted packet's frame came from, so it can be returned
// to the pool when the NIC is finished with it.
type slot = packetio.Desc

// entryMeta records what one work queue entry did, so that a completion naming
// it can be turned back into a set of frames to release.
//
// It is kept for every entry, not just the ones that asked for a completion.
// An entry that fails produces an error completion naming itself even though it
// never asked to be told about success, and releasing the wrong frames at that
// point hands the application memory the NIC is still reading.
type entryMeta struct {
	slotTail uint32 // slot ring position after this entry's packets
	bbs      uint16 // blocks this entry occupies
}

// Tx is one send queue.
//
// Indices are free-running 32-bit counters, masked when used. The hardware sees
// only their low 16 bits, in the producer index of a work queue entry and in
// the doorbell record, and reports only the low 16 bits back in a completion;
// keeping the full counter in software is what makes "how many are in flight"
// answerable at all.
type Tx struct {
	sender *wqe.Sender
	mpw    *wqe.MPWSender
	frames [][]byte // gathered for one multi-packet entry; reused
	paddrs []uint64 // their device addresses, gathered alongside; pointer mode only
	cq     wqe.CQ

	sqDbrec *uint32
	cqDbrec *uint32
	uar     unsafe.Pointer

	region    []byte
	regionVA  uint64
	frameMask uint64 // frame size less one: a descriptor may not cross this
	sqBytes   []byte // the send queue, for patching an entry after it is written

	bbs    uint32 // blocks in the send queue
	bbMask uint32
	maxBBs uint32 // the most blocks one entry can occupy
	pi     uint32 // next block to write
	head   uint32 // oldest block the NIC has not finished
	ci     uint32 // completion queue consumer index

	slots       []slot
	slotMask    uint32
	slotHead    uint32 // oldest frame the NIC has not returned
	slotTail    uint32 // next free slot
	meta        []entryMeta
	signalEvery uint32
	sinceSignal uint32

	// failed is set once the hardware reports a failure. It is atomic because
	// a monitoring goroutine reads it while the queue's own goroutine writes
	// it; everything else here belongs to the one goroutine that owns the
	// queue.
	failed atomic.Pointer[wqe.ErrorInfo]

	c counters

	Stats TxStats
}

// NewTx prepares a send ring over the given queue memory.
func NewTx(cfg TxConfig) (*Tx, error) {
	sq, err := wqe.NewSQ(cfg.SQ)
	if err != nil {
		return nil, err
	}
	cq, err := wqe.NewCQ(cfg.CQ, wqe.CQESize)
	if err != nil {
		return nil, err
	}
	sender, err := wqe.NewSender(sq, wqe.SenderConfig{
		QPN:       cfg.QPN,
		LKey:      cfg.LKey,
		InlineLen: cfg.InlineLen,
		CSFlags:   cfg.CSFlags,
	})
	if err != nil {
		return nil, err
	}
	if cfg.SQDbrec == nil || cfg.CQDbrec == nil || cfg.UAR == nil {
		return nil, fmt.Errorf("ring: send queue is missing a doorbell record or user access region")
	}
	// Every entry that can be outstanding must have somewhere to record what it
	// did, and every packet a slot. An entry is at least one block and carries
	// one packet, so a slot per block is always enough; an entry carrying
	// several packets, which is how these devices reach their highest rates,
	// would need more.
	slots := sq.BBs()
	if cq.Entries() < sq.BBs() {
		return nil, fmt.Errorf("ring: a completion queue of %d entries is smaller than the %d that can be outstanding", cq.Entries(), slots)
	}

	var mpw *wqe.MPWSender
	slotsPerBlock := uint32(1)
	if cfg.MultiPacket {
		maxLen := cfg.MultiPacketMaxLen
		if maxLen <= 0 && !cfg.MultiPacketPointer {
			return nil, fmt.Errorf("ring: multi-packet entries need a longest packet length")
		}
		if cfg.InlineLen != 0 {
			// The compact Ethernet segment of a multi-packet entry has no room
			// for an inline header, so a device that insists on one cannot be
			// given these entries at all.
			return nil, fmt.Errorf("ring: multi-packet entries cannot carry the %d inline header bytes this device requires", cfg.InlineLen)
		}
		if mpw, err = wqe.NewMPWSender(sq, wqe.MPWConfig{
			QPN: cfg.QPN, CSFlags: cfg.CSFlags, MaxLen: maxLen,
			Pointer: cfg.MultiPacketPointer, LKey: cfg.LKey,
		}); err != nil {
			return nil, err
		}
		// One entry can hold more packets than it does blocks, so the record of
		// what is in flight has to be that much larger than the queue. The
		// worst case is the shortest packets, not the longest: the more that
		// fit in an entry, the more slots a block's worth of entries needs.
		perEntry := uint32(wqe.MaxDS - 2) // one octoword each, the smallest a packet can take
		entryBlocks := ((2 + perEntry) * wqe.Octoword) / wqe.WQEBB
		slotsPerBlock = (perEntry + entryBlocks - 1) / entryBlocks
		if slotsPerBlock == 0 {
			slotsPerBlock = 1
		}
	}
	slots = nextPow2(slots * slotsPerBlock)

	t := &Tx{
		sender:      sender,
		mpw:         mpw,
		cq:          cq,
		sqDbrec:     cfg.SQDbrec,
		cqDbrec:     cfg.CQDbrec,
		uar:         cfg.UAR,
		region:      cfg.Region,
		regionVA:    cfg.RegionVA,
		frameMask:   uint64(cfg.FrameSize) - 1,
		sqBytes:     cfg.SQ,
		bbs:         sq.BBs(),
		bbMask:      sq.BBs() - 1,
		maxBBs:      sender.MaxBBs(),
		slots:       make([]slot, slots),
		slotMask:    slots - 1,
		frames:      make([][]byte, 0, wqe.MaxDS),
		paddrs:      make([]uint64, 0, wqe.MaxDS),
		meta:        make([]entryMeta, sq.BBs()),
		signalEvery: uint32(cfg.SignalEvery),
	}
	if bits.OnesCount32(t.bbs) != 1 {
		return nil, fmt.Errorf("ring: send queue of %d blocks is not a power of two", t.bbs)
	}
	if cfg.FrameSize <= 0 || bits.OnesCount64(uint64(cfg.FrameSize)) != 1 {
		return nil, fmt.Errorf("ring: frame size %d is not a power of two", cfg.FrameSize)
	}
	if len(cfg.Region)%cfg.FrameSize != 0 {
		// Checked once so the packet path need not. A descriptor that stays
		// inside its own frame is then inside the region by construction,
		// which is one comparison per packet saved.
		return nil, fmt.Errorf("ring: a region of %d bytes is not a whole number of %d-byte frames",
			len(cfg.Region), cfg.FrameSize)
	}
	return t, nil
}

// descOK reports whether a descriptor names at least one byte inside one frame
// of the region.
//
// This is the check BACKENDS.md requires of every backend, and it guards the
// two things that go wrong without it. A descriptor past the region slices out
// of range, which panics a process that runs as root. One inside the region but
// running past its own frame is worse and silent: the NIC is told to fetch
// those bytes, so the neighbouring frames -- other flows' packets -- go on the
// wire.
//
// The arithmetic subtracts rather than adds. Addr and Len are attacker-reachable
// on a forwarding path, where Len came from a completion, and an addition on
// the left of the comparison is how this kind of check goes wrong.
func (t *Tx) descOK(d packetio.Desc) bool {
	// The region is a whole number of frames, checked in NewTx, so a
	// descriptor that starts inside the region and ends inside its own frame
	// is inside the region: no separate region-end comparison is needed.
	// Two comparisons, not three: subtracting one from the length folds the
	// zero-length case into the bounds test, because zero wraps to the largest
	// uint64 and fails it. Len-1 < room is Len <= room for every non-zero Len.
	return d.Addr < uint64(len(t.region)) &&
		uint64(d.Len)-1 < t.frameMask+1-(d.Addr&t.frameMask)
}

// InlineLen is how many packet bytes each entry inlines.
func (t *Tx) InlineLen() int { return t.sender.InlineLen() }

// MultiPacket reports whether several packets are carried in one entry.
func (t *Tx) MultiPacket() bool { return t.mpw != nil }

// entryBlocksFor is how many blocks a full multi-packet entry occupies.
func entryBlocksFor(maxLen, packets uint32) uint32 {
	perPacket := (4 + maxLen + wqe.Octoword - 1) / wqe.Octoword
	ds := 2 + packets*perPacket
	blocks := (ds*wqe.Octoword + wqe.WQEBB - 1) / wqe.WQEBB
	if blocks == 0 {
		return 1
	}
	return blocks
}

func nextPow2(n uint32) uint32 {
	p := uint32(1)
	for p < n {
		p <<= 1
	}
	return p
}

// FreeSlots is how many more packets the queue can accept.
func (t *Tx) FreeSlots() int {
	if t.failed.Load() != nil {
		return 0
	}
	free := t.bbs - (t.pi - t.head)
	if t.mpw != nil {
		// Blocks are not the limit here; how many packets those blocks can be
		// made to hold is, and that depends on how long the packets turn out
		// to be. This is an estimate, so Post may take fewer than it offers --
		// which is why Post returns a count.
		// Assume packets pack as well as the longest one allowed, which is
		// pessimistic for short packets and never promises room that is not
		// there.
		// In pointer mode a packet is one octoword of descriptor, not the
		// copy-mode length cap: MaxLen means nothing here, and deriving the
		// estimate from it under-reported the default queue's room by ~12x --
		// Alloc clamped and ringFull tripped while dozens of packets of real
		// capacity remained.
		//
		// The entry's control and Ethernet segments are two octowords that
		// carry no packet, and Post fills one entry at a time, so the room is
		// counted in entries: as many full ones as fit, then whatever a
		// partial one can still hold. Counting packets instead would promise
		// the headers' worth of room that is not there, and Alloc's guarantee
		// -- everything it offers, the next Transmit takes -- rests on this
		// staying pessimistic.
		var byBlocks uint64
		if t.mpw.Pointer() {
			const perEntry = wqe.MaxDS - 2 // packets behind one header
			const entryBBs = ((2 + perEntry) * wqe.Octoword) / wqe.WQEBB
			byBlocks = uint64(free/entryBBs) * perEntry
			if rem := int64(free%entryBBs)*(wqe.WQEBB/wqe.Octoword) - 2; rem > 0 {
				byBlocks += uint64(rem)
			}
		} else {
			perPacket := (4 + uint64(t.mpw.MaxLen()) + wqe.Octoword - 1) / wqe.Octoword
			byBlocks = uint64(free) * (wqe.WQEBB / wqe.Octoword) / perPacket
		}
		bySlots := uint64(uint32(len(t.slots)) - (t.slotTail - t.slotHead))
		if byBlocks > bySlots {
			byBlocks = bySlots
		}
		return int(byBlocks)
	}
	// An entry may turn out to be smaller than the largest one, but the room
	// has to be reserved before the packet is looked at.
	return int(free / t.maxBBs)
}

// Capacity is the most packets that can be outstanding at once.
func (t *Tx) Capacity() int { return int(t.bbs / t.maxBBs) }

// InFlight is how many packets the NIC currently owns.
func (t *Tx) InFlight() int { return int(t.slotTail - t.slotHead) }

// CQConsumerIndex is how far the ring has read its completion queue, and
// SQHead and SQProducer are the blocks the queue has completed up to and
// written up to. They are counters, not indices: they run free and wrap at
// 2^32, and the hardware sees only their low bits.
func (t *Tx) CQConsumerIndex() uint32 { return t.ci }

// SQHead is the oldest block the NIC has not finished with.
func (t *Tx) SQHead() uint32 { return t.head }

// SQProducer is the next block software will write.
func (t *Tx) SQProducer() uint32 { return t.pi }

// Failed reports the hardware error that put the queue out of service, or nil.
// It is safe to call from any goroutine.
func (t *Tx) Failed() *wqe.ErrorInfo { return t.failed.Load() }

// Post writes work queue entries for as many of descs as will fit, publishes
// them with one doorbell, and returns how many it took. The frames it took
// belong to the NIC until a completion returns them; the rest still belong to
// the caller.
func (t *Tx) Post(descs []packetio.Desc) int {
	if t.failed.Load() != nil {
		return 0
	}
	n := len(descs)
	if free := t.FreeSlots(); n > free {
		if free == 0 {
			t.c.ringFull++
			t.publish() // nothing is happening, so this costs nothing
			return 0
		}
		n = free
	}
	if n == 0 {
		// Nothing to publish. Ringing the doorbell anyway would tell the NIC
		// to re-read a producer index it already has, using bytes from an
		// entry it may still be working on.
		return 0
	}
	if t.mpw != nil {
		return t.postMulti(descs[:n])
	}

	var bytes uint64
	var lastIdx uint16
	posted := 0
	for i := 0; i < n; i++ {
		d := descs[i]
		// Checked here rather than in a pass of its own: the descriptors are
		// walked once, and the ones already posted are still published below.
		if !t.descOK(d) {
			t.c.badDesc++
			break
		}
		idx := uint16(t.pi)

		// Ask for a completion on the last entry of the batch, and every so
		// often within a long one if the caller wanted that.
		t.sinceSignal++
		signal := i == n-1 || (t.signalEvery != 0 && t.sinceSignal >= t.signalEvery)
		if signal {
			t.sinceSignal = 0
		}

		bbs := t.sender.Put(idx, t.region[d.Addr:d.Addr+uint64(d.Len)], t.regionVA+d.Addr, signal)

		t.slots[t.slotTail&t.slotMask] = d
		t.slotTail++
		t.meta[t.pi&t.bbMask] = entryMeta{slotTail: t.slotTail, bbs: uint16(bbs)}
		t.pi += bbs
		lastIdx = idx
		bytes += uint64(d.Len)
		posted++
	}
	if posted == 0 {
		t.publish() // a refusal means a bug above; make it visible now
		return 0
	}
	n = posted

	// One doorbell for the whole batch. Everything written above becomes
	// visible to the device here, and not before.
	var db [4]byte
	binary.BigEndian.PutUint32(db[:], t.pi&0xffff)
	arch.PublishDoorbell(t.sqDbrec, nativeU32(db[:]), t.uar, t.sender.FirstOctoword(lastIdx))

	t.c.packets += uint64(n)
	t.c.bytes += bytes
	t.c.batches++
	t.tick()
	return n
}

// postMulti writes as many packets as it can into multi-packet entries, each
// carrying several packets copied in behind their lengths, and publishes them
// all with one doorbell.
//
// It falls back to an ordinary entry for a packet that cannot be carried this
// way -- too long to be worth copying, or alone at the end of a batch -- so a
// mixture of sizes still goes out in one pass.
func (t *Tx) postMulti(descs []packetio.Desc) int {
	var (
		bytes   uint64
		lastIdx uint16
		posted  int
		anyPut  bool
		bad     bool // a descriptor was refused; post what was gathered, then stop
		// checked is how far along descs validation has already reached. An
		// entry that takes fewer packets than were gathered leaves the rest to
		// be gathered again next time round, and without this they would be
		// validated again with them -- several times over, for a batch that
		// packs badly.
		checked int
	)

	for posted < len(descs) && !bad {
		free := t.bbs - (t.pi - t.head)
		if free == 0 || t.slotTail-t.slotHead == uint32(len(t.slots)) {
			break
		}

		// Gather what can share one entry: consecutive packets short enough to
		// copy, as many as the entry's octoword budget allows. How many that
		// is depends on how long they actually are, so gather up to the most
		// an entry could ever hold and let Fits cut it down.
		t.frames = t.frames[:0]
		limit := posted + wqe.MaxDS - 2
		if limit > len(descs) {
			limit = len(descs)
		}
		if room := int(t.slotTail - t.slotHead); limit-posted > len(t.slots)-room {
			limit = posted + len(t.slots) - room
		}
		pointer := t.mpw.Pointer()
		t.paddrs = t.paddrs[:0]
		for i := posted; i < limit; i++ {
			d := descs[i]
			if i >= checked {
				if !t.descOK(d) {
					t.c.badDesc++
					bad = true
					break
				}
				checked = i + 1
			}
			t.frames = append(t.frames, t.region[d.Addr:d.Addr+uint64(d.Len)])
			if pointer {
				t.paddrs = append(t.paddrs, t.regionVA+d.Addr)
			}
		}
		if len(t.frames) == 0 {
			break
		}
		k := t.mpw.Fits(t.frames)

		idx := uint16(t.pi)
		var bbs uint32
		switch {
		case k >= 2 && pointer:
			bbs = (uint32(2+k)*wqe.Octoword + wqe.WQEBB - 1) / wqe.WQEBB
			if bbs > free {
				break
			}
			t.mpw.PutPointers(idx, t.frames[:k], t.paddrs[:k], false)
		case k >= 2:
			bbs = mpwBlocks(t.frames[:k])
			if bbs > free {
				break
			}
			t.mpw.Put(idx, t.frames[:k], false)
		case posted < len(descs):
			// One packet on its own: an ordinary entry describes it more
			// cheaply than a multi-packet entry carrying one.
			k = 1
			d := descs[posted]
			if t.maxBBs > free {
				break
			}
			bbs = t.sender.Put(idx, t.region[d.Addr:d.Addr+uint64(d.Len)], t.regionVA+d.Addr, false)
		}
		if bbs == 0 || bbs > free {
			break
		}

		for i := 0; i < k; i++ {
			d := descs[posted+i]
			t.slots[t.slotTail&t.slotMask] = d
			t.slotTail++
			bytes += uint64(d.Len)
		}
		t.meta[t.pi&t.bbMask] = entryMeta{slotTail: t.slotTail, bbs: uint16(bbs)}
		t.pi += bbs
		lastIdx = idx
		posted += k
		anyPut = true
	}

	if !anyPut {
		t.c.ringFull++
		t.publish() // nothing is happening, so this costs nothing
		return 0
	}

	// The last entry of the batch is the one that asks to be reported, and the
	// record kept for it says how many packets it accounts for.
	t.setSignaled(lastIdx)
	var db [4]byte
	binary.BigEndian.PutUint32(db[:], t.pi&0xffff)
	arch.PublishDoorbell(t.sqDbrec, nativeU32(db[:]), t.uar, t.mpw.FirstOctoword(lastIdx))

	t.c.packets += uint64(posted)
	t.c.bytes += bytes
	t.c.batches++
	t.tick()
	return posted
}

// setSignaled asks for a completion on the entry at idx, after it was written.
func (t *Tx) setSignaled(idx uint16) {
	off := (uint32(idx) * wqe.WQEBB) & (uint32(len(t.sqBytes)) - 1)
	t.sqBytes[off+11] |= wqe.CtrlCQUpdate
}

// mpwBlocks is how many blocks a multi-packet entry carrying these frames takes.
func mpwBlocks(frames [][]byte) uint32 {
	ds := uint32(2)
	for _, f := range frames {
		ds += (4 + uint32(len(f)) + wqe.Octoword - 1) / wqe.Octoword
	}
	return (ds*wqe.Octoword + wqe.WQEBB - 1) / wqe.WQEBB
}

// Complete reads completions and appends the frames they release to out,
// returning the grown slice.
//
// max bounds the work rather than the result: reading stops once that many
// frames have been released, but a completion is never split, so the first one
// is taken whatever it covers. With one completion per batch, which is the
// default, that means a whole batch comes back at once.
//
// The send queue finishes entries in order, so the last completion read
// accounts for every packet before it and only that one needs acting on.
//
// A packet is owned by the NIC from the moment Post takes it until it comes
// back from here. Releasing one early hands the application a frame the NIC is
// still reading, which corrupts a packet already on its way out and shows up
// nowhere in any counter, so every completion is checked against what is
// actually outstanding before anything is released.
func (t *Tx) Complete(max int, out []packetio.Desc) []packetio.Desc {
	if max <= 0 {
		return out
	}

	var (
		release   uint32
		newHead   uint32
		haveAny   bool
		completed uint64
	)
	for i := uint32(0); i < t.cq.Entries(); i++ {
		// Everything the device wrote before saying this entry was ready has to
		// be visible before any of it is read, and that is a barrier per entry:
		// see the note in Rx.Receive for why one per round is not enough.
		header := arch.LoadCQEHeader(t.cq.HeaderPtr(t.ci))
		if !t.cq.Owned(t.ci, header) {
			break
		}
		if wqe.Format(header) != wqe.CQEFormatNoData {
			// A compressed or inline-scatter completion means the queue was
			// set up with something this code does not decode. Stop rather
			// than guess: consuming it would put the consumer index out of
			// step with the hardware for good.
			t.c.errors++
			break
		}

		opcode := wqe.Opcode(header)
		if opcode != wqe.CQEReq && opcode != wqe.CQEReqErr {
			t.ci++
			completed++
			t.c.errors++
			continue
		}

		target, head, ok := t.entryFor(header)
		if ok && haveAny && target-t.slotHead > uint32(max) {
			break
		}

		t.ci++
		completed++

		if opcode == wqe.CQEReqErr {
			info := wqe.Error(t.cq.Entry(t.ci - 1))
			t.c.errors++
			t.failed.CompareAndSwap(nil, &info)
			// The frames this entry described are finished with either way,
			// and the entries behind it are about to be flushed with their own
			// error completions, so releasing up to here is still correct.
		}
		if !ok {
			// A completion naming an entry that is not outstanding: stale from
			// an earlier lap, or hardware misbehaving. Consume it so the queue
			// keeps moving, but do not let it release anything.
			t.c.errors++
			continue
		}

		release, newHead, haveAny = target, head, true
		if release-t.slotHead >= uint32(max) {
			break
		}
	}

	if completed == 0 {
		return out
	}
	t.c.completions += completed

	if haveAny {
		freed := release - t.slotHead
		for ; t.slotHead != release; t.slotHead++ {
			out = append(out, t.slots[t.slotHead&t.slotMask])
		}
		t.head = newHead
		t.c.completed += uint64(freed)
		if t.slotTail == t.slotHead {
			t.publish() // the queue has drained, so this costs nothing
		}
	}

	// Telling the NIC how far software has read frees those entries for reuse,
	// so it comes after every read of them.
	var db [4]byte
	binary.BigEndian.PutUint32(db[:], t.ci&0xffffff)
	arch.ReleaseCQ(t.cqDbrec, nativeU32(db[:]))
	return out
}

// Completable is how many frames Complete could release right now. It reads the
// completions without consuming them.
func (t *Tx) Completable() int {
	var release uint32
	var haveAny bool
	for i, ci := uint32(0), t.ci; i < t.cq.Entries(); i, ci = i+1, ci+1 {
		header := arch.LoadCQEHeaderRaw(t.cq.HeaderPtr(ci))
		if !t.cq.Owned(ci, header) || wqe.Format(header) != wqe.CQEFormatNoData {
			break
		}
		switch wqe.Opcode(header) {
		case wqe.CQEReq, wqe.CQEReqErr:
			if target, _, ok := t.entryFor(header); ok {
				release, haveAny = target, true
			}
		}
	}
	if !haveAny {
		return 0
	}
	return int(release - t.slotHead)
}

// entryFor turns a completion's work queue entry counter into the slot ring
// position everything up to and including that entry has reached, and the block
// index the queue has then completed up to.
//
// It rejects a counter that does not name an entry currently in flight. A
// completion left over from an earlier lap of the queue, or one naming an entry
// software never posted, would otherwise release frames that are still in use
// or that were released already.
func (t *Tx) entryFor(header uint32) (slotTail, head uint32, ok bool) {
	counter := wqe.WQECounter(header)

	// The counter is the low 16 bits of a block index. The entry it names lies
	// between head and pi, a span no larger than the queue, so its distance
	// from head places it exactly.
	delta := uint32(counter - uint16(t.head))
	if delta >= t.pi-t.head {
		return 0, 0, false
	}

	m := t.meta[(t.head+delta)&t.bbMask]
	if m.bbs == 0 {
		return 0, 0, false
	}
	// What the entry recorded must name frames the NIC still owns, and must
	// not go backwards. Given the two checks above this should be
	// unreachable; it is here because the cost is one comparison per batch and
	// the alternative to catching it is a corrupted packet nobody can trace.
	if used := m.slotTail - t.slotHead; used == 0 || used > t.slotTail-t.slotHead {
		return 0, 0, false
	}
	return m.slotTail, t.head + delta + uint32(m.bbs), true
}

// nativeU32 reads four bytes in host order, so that storing the result
// reproduces them exactly. The doorbell record holds a big-endian index; the
// store that publishes it is an ordinary word store.
func nativeU32(b []byte) uint32 { return binary.NativeEndian.Uint32(b) }
