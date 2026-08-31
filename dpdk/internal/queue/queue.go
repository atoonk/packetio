package queue

import (
	"fmt"
	"sync/atomic"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/dpdk/internal/mbuf"
	"github.com/atoonk/packetio/internal/pool"
)

// PMD is the driver, as this package needs it: three calls, each a single cgo
// crossing in production and none of them per packet.
//
// Measured on a ConnectX-6 Dx, a crossing costs about 36 ns. Over a batch of 64
// that is under a nanosecond a packet, which is why the whole design is built
// around keeping the crossings per batch rather than per frame.
type PMD interface {
	// TxBurst hands mbuf addresses to the driver and returns how many it
	// accepted, always a prefix. The driver owns those buffers until it frees
	// them, which it does into the mempool each mbuf names.
	TxBurst(mbufs []uint64) int

	// RxBurst fills out with the addresses of received mbufs and returns how
	// many. The driver takes the buffers it needs from the supply ring.
	RxBurst(out []uint64) int

	// Poke asks the driver to look at its transmit completions now.
	//
	// It exists because DPDK has no way to say "give me back what you have
	// finished with". A PMD processes completions inside its own transmit
	// burst and nowhere else, so a queue that has stopped sending never gets
	// its last frames back. On mlx5, rte_eth_tx_done_cleanup is not
	// implemented at all and rte_eth_tx_descriptor_status is what runs the
	// completion handler; either way it is one call, on the idle path only.
	Poke()
}

// Config describes one queue's share of a device.
type Config struct {
	// Region is the frame memory, and RegionVA the address the same memory is
	// known by to the driver and to the mempool. Subtracting one from the other
	// is how an address the driver returns becomes an offset this package can
	// index -- the only pointer arithmetic in the backend, done once per batch.
	Region   []byte
	RegionVA uint64

	// Layout says where the mbuf and the packet sit inside a frame.
	Layout mbuf.Layout

	// PoolVA is this queue's rte_mempool, which every frame it hands to the
	// driver is stamped with so that the driver frees it back here.
	PoolVA uint64

	// FirstFrame and Frames are this queue's slice of the region, in frames.
	// Queues never share a frame: that is what lets their free lists run
	// without a lock.
	FirstFrame int
	Frames     int

	// Depth is the driver's descriptor ring depth, which bounds how many
	// frames can be outstanding at once.
	Depth int

	// Supply is the ring the driver draws receive buffers from, and Returned
	// the ring it frees finished buffers into. A transmit queue uses only
	// Returned; a receive queue uses both.
	Supply   *Ring
	Returned *Ring

	// Checksums says the device took transmit checksum offload, so plain
	// Transmit should ask the NIC to compute them, and TSO that it took
	// segmentation. Both are what the device actually accepted at configure
	// time, never what was asked for.
	Checksums bool
	TSO       bool

	PMD PMD
}

func (c Config) validate() error {
	if err := c.Layout.Validate(); err != nil {
		return err
	}
	switch {
	case len(c.Region) == 0:
		return fmt.Errorf("dpdk: a queue with no region")
	case len(c.Region)%c.Layout.FrameSize != 0:
		// Checked once here so the packet path need not: a descriptor that
		// stays inside its own frame is then inside the region by construction.
		return fmt.Errorf("dpdk: a region of %d bytes is not a whole number of %d-byte frames",
			len(c.Region), c.Layout.FrameSize)
	case c.Frames <= 0:
		return fmt.Errorf("dpdk: a queue with %d frames", c.Frames)
	case (c.FirstFrame+c.Frames)*c.Layout.FrameSize > len(c.Region):
		return fmt.Errorf("dpdk: frames %d..%d run past the %d-byte region",
			c.FirstFrame, c.FirstFrame+c.Frames, len(c.Region))
	case c.Depth <= 0:
		return fmt.Errorf("dpdk: a queue of depth %d", c.Depth)
	case c.Returned == nil:
		return fmt.Errorf("dpdk: a queue with no returned ring")
	case c.PMD == nil:
		return fmt.Errorf("dpdk: a queue with no driver")
	}
	return nil
}

// base is the common half of both directions: the region, the layout, the free
// list, and the arithmetic that turns addresses into frames and back.
type base struct {
	cfg    Config
	region []byte
	l      mbuf.Layout
	pool   *pool.Frames

	// regionLen and frameMask are captured rather than read through the config
	// on every descriptor: this is the packet path.
	regionLen uint64
	frameMask uint64
	dataStart uint64
	bufStart  uint64

	// scratch reused by every call so nothing allocates per batch.
	addrs []uint64
	descs []packetio.Desc
	mbufs []uint64
	// burst is the receive burst's own array. It used to share mbufs, and
	// that sharing was a bug that cost a third of the receive rate: Fill
	// truncates mbufs to what it posts, and Receive clamped its burst to
	// len(mbufs) -- so a Fill that topped the supply up by five frames capped
	// every following burst at five packets, and the loop paid a cgo
	// crossing's fixed cost over five packets instead of sixty-four.
	burst []uint64

	closed atomic.Bool

	// dead is the first ownership-invariant violation, latched forever. The
	// owner sets it and a monitoring goroutine reads it through Dead, which
	// is why it is an atomic pointer rather than a plain error.
	dead atomic.Pointer[error]
}

// initBase fills a queue's embedded base in place. In place rather than by
// value because base holds atomics now, and a struct with atomics must never
// be copied -- go vet said so the moment it was tried.
func initBase(b *base, cfg Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	l := cfg.Layout
	b.cfg = cfg
	b.region = cfg.Region
	b.l = l
	b.pool = pool.New(cfg.FirstFrame, cfg.Frames, l.FrameSize)
	b.regionLen = uint64(len(cfg.Region))
	b.frameMask = uint64(l.FrameSize - 1)
	b.dataStart = uint64(l.DataStart())
	b.bufStart = uint64(l.ObjHeader + l.MbufSize)
	n := cfg.Depth
	b.addrs = make([]uint64, 0, n)
	b.descs = make([]packetio.Desc, 0, n)
	b.mbufs = make([]uint64, n)
	b.burst = make([]uint64, n)
	return nil
}

// descOK reports whether a descriptor names at least one byte inside the packet
// area of one frame.
//
// It is the check BACKENDS.md requires of every backend, with one thing to say
// beyond the mlx5 version it is modelled on: a frame here begins with the
// mempool's object header and the mbuf itself, so a descriptor must not only
// stay inside its frame but stay out of the first 192 bytes of it. A length
// running past the frame would put the neighbouring frames on the wire; an
// address below the buffer would have the NIC transmit the mbuf header, and
// then the driver would free a buffer whose header the packet had overwritten.
//
// The arithmetic subtracts rather than adds, because on a forwarding path the
// length came from the hardware, and an addition on the left of the comparison
// is how this kind of check goes wrong. Subtracting one from the length folds
// the zero-length case in: zero wraps to the largest uint64 and fails.
func (b *base) descOK(d packetio.Desc) bool {
	if d.Addr >= b.regionLen {
		return false
	}
	within := d.Addr & b.frameMask
	if within < b.bufStart {
		return false
	}
	return uint64(d.Len)-1 < b.frameMask+1-within
}

// frameOf is the start of the frame an address is in, and mbufOf that frame's
// mbuf. Both are masks, and both run once per packet.
func (b *base) frameOf(addr uint64) uint64 { return addr &^ b.frameMask }

func (b *base) mbufOf(addr uint64) int {
	return int(b.frameOf(addr)) + b.l.ObjHeader
}

// addrOfMbuf turns an address the driver handed back into the offset of that
// mbuf in the region. It is the inverse of the one addition made on the way out.
func (b *base) offsetOfVA(va uint64) (int, bool) {
	if va < b.cfg.RegionVA {
		return 0, false
	}
	off := va - b.cfg.RegionVA
	if off >= b.regionLen {
		return 0, false
	}
	return int(off), true
}

// Region is the frame memory this queue draws on.
func (b *base) Region() []byte { return b.region }

// Owns reports whether an address lies inside this queue's slice of the
// region -- the frames its pool covers. A wrapper that fans several hardware
// queues out behind one logical queue uses it to route a descriptor back to
// the queue whose pool the frame belongs to.
func (b *base) Owns(addr uint64) bool {
	lo := uint64(b.cfg.FirstFrame) * uint64(b.l.FrameSize)
	hi := lo + uint64(b.cfg.Frames)*uint64(b.l.FrameSize)
	return addr >= lo && addr < hi
}

// NumFreeFrames is how many frames are on this queue's free list.
func (b *base) NumFreeFrames() int { return b.pool.Len() }

// PoolRejected is how many frames the pool refused: foreign, misaligned, or
// already free. Anything but zero is a bug above this layer.
func (b *base) PoolRejected() uint64 { return b.pool.Rejected() }

// Close stops the queue. Every method afterwards returns nothing rather than
// touching memory the device may have taken away.
func (b *base) Close() { b.closed.Store(true) }

// Closed reports whether Close has been called.
func (b *base) Closed() bool { return b.closed.Load() }

// fail latches the first fatal error. It is for events after which this
// package can no longer prove where every frame is; from then on the queue
// reports itself dead rather than pretending to be healthy while it leaks.
func (b *base) fail(err error) { b.dead.CompareAndSwap(nil, &err) }

// Dead is the latched fatal error, or nil. Safe from any goroutine.
func (b *base) Dead() error {
	if p := b.dead.Load(); p != nil {
		return *p
	}
	return nil
}

// ---------------------------------------------------------------- transmit

// TxStats counts what a transmit queue has done.
//
// The fields are atomics so that a monitoring goroutine may read them while
// the owner drives the queue. The owner adds once per batch, never per
// packet, so the cost on the packet path is a handful of uncontended atomic
// adds per burst -- the same arrangement the mlx5 backend runs at line rate.
type TxStats struct {
	Packets   atomic.Uint64
	Bytes     atomic.Uint64
	Completed atomic.Uint64
	Batches   atomic.Uint64
	RingFull  atomic.Uint64
	PoolEmpty atomic.Uint64
	BadDesc   atomic.Uint64
	// BadOffload counts Offloads refused: metadata that does not describe the
	// packet it came with, or asks for work this device does not do.
	BadOffload atomic.Uint64
	Pokes      atomic.Uint64
	Foreign    atomic.Uint64 // addresses returned that this region does not contain
}

// Tx is one transmit queue.
type Tx struct {
	base
	inFlight atomic.Int64
	Stats    TxStats
}

// NewTx builds a transmit queue over one slice of the region.
func NewTx(cfg Config) (*Tx, error) {
	t := &Tx{}
	if err := initBase(&t.base, cfg); err != nil {
		return nil, err
	}
	return t, nil
}

// Alloc takes up to n frames from the free list and returns descriptors naming
// the packet area of each, with the headroom already reserved.
//
// It never returns more than the following Transmit could accept, so a caller
// that transmits everything it allocated cannot leak a frame.
func (t *Tx) Alloc(n int) []packetio.Desc {
	if t.closed.Load() || n <= 0 || t.dead.Load() != nil {
		return nil
	}
	if free := t.NumFreeSlots(); n > free {
		if free == 0 {
			t.Stats.RingFull.Add(1)
			return nil
		}
		n = free
	}
	if have := t.pool.Len(); n > have {
		if have == 0 {
			t.Stats.PoolEmpty.Add(1)
			return nil
		}
		n = have
	}
	t.addrs = t.pool.Pop(n, t.addrs[:0])
	t.descs = t.descs[:0]
	for _, frame := range t.addrs {
		t.descs = append(t.descs, packetio.Desc{Addr: frame + t.dataStart})
	}
	return t.descs
}

// Transmit hands descriptors to the driver and returns how many it took, always
// a prefix.
//
// Frames in the prefix belong to the driver until it frees them; frames in the
// rest still belong to the caller. The mbuf of each is written first -- where
// its packet starts, how long it is, and which pool to free it into -- and then
// the whole batch goes across in one call.
//
// Where the device was opened with checksum offload, each packet's headers are
// read here and the NIC is asked to compute its checksums. A packet this
// package cannot parse is sent as it stands, with whatever checksums the caller
// wrote: that is the behaviour of every other backend, and refusing it would
// make an ARP frame an error.
func (t *Tx) Transmit(descs []packetio.Desc) int {
	n, _ := t.transmit(descs, nil)
	return n
}

// TransmitOffload is Transmit with segmentation and checksum metadata for each
// frame, so a super-frame is cut up by the NIC rather than here.
//
// It returns the accepted prefix and an error naming the first descriptor whose
// Offload it refused. A zero Offload sends an ordinary frame. What the device
// cannot do is ErrUnsupported rather than something quietly weaker: a caller
// asking for segmentation and getting a 64 KB frame on the wire has no way to
// notice until the far end does.
func (t *Tx) TransmitOffload(descs []packetio.Desc, offs []packetio.Offload) (int, error) {
	if len(offs) != len(descs) {
		return 0, fmt.Errorf("%w: %d descriptors but %d offloads",
			packetio.ErrBadLength, len(descs), len(offs))
	}
	return t.transmit(descs, offs)
}

// applyOffload writes what one packetio.Offload means for this mbuf, or reports
// why it cannot be honoured.
//
// The translation is between two vocabularies. packetio speaks virtio-net,
// where a checksum is "start here, write two bytes at this offset" and headers
// are one HdrLen; a NIC wants the header lengths broken out, l2 from l3 from
// l4, and a flag saying which protocol it is looking at. Only the packet itself
// can settle that split, so the headers are parsed and the caller's own numbers
// are checked against them rather than trusted: a CsumStart that disagrees with
// the packet is a caller bug that would otherwise reach the wire as a checksum
// written over payload.
func (t *Tx) applyOffload(m int, o packetio.Offload, packet []byte) error {
	if o.Flags == 0 && o.GSOType == packetio.OffloadGSONone {
		return nil // an ordinary frame
	}
	h, ok := mbuf.Parse(packet)
	if !ok {
		return fmt.Errorf("%w: offload asked for a packet whose headers this backend "+
			"does not recognise", packetio.ErrBadLength)
	}
	hdrLen := h.L2Len + h.L3Len
	if o.Flags&packetio.OffloadNeedsCsum != 0 && int(o.CsumStart) != hdrLen {
		return fmt.Errorf("%w: CsumStart %d, but the packet's headers end at %d",
			packetio.ErrBadLength, o.CsumStart, hdrLen)
	}
	flags := mbuf.ChecksumFlags(h)
	if flags == 0 {
		return fmt.Errorf("%w: offload asked for a packet that is neither IPv4 nor IPv6",
			packetio.ErrBadLength)
	}
	l4Len, segSize := uint64(0), uint64(0)

	if o.Segmented() {
		if !t.cfg.TSO {
			return fmt.Errorf("%w: segmentation, which this device was not opened for "+
				"or does not offer", packetio.ErrUnsupported)
		}
		switch o.GSOType &^ packetio.OffloadGSOECN {
		case packetio.OffloadGSOTCPv4, packetio.OffloadGSOTCPv6:
			if h.Proto != mbuf.ProtoTCP {
				return fmt.Errorf("%w: TCP segmentation of a non-TCP packet",
					packetio.ErrBadLength)
			}
		default:
			// UDP segmentation is a separate device offload this backend does
			// not ask for, so it is refused rather than sent unsegmented.
			return fmt.Errorf("%w: segmentation type %d", packetio.ErrUnsupported, o.GSOType)
		}
		if o.GSOSize == 0 {
			return fmt.Errorf("%w: segmented but the segment size is zero", packetio.ErrBadLength)
		}
		if int(o.HdrLen) <= hdrLen || int(o.HdrLen) > len(packet) {
			return fmt.Errorf("%w: header length %d, with %d bytes of L2 and L3 in a "+
				"%d-byte packet", packetio.ErrBadLength, o.HdrLen, hdrLen, len(packet))
		}
		l4Len, segSize = uint64(int(o.HdrLen)-hdrLen), uint64(o.GSOSize)
		// A NIC segmenting TCP writes a fresh checksum into every segment it
		// makes, and DPDK requires the flags to say so whatever the caller
		// asked for. ChecksumFlags has already set them from the packet's own
		// headers, which is why only the segmentation bit is added here.
		flags |= mbuf.TxTCPSeg
	}

	mbuf.SetOlFlags(t.region, m, flags)
	mbuf.SetTxOffloadTSO(t.region, m, uint64(h.L2Len), uint64(h.L3Len), l4Len, segSize)
	return nil
}

func (t *Tx) transmit(descs []packetio.Desc, offs []packetio.Offload) (int, error) {
	if t.closed.Load() {
		return 0, packetio.ErrClosed
	}
	// A dead queue takes nothing more. Handing the driver frames after an
	// ownership violation would only grow the set of frames nobody can
	// account for, and the caller was promised a failed queue stays failed.
	if err := t.Dead(); err != nil {
		return 0, err
	}
	if len(descs) == 0 {
		return 0, nil
	}
	n := len(descs)
	if free := t.NumFreeSlots(); n > free {
		n = free
	}
	if n == 0 {
		t.Stats.RingFull.Add(1)
		return 0, nil
	}
	if n > len(t.mbufs) {
		n = len(t.mbufs)
	}

	built, bytes := 0, uint64(0)
	var refused error
	for i := 0; i < n; i++ {
		d := descs[i]
		if !t.descOK(d) {
			// Refuse the first bad descriptor and send the prefix before it,
			// so a short return with a free ring points at the offender.
			t.Stats.BadDesc.Add(1)
			refused = fmt.Errorf("%w: descriptor %d names %d bytes at offset %d, which is "+
				"not inside one frame's packet area", packetio.ErrBadLength, i, d.Len, d.Addr)
			break
		}
		frame := t.frameOf(d.Addr)
		m := int(frame) + t.l.ObjHeader
		mbuf.PrepareTx(t.region, m, t.cfg.PoolVA,
			uint16(d.Addr-frame-t.bufStart), d.Len)

		if offs != nil {
			if err := t.applyOffload(m, offs[i], t.region[d.Addr:d.Addr+uint64(d.Len)]); err != nil {
				t.Stats.BadOffload.Add(1)
				refused = fmt.Errorf("descriptor %d: %w", i, err)
				break
			}
		} else if t.cfg.Checksums {
			// Best effort by design: see Transmit. Parsing costs a handful of
			// loads over the first two cache lines of the packet, which are
			// already warm because the caller just wrote them.
			if h, ok := mbuf.Parse(t.region[d.Addr : d.Addr+uint64(d.Len)]); ok {
				mbuf.SetOlFlags(t.region, m, mbuf.ChecksumFlags(h))
				mbuf.SetTxOffload(t.region, m, uint64(h.L2Len), uint64(h.L3Len))
			}
		}

		t.mbufs[built] = t.cfg.RegionVA + uint64(m)
		built++
		bytes += uint64(d.Len)
	}
	if built == 0 {
		return 0, refused
	}

	sent := t.cfg.PMD.TxBurst(t.mbufs[:built])
	if sent < 0 || sent > built {
		sent = 0 // a driver that lies gets nothing recorded
	}
	for i := sent; i < built; i++ {
		bytes -= uint64(descs[i].Len)
	}
	t.inFlight.Add(int64(sent))
	t.Stats.Packets.Add(uint64(sent))
	t.Stats.Bytes.Add(bytes)
	t.Stats.Batches.Add(1)
	// The refusal is only the caller's business if everything before it was
	// taken: otherwise the short return already says where to look again.
	if sent == built {
		return sent, refused
	}
	return sent, nil
}

// NumFreeSlots is how many more frames the driver's ring can accept.
func (t *Tx) NumFreeSlots() int {
	if t.closed.Load() {
		return 0
	}
	if n := t.cfg.Depth - int(t.inFlight.Load()); n > 0 {
		return n
	}
	return 0
}

// NumInFlight is how many frames the driver currently owns.
func (t *Tx) NumInFlight() int { return int(t.inFlight.Load()) }

// NumCompleted is how many frames are waiting to be handed back right now,
// without asking the driver for more.
func (t *Tx) NumCompleted() int { return t.cfg.Returned.Len() }

// Complete returns up to max finished frames to this queue's free list and
// reports how many.
func (t *Tx) Complete(max int) int {
	n := 0
	t.reclaim(max, func(frame uint64) {
		t.pool.Push(frame)
		n++
	})
	return n
}

// Reclaim is Complete for frames that belong somewhere else: it hands them to
// the caller instead of returning them here.
//
// This is what a forwarder uses. The frame came from a receive queue's pool, so
// putting it on this queue's free list would starve that receive queue and
// alias the frame into the wrong pool -- the one bug in a forwarder that no
// counter on the wire shows.
func (t *Tx) Reclaim(max int, out []packetio.Desc) []packetio.Desc {
	t.reclaim(max, func(frame uint64) {
		out = append(out, packetio.Desc{Addr: frame + t.dataStart})
	})
	return out
}

// reclaim drains what the driver has freed, asking it once if there is nothing
// there and something is outstanding.
//
// The ask is the whole reason this is not a simple drain. A PMD only looks at
// its completions inside a transmit burst, so a queue that has just sent its
// last batch and stopped would wait forever for frames the hardware finished
// with microseconds ago. One call fixes that, and it happens only when the
// queue is otherwise idle -- a busy queue always finds the previous burst's
// completions already waiting and never pays for it.
func (t *Tx) reclaim(max int, give func(frame uint64)) {
	if t.closed.Load() || max <= 0 {
		return
	}
	if t.cfg.Returned.Len() == 0 && t.inFlight.Load() > 0 {
		t.cfg.PMD.Poke()
		t.Stats.Pokes.Add(1)
	}
	n := t.cfg.Returned.Len()
	if n > max {
		n = max
	}
	if n == 0 {
		return
	}
	t.addrs = t.cfg.Returned.Pop(n, t.addrs[:0])
	completed := 0
	for _, va := range t.addrs {
		off, ok := t.offsetOfVA(va)
		if !ok {
			// An address from outside this region: the driver freed something
			// that was never ours. Pushing it would corrupt the free list,
			// and the frame it displaced can never be found again -- inFlight
			// stays high by one forever. That is an ownership invariant gone,
			// so the queue is dead, not merely short a counter.
			t.Stats.Foreign.Add(1)
			t.fail(fmt.Errorf("%w: the driver freed address %#x, which is not in this queue's region",
				packetio.ErrQueueFailed, va))
			continue
		}
		completed++
		give(t.frameOf(uint64(off)))
	}
	if completed > 0 {
		t.inFlight.Add(-int64(completed))
		t.Stats.Completed.Add(uint64(completed))
	}
}

// Free returns frames to the free list without transmitting them.
func (t *Tx) Free(descs []packetio.Desc) {
	for _, d := range descs {
		t.pool.Push(t.pool.Base(d.Addr))
	}
}

// ----------------------------------------------------------------- receive

// RxStats counts what a receive queue has done. Atomic for the same reason
// as TxStats: read by a monitoring goroutine, added to once per batch.
type RxStats struct {
	Packets   atomic.Uint64
	Bytes     atomic.Uint64
	Filled    atomic.Uint64
	Batches   atomic.Uint64
	PoolEmpty atomic.Uint64
	Chained   atomic.Uint64 // packets spanning several mbufs, which this backend refuses
	BadLen    atomic.Uint64 // lengths the driver reported that do not fit their frame
	Foreign   atomic.Uint64
}

// Rx is one receive queue.
type Rx struct {
	base
	outstanding atomic.Int64
	Stats       RxStats
}

// NewRx builds a receive queue over one slice of the region.
func NewRx(cfg Config) (*Rx, error) {
	if cfg.Supply == nil {
		return nil, fmt.Errorf("dpdk: a receive queue with no supply ring")
	}
	q := &Rx{}
	if err := initBase(&q.base, cfg); err != nil {
		return nil, err
	}
	return q, nil
}

// Fill makes up to n frames available for the driver to receive into and
// reports how many it made available.
//
// This is where the DPDK receive model differs from every other backend's, and
// the difference is worth stating: nothing is posted to a ring here. The driver
// takes buffers from the supply when it wants them, in bulks it chooses, and a
// bulk it cannot satisfy in full is one it does not take at all. So a receiver
// should keep the supply comfortably full rather than topping it up a frame at
// a time.
func (q *Rx) Fill(n int) int {
	if q.closed.Load() || n <= 0 || q.dead.Load() != nil {
		return 0
	}
	// Deliberately not clamped against the supply's room here. The ring is the
	// one authority on what it will take, and duplicating that test above it
	// made the path below -- giving back what it refused -- unreachable, which
	// is to say untestable. Asking for more than fits costs a few pool pushes
	// and only happens when a caller ignores NumFreeFillSlots.
	if have := q.pool.Len(); n > have {
		if have == 0 {
			q.Stats.PoolEmpty.Add(1)
			return 0
		}
		n = have
	}
	if n == 0 {
		return 0
	}

	q.addrs = q.pool.Pop(n, q.addrs[:0])
	q.mbufs = q.mbufs[:0]
	for _, frame := range q.addrs {
		m := int(frame) + q.l.ObjHeader
		mbuf.ResetRx(q.region, m, uint16(q.l.Headroom), q.cfg.PoolVA)
		q.mbufs = append(q.mbufs, q.cfg.RegionVA+uint64(m))
	}
	posted := q.cfg.Supply.Push(q.mbufs)
	// Anything the supply would not take goes straight back, rather than being
	// held by nobody.
	for _, frame := range q.addrs[posted:] {
		q.pool.Push(frame)
	}
	q.outstanding.Add(int64(posted))
	q.Stats.Filled.Add(uint64(posted))
	return posted
}

// NumFreeFillSlots is how many more frames the supply will accept.
func (q *Rx) NumFreeFillSlots() int {
	if q.closed.Load() {
		return 0
	}
	room := q.cfg.Supply.Room()
	if have := q.pool.Len(); room > have {
		return have
	}
	return room
}

// NumOutstanding is how many frames the driver currently holds.
func (q *Rx) NumOutstanding() int { return int(q.outstanding.Load()) }

// Receive takes up to max received packets. The frames belong to the caller
// until Recycle.
//
// A packet spanning more than one mbuf is refused rather than delivered: this
// backend keeps the MTU inside one frame precisely so that cannot happen, and a
// chained packet would mean the queue was configured differently from what this
// code assumes. Its frames go straight back.
func (q *Rx) Receive(max int) []packetio.Desc {
	if q.closed.Load() || max <= 0 || q.dead.Load() != nil {
		return nil
	}
	if max > len(q.burst) {
		max = len(q.burst)
	}
	got := q.cfg.PMD.RxBurst(q.burst[:max])
	if got <= 0 || got > max {
		return nil
	}

	q.descs = q.descs[:0]
	bytes, taken := uint64(0), 0
	for _, va := range q.burst[:got] {
		off, ok := q.offsetOfVA(va)
		if !ok {
			// A received buffer from outside the region: a frame this queue
			// gave the driver has been displaced and cannot be recovered, so
			// ownership is no longer provable. See reclaim.
			q.Stats.Foreign.Add(1)
			q.fail(fmt.Errorf("%w: the driver delivered address %#x, which is not in this queue's region",
				packetio.ErrQueueFailed, va))
			continue
		}
		taken++
		frame := q.frameOf(uint64(off))
		m := int(frame) + q.l.ObjHeader
		dataOff, nbSegs, dataLen, flags := mbuf.RxRead(q.region, m)
		if nbSegs != 1 {
			q.Stats.Chained.Add(1)
			q.pool.Push(frame)
			continue
		}
		d := packetio.Desc{
			Addr: frame + q.bufStart + uint64(dataOff),
			Len:  uint32(dataLen),
		}
		// A length the driver reports that does not fit the frame means the
		// queue and this code disagree about the buffer size. Believing it
		// would hand the caller a descriptor running past its frame, and on a
		// forwarding path straight back to a transmit queue as an instruction
		// to put the neighbouring frames on the wire.
		if !q.descOK(d) {
			q.Stats.BadLen.Add(1)
			q.pool.Push(frame)
			continue
		}
		if flags&mbuf.RxIPChecksumGood != 0 {
			d.Options |= packetio.OptL3ChecksumOK
			if flags&mbuf.RxL4ChecksumGood != 0 {
				d.Options |= packetio.OptChecksumOK
			}
		}
		q.descs = append(q.descs, d)
		bytes += uint64(d.Len)
	}
	if taken > 0 {
		q.outstanding.Add(-int64(taken))
	}
	if len(q.descs) > 0 {
		// Counted after the filtering, not before: a burst whose every mbuf
		// was refused delivered no packets, and a batch that delivered none
		// would make packets-per-batch smaller than it really is -- and can
		// make batches exceed packets, which the contract forbids. Packets
		// and bytes were summed locally in the loop for the same reason the
		// batch is: one atomic add per burst, not one per packet.
		q.Stats.Packets.Add(uint64(len(q.descs)))
		q.Stats.Bytes.Add(bytes)
		q.Stats.Batches.Add(1)
	}
	return q.descs
}

// Recycle returns received frames to the free list.
func (q *Rx) Recycle(descs []packetio.Desc) {
	for _, d := range descs {
		q.pool.Push(q.pool.Base(d.Addr))
	}
}

// DrainReturned takes back frames the driver freed on this queue's pool rather
// than delivering them, which is what happens to the buffers still in its ring
// when a port is stopped. It reports how many came back.
func (q *Rx) DrainReturned() int {
	if q.cfg.Returned == nil {
		return 0
	}
	n := q.cfg.Returned.Len()
	if n == 0 {
		return 0
	}
	q.addrs = q.cfg.Returned.Pop(n, q.addrs[:0])
	back := 0
	for _, va := range q.addrs {
		off, ok := q.offsetOfVA(va)
		if !ok {
			// See Receive: an address from outside the region means a frame
			// is unaccounted for, and the queue cannot be trusted again.
			q.Stats.Foreign.Add(1)
			q.fail(fmt.Errorf("%w: the driver returned address %#x, which is not in this queue's region",
				packetio.ErrQueueFailed, va))
			continue
		}
		q.pool.Push(q.frameOf(uint64(off)))
		back++
	}
	q.outstanding.Add(-int64(back))
	return back
}
