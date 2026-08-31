package queue

import (
	"testing"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/dpdk/internal/mbuf"
)

const (
	poolA = 0xaaaa0000
	poolB = 0xbbbb0000
)

// A descriptor from Alloc must name the packet area, past the mempool header,
// the mbuf and the headroom -- and Region.Writable on top of it must not be able
// to reach either the mbuf or the next frame.
func TestAllocReservesTheHeadroom(t *testing.T) {
	r := newRig(t, 64)
	q := r.tx(t, poolA, 0, 64, 32)

	descs := q.Alloc(4)
	if len(descs) != 4 {
		t.Fatalf("Alloc(4) returned %d", len(descs))
	}
	for i, d := range descs {
		frame := d.Addr &^ uint64(r.l.FrameSize-1)
		if got, want := d.Addr-frame, uint64(r.l.DataStart()); got != want {
			t.Errorf("descriptor %d starts %d into its frame, want %d", i, got, want)
		}
		if d.Len != 0 {
			t.Errorf("descriptor %d has Len %d, want 0", i, d.Len)
		}
		// The bytes before it are the mbuf, and must stay untouched.
		if d.Addr-frame < uint64(r.l.ObjHeader+r.l.MbufSize) {
			t.Errorf("descriptor %d overlaps its own mbuf", i)
		}
	}
	// Distinct frames, every time.
	seen := map[uint64]bool{}
	for _, d := range descs {
		if seen[d.Addr] {
			t.Fatalf("frame %d handed out twice", d.Addr)
		}
		seen[d.Addr] = true
	}
}

// The rule from BACKENDS.md, plus the one this backend adds: a descriptor may
// not reach into the mempool header or the mbuf that sit at the start of its
// own frame.
func TestTransmitRefusesDescriptorsOutsideThePacketArea(t *testing.T) {
	r := newRig(t, 64)
	q := r.tx(t, poolA, 0, 64, 32)
	frameSize := uint64(r.l.FrameSize)
	good := q.Alloc(1)[0]
	q.Free([]packetio.Desc{good})

	for _, c := range []struct {
		name string
		d    packetio.Desc
	}{
		{"past the region", packetio.Desc{Addr: uint64(len(r.region)), Len: 64}},
		{"address wraps", packetio.Desc{Addr: ^uint64(0), Len: 64}},
		{"past its own frame", packetio.Desc{Addr: good.Addr, Len: uint32(frameSize)}},
		{"one byte past its frame", packetio.Desc{Addr: good.Addr,
			Len: uint32(frameSize-(good.Addr%frameSize)) + 1}},
		{"zero length", packetio.Desc{Addr: good.Addr, Len: 0}},
		{"into the mbuf", packetio.Desc{Addr: good.Addr - uint64(r.l.Headroom) - 8, Len: 64}},
		{"at the frame start", packetio.Desc{Addr: good.Addr &^ (frameSize - 1), Len: 64}},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := q.NumFreeFrames()
			if n := q.Transmit([]packetio.Desc{c.d}); n != 0 {
				t.Errorf("Transmit took %d of a descriptor that is not in a packet area", n)
			}
			if got := q.NumFreeFrames(); got > before {
				t.Errorf("a refused descriptor reached the pool: %d free, want at most %d", got, before)
			}
		})
	}
	// The headroom is legal: a forwarder may push a header in front of the
	// packet it received, and that is what the headroom is for.
	inHeadroom := packetio.Desc{Addr: good.Addr - 4, Len: 64}
	if n := q.Transmit([]packetio.Desc{inHeadroom}); n != 1 {
		t.Error("Transmit refused a descriptor starting in the headroom")
	}

	// The exact last byte of the frame is legal, and is the boundary the
	// two-comparison check exists to get right.
	last := packetio.Desc{Addr: good.Addr, Len: uint32(frameSize - (good.Addr % frameSize))}
	if n := q.Transmit([]packetio.Desc{last}); n != 1 {
		t.Errorf("Transmit refused a descriptor ending exactly at the frame end")
	}
}

// Transmit takes a prefix. What it did not take is still the caller's, and a
// backend that quietly freed the suffix would cause a double return the caller
// cannot see.
func TestTransmitTakesAPrefixAndLeavesTheRest(t *testing.T) {
	r := newRig(t, 64)
	q := r.tx(t, poolA, 0, 64, 32)
	r.pmd.accept = 3 // the driver will take no more than three at a time

	before := q.NumFreeFrames()
	descs := append([]packetio.Desc(nil), q.Alloc(8)...)
	for i := range descs {
		descs[i].Len = 64
	}
	sent := q.Transmit(descs)
	if sent != 3 {
		t.Fatalf("Transmit took %d, want the driver's prefix of 3", sent)
	}
	// The suffix must still be ours: handing it back must be accepted.
	q.Free(descs[sent:])
	if got, want := q.NumFreeFrames(), before-sent; got != want {
		t.Errorf("%d frames free, want %d: the suffix was not left with the caller", got, want)
	}
	if got := q.NumInFlight(); got != sent {
		t.Errorf("%d in flight, want %d", got, sent)
	}
}

// A descriptor in the middle that no backend may accept ends the batch there.
func TestTransmitStopsAtTheFirstBadDescriptor(t *testing.T) {
	r := newRig(t, 64)
	q := r.tx(t, poolA, 0, 64, 32)

	descs := append([]packetio.Desc(nil), q.Alloc(8)...)
	for i := range descs {
		descs[i].Len = 64
	}
	kept := append([]packetio.Desc(nil), descs...)
	descs[4] = packetio.Desc{Addr: ^uint64(0), Len: 64}

	sent := q.Transmit(descs)
	if sent != 4 {
		t.Fatalf("Transmit took %d, want the 4 before the bad descriptor", sent)
	}
	if q.Stats.BadDesc.Load() != 1 {
		t.Errorf("BadDesc = %d, want 1", q.Stats.BadDesc.Load())
	}
	q.Free(kept[sent:])
}

// The completion behaviour DPDK actually has: nothing comes back until the
// driver has seen enough traffic, and a poke only helps once it has.
func TestCompletionsNeedTheDriverToLook(t *testing.T) {
	r := newRig(t, 256)
	q := r.tx(t, poolA, 0, 256, 256)
	r.pmd.compThresh = 32

	send := func(n int) int {
		descs := append([]packetio.Desc(nil), q.Alloc(n)...)
		for i := range descs {
			descs[i].Len = 64
		}
		sent := q.Transmit(descs)
		q.Free(descs[sent:])
		return sent
	}

	// A batch below the threshold: the driver never looks, so nothing comes
	// back, poke or no poke. This is measured behaviour, not a guess.
	sent := send(4)
	if got := q.Complete(1024); got != 0 {
		t.Errorf("a %d-packet batch returned %d frames; below the threshold none can", sent, got)
	}
	if q.Stats.Pokes.Load() == 0 {
		t.Error("Complete did not poke a driver holding frames")
	}
	if got := q.NumInFlight(); got != sent {
		t.Errorf("%d in flight after a short batch, want %d", got, sent)
	}

	// Enough more, and the poke frees the lot.
	send(64)
	if got := q.Complete(1024); got != 68 {
		t.Errorf("Complete returned %d frames, want all 68", got)
	}
	if got := q.NumInFlight(); got != 0 {
		t.Errorf("%d still in flight", got)
	}
}

// A busy queue must not pay for the poke: the previous burst's completions are
// already waiting.
func TestCompleteDoesNotPokeWhenFramesAreWaiting(t *testing.T) {
	r := newRig(t, 256)
	q := r.tx(t, poolA, 0, 256, 256)
	r.pmd.compThresh = 1

	send := func(n int) {
		descs := append([]packetio.Desc(nil), q.Alloc(n)...)
		for i := range descs {
			descs[i].Len = 64
		}
		sent := q.Transmit(descs)
		q.Free(descs[sent:])
	}
	send(8)
	// The driver reads its completions at the top of a burst, so the second
	// batch is what makes the first one's frames available. An empty burst
	// would not do it: mlx5 returns before its completion handler when it is
	// given no packets, which the M0 spike measured.
	send(8)
	before := q.Stats.Pokes.Load()
	if got := q.Complete(1024); got == 0 {
		t.Fatal("nothing came back although the driver had freed it")
	}
	if q.Stats.Pokes.Load() != before {
		t.Error("Complete poked the driver although frames were already waiting")
	}
}

// The forwarding cycle, and the reason Reclaim exists: a frame received on one
// queue and transmitted on another must come back to the queue it came from,
// not to the one that sent it.
func TestForwardedFramesGoHomeNotToTheSender(t *testing.T) {
	r := newRig(t, 256)
	rx := r.rx(t, poolA, 0, 128, 128)
	tx := r.tx(t, poolB, 128, 128, 128)
	r.pmd.compThresh = 1

	rxFreeAtRest := rx.NumFreeFrames()
	txFreeAtRest := tx.NumFreeFrames()

	rx.Fill(64)
	pkt := make([]byte, 64)
	for i := range pkt {
		pkt[i] = byte(i)
	}
	for i := 0; i < 16; i++ {
		r.pmd.deliver(pkt)
	}

	got := rx.Receive(16)
	if len(got) != 16 {
		t.Fatalf("received %d packets, want 16", len(got))
	}
	// Forward them out of the other queue, in the frames they arrived in.
	sent := tx.Transmit(got)
	if sent != 16 {
		t.Fatalf("forwarded %d of 16", sent)
	}
	// Reclaim pokes the driver, which is what makes a queue that has stopped
	// sending get its last frames back at all.

	// The frames must come back through the transmitting queue's Reclaim and
	// go home to the receive queue. Complete here would put them on the
	// transmit pool, where the receive queue could never find them.
	back := tx.Reclaim(1024, nil)
	if len(back) != 16 {
		t.Fatalf("Reclaim handed back %d frames, want 16", len(back))
	}
	if got := tx.NumFreeFrames(); got != txFreeAtRest {
		t.Errorf("the transmit pool grew to %d from %d: it kept frames that were not its own",
			got, txFreeAtRest)
	}
	rx.Recycle(back)
	if got := rx.NumFreeFrames(); got != rxFreeAtRest-64+16 {
		t.Errorf("receive pool has %d frames, want %d", got, rxFreeAtRest-64+16)
	}
	// And nothing was refused along the way, which is what says every frame
	// went back to the pool that owns it.
	if n := rx.PoolRejected() + tx.PoolRejected(); n != 0 {
		t.Errorf("%d frames were refused by a pool: they went to the wrong queue", n)
	}
}

// One leaked frame per round is invisible in a single round. Ten thousand
// rounds of the whole cycle are not.
func TestFramesAreConservedOverManyCycles(t *testing.T) {
	r := newRig(t, 256)
	rx := r.rx(t, poolA, 0, 128, 128)
	tx := r.tx(t, poolB, 128, 128, 128)
	r.pmd.compThresh = 1

	rxTotal := rx.NumFreeFrames()
	txTotal := tx.NumFreeFrames()
	pkt := make([]byte, 64)

	var back []packetio.Desc
	for round := 0; round < 10000; round++ {
		rx.Fill(rx.NumFreeFillSlots())
		r.pmd.deliver(pkt, pkt, pkt, pkt)
		descs := rx.Receive(4)
		sent := tx.Transmit(descs)
		rx.Recycle(descs[sent:])
		back = tx.Reclaim(1024, back[:0])
		rx.Recycle(back)
	}
	// Drain everything still with the driver, then count.
	for i := 0; i < 100 && tx.NumInFlight() > 0; i++ {
		back = tx.Reclaim(1024, back[:0])
		rx.Recycle(back)
	}

	rxHeld := rx.NumFreeFrames() + rx.NumOutstanding()
	if rxHeld != rxTotal {
		t.Errorf("receive queue accounts for %d of its %d frames after 10000 cycles",
			rxHeld, rxTotal)
	}
	if got := tx.NumFreeFrames(); got != txTotal {
		t.Errorf("transmit queue has %d of its %d frames; it never allocated any",
			got, txTotal)
	}
	if n := rx.PoolRejected() + tx.PoolRejected(); n != 0 {
		t.Errorf("%d pool rejections over 10000 cycles", n)
	}
}

// The driver takes receive buffers in all-or-nothing bulks. A supply holding
// fewer than a bulk yields nothing at all, which is why Fill posts generously.
func TestSupplyIsAllOrNothing(t *testing.T) {
	r := newRig(t, 64)
	q := r.rx(t, poolA, 0, 64, 64)
	r.pmd.bulk = 8

	if n := q.Fill(3); n != 3 {
		t.Fatalf("Fill(3) posted %d", n)
	}
	pkt := make([]byte, 64)
	r.pmd.deliver(pkt, pkt)
	if got := q.Receive(2); len(got) != 0 {
		t.Errorf("received %d packets from a supply of 3 with a bulk of 8", len(got))
	}
	if r.pmd.noBuf == 0 {
		t.Error("the driver did not report running short of buffers")
	}

	// Top it up past a bulk and the same packets arrive.
	q.Fill(16)
	if got := q.Receive(2); len(got) != 2 {
		t.Errorf("received %d packets once the supply held a whole bulk", len(got))
	}
}

// Fill takes frames off the free list before it knows the supply will accept
// them. Whatever the supply refuses has to go straight back, or those frames
// belong to nobody: not the pool, not the driver, not the caller. A leak of one
// frame per Fill empties a queue over a long run and looks like a slow stall.
func TestFillReturnsWhatTheSupplyRefused(t *testing.T) {
	r := newRig(t, 128)
	q := r.rxSupply(t, poolA, 0, 128, 128, 8) // a supply of only 8
	total := q.NumFreeFrames()

	posted := q.Fill(128)
	if posted != 8 {
		t.Fatalf("Fill posted %d into a supply of 8", posted)
	}
	if got, want := q.NumFreeFrames(), total-posted; got != want {
		t.Errorf("%d frames on the free list, want %d: %d went nowhere",
			got, want, want-got)
	}
	if got := q.NumFreeFrames() + q.NumOutstanding(); got != total {
		t.Errorf("%d of %d frames accounted for after Fill", got, total)
	}

	// And again, so a leak of one per call would show.
	for i := 0; i < 50; i++ {
		q.Fill(128)
	}
	if got := q.NumFreeFrames() + q.NumOutstanding(); got != total {
		t.Errorf("%d of %d frames accounted for after 51 Fills", got, total)
	}
}

// Receive must report the packet the driver delivered, and turn what the NIC
// said about its checksums into packetio's own flags.
func TestReceiveReportsLengthAndChecksums(t *testing.T) {
	r := newRig(t, 64)
	q := r.rx(t, poolA, 0, 64, 64)
	q.Fill(32)

	pkt := make([]byte, 100)
	for i := range pkt {
		pkt[i] = byte(i)
	}
	r.pmd.deliver(pkt)
	got := q.Receive(4)
	if len(got) != 1 {
		t.Fatalf("received %d packets", len(got))
	}
	d := got[0]
	if d.Len != 100 {
		t.Errorf("Len = %d, want 100", d.Len)
	}
	// The descriptor must name the bytes the driver actually wrote.
	if b := r.region[d.Addr : d.Addr+uint64(d.Len)]; b[0] != 0 || b[99] != 99 {
		t.Errorf("the descriptor does not name the packet: % x", b[:8])
	}
	if d.Options != 0 {
		t.Errorf("Options = %#x with no checksum reported, want 0", d.Options)
	}
	if q.Stats.Bytes.Load() != 100 || q.Stats.Packets.Load() != 1 {
		t.Errorf("counted %d packets and %d bytes", q.Stats.Packets.Load(), q.Stats.Bytes.Load())
	}
	q.Recycle(got)

	// The two cases packetio distinguishes: a good IP header on its own, and a
	// good header with a good payload. A forwarder trusts the first and skips
	// verifying the header; only the second says anything about the payload.
	for _, c := range []struct {
		name  string
		flags uint64
		want  uint32
	}{
		{"header only", mbuf.RxIPChecksumGood, packetio.OptL3ChecksumOK},
		{"header and payload", mbuf.RxIPChecksumGood | mbuf.RxL4ChecksumGood,
			packetio.OptL3ChecksumOK | packetio.OptChecksumOK},
		{"payload without header", mbuf.RxL4ChecksumGood, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			q.Fill(q.NumFreeFillSlots())
			r.pmd.rxOlFlags = c.flags
			r.pmd.deliver(pkt)
			got := q.Receive(4)
			if len(got) != 1 {
				t.Fatalf("received %d packets", len(got))
			}
			if got[0].Options != c.want {
				t.Errorf("Options = %#x, want %#x", got[0].Options, c.want)
			}
			q.Recycle(got)
		})
	}
}

// A packet the driver reports as spanning several mbufs is refused, and its
// frame goes straight back rather than being delivered as a truncated one.
func TestChainedPacketsAreRefused(t *testing.T) {
	r := newRig(t, 64)
	q := r.rx(t, poolA, 0, 64, 64)
	q.Fill(32)
	free := q.NumFreeFrames()

	pkt := make([]byte, 64)
	r.pmd.deliver(pkt)
	// Deliver it, then make it look chained before the queue reads it: the
	// fake driver writes nb_segs when it fills the buffer, so reach in.
	out := make([]uint64, 1)
	if n := r.pmd.RxBurst(out); n != 1 {
		t.Fatal("the driver delivered nothing")
	}
	m := int(out[0] - r.regionVA)
	mbuf.PrepareTx(r.region, m, mbuf.Pool(r.region, m), uint16(r.l.Headroom), 64)
	setNbSegs(r.region, m, 2)
	r.pmd.pending = nil
	// Hand it to the queue by putting it back where a burst would find it.
	r.pmd.held = append(r.pmd.held, out[0])
	r.pmd.deliver(pkt)

	// The chained frame is the one already filled; receive it directly.
	q.cfg.PMD = &oneShotPMD{va: out[0]}
	got := q.Receive(4)
	if len(got) != 0 {
		t.Errorf("a chained packet was delivered as %d descriptors", len(got))
	}
	if q.Stats.Chained.Load() != 1 {
		t.Errorf("Chained = %d, want 1", q.Stats.Chained.Load())
	}
	if q.NumFreeFrames() != free+1 {
		t.Errorf("the chained packet's frame did not go back to the pool")
	}
}

// oneShotPMD hands over one prepared mbuf and then nothing.
type oneShotPMD struct {
	va   uint64
	done bool
}

func (p *oneShotPMD) TxBurst(m []uint64) int { return 0 }
func (p *oneShotPMD) Poke()                  {}
func (p *oneShotPMD) RxBurst(out []uint64) int {
	if p.done || len(out) == 0 {
		return 0
	}
	p.done = true
	out[0] = p.va
	return 1
}

func setNbSegs(b []byte, m int, n uint16) {
	b[m+20] = byte(n)
	b[m+21] = byte(n >> 8)
}

// An address the driver returns that this region does not contain must be
// counted and dropped, never pushed onto a free list.
func TestForeignAddressesAreRefused(t *testing.T) {
	r := newRig(t, 64)
	q := r.tx(t, poolA, 0, 64, 64)
	before := q.NumFreeFrames()

	q.cfg.Returned.PushOne(0x11)                 // far below the region
	q.cfg.Returned.PushOne(testRegionVA + 1<<40) // far above it
	q.cfg.Returned.PushOne(testRegionVA - 1)     // one byte below
	if got := q.Complete(1024); got != 0 {
		t.Errorf("Complete returned %d foreign frames", got)
	}
	if q.Stats.Foreign.Load() != 3 {
		t.Errorf("Foreign = %d, want 3", q.Stats.Foreign.Load())
	}
	if q.NumFreeFrames() != before {
		t.Errorf("a foreign address reached the pool")
	}
}

// Every method after Close must be safe and do nothing.
func TestClosedQueueDoesNothing(t *testing.T) {
	r := newRig(t, 64)
	tx := r.tx(t, poolA, 0, 32, 32)
	rx := r.rx(t, poolB, 32, 32, 32)

	descs := append([]packetio.Desc(nil), tx.Alloc(4)...)
	for i := range descs {
		descs[i].Len = 64
	}
	tx.Close()
	rx.Close()

	if got := tx.Alloc(4); got != nil {
		t.Errorf("Alloc after Close returned %d descriptors", len(got))
	}
	if n := tx.Transmit(descs); n != 0 {
		t.Errorf("Transmit after Close sent %d", n)
	}
	if n := tx.Complete(16); n != 0 {
		t.Errorf("Complete after Close returned %d", n)
	}
	if got := tx.Reclaim(16, nil); len(got) != 0 {
		t.Errorf("Reclaim after Close returned %d", len(got))
	}
	if n := tx.NumFreeSlots(); n != 0 {
		t.Errorf("NumFreeSlots after Close is %d", n)
	}
	if n := rx.Fill(4); n != 0 {
		t.Errorf("Fill after Close posted %d", n)
	}
	if got := rx.Receive(4); len(got) != 0 {
		t.Errorf("Receive after Close returned %d", len(got))
	}
	if n := rx.NumFreeFillSlots(); n != 0 {
		t.Errorf("NumFreeFillSlots after Close is %d", n)
	}
}

// The config is checked at build time so the packet path need not re-check.
func TestConfigIsChecked(t *testing.T) {
	r := newRig(t, 16)
	good := Config{
		Region: r.region, RegionVA: r.regionVA, Layout: r.l, PoolVA: poolA,
		FirstFrame: 0, Frames: 16, Depth: 16,
		Returned: mustRing(t, 16), PMD: r.pmd,
	}
	if _, err := NewTx(good); err != nil {
		t.Fatalf("a good config was refused: %v", err)
	}
	for _, c := range []struct {
		name  string
		munge func(*Config)
	}{
		{"no region", func(c *Config) { c.Region = nil }},
		{"a region that is not whole frames", func(c *Config) { c.Region = r.region[:100] }},
		{"no frames", func(c *Config) { c.Frames = 0 }},
		{"frames past the region", func(c *Config) { c.FirstFrame, c.Frames = 8, 16 }},
		{"no depth", func(c *Config) { c.Depth = 0 }},
		{"no returned ring", func(c *Config) { c.Returned = nil }},
		{"no driver", func(c *Config) { c.PMD = nil }},
		{"a frame size that is not a power of two", func(c *Config) { c.Layout.FrameSize = 1000 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := good
			c.munge(&cfg)
			if _, err := NewTx(cfg); err == nil {
				t.Error("accepted")
			}
		})
	}
	t.Run("a receive queue with no supply", func(t *testing.T) {
		cfg := good
		if _, err := NewRx(cfg); err == nil {
			t.Error("accepted")
		}
	})
}

func mustRing(t *testing.T, n int) *Ring {
	t.Helper()
	r, err := NewRing(n)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Counters are what a caller sees when packets go missing, so they have to add
// up rather than approximately add up.
func TestCountersAddUp(t *testing.T) {
	r := newRig(t, 128)
	q := r.tx(t, poolA, 0, 128, 128)
	r.pmd.compThresh = 1

	descs := append([]packetio.Desc(nil), q.Alloc(10)...)
	for i := range descs {
		descs[i].Len = 100
	}
	sent := q.Transmit(descs)
	q.Free(descs[sent:])
	if q.Stats.Packets.Load() != uint64(sent) {
		t.Errorf("Packets = %d, want %d", q.Stats.Packets.Load(), sent)
	}
	if want := uint64(sent) * 100; q.Stats.Bytes.Load() != want {
		t.Errorf("Bytes = %d, want %d", q.Stats.Bytes.Load(), want)
	}
	if q.Stats.Batches.Load() != 1 {
		t.Errorf("Batches = %d, want 1", q.Stats.Batches.Load())
	}
	back := q.Complete(1024)
	if q.Stats.Completed.Load() != uint64(back) {
		t.Errorf("Completed = %d, want %d", q.Stats.Completed.Load(), back)
	}
}

// Bytes must count only what the driver took, not what was offered.
func TestBytesCountOnlyWhatWentOut(t *testing.T) {
	r := newRig(t, 64)
	q := r.tx(t, poolA, 0, 64, 32)
	r.pmd.accept = 2

	descs := append([]packetio.Desc(nil), q.Alloc(6)...)
	for i := range descs {
		descs[i].Len = 50
	}
	sent := q.Transmit(descs)
	q.Free(descs[sent:])
	if sent != 2 {
		t.Fatalf("sent %d, want 2", sent)
	}
	if q.Stats.Bytes.Load() != 100 {
		t.Errorf("Bytes = %d, want 100 for the two the driver took", q.Stats.Bytes.Load())
	}
}

// A ring is a power of two and its bulk semantics are exact.
func TestRing(t *testing.T) {
	if _, err := NewRing(3); err == nil {
		t.Error("a ring of 3 was accepted")
	}
	if _, err := NewRing(0); err == nil {
		t.Error("a ring of 0 was accepted")
	}
	r := mustRing(t, 4)
	if r.Cap() != 4 || r.Len() != 0 || r.Room() != 4 {
		t.Fatalf("a fresh ring reports cap %d len %d room %d", r.Cap(), r.Len(), r.Room())
	}
	if n := r.Push([]uint64{1, 2, 3, 4, 5, 6}); n != 4 {
		t.Errorf("Push took %d into a ring of 4", n)
	}
	if r.Room() != 0 {
		t.Errorf("room %d in a full ring", r.Room())
	}
	if r.PushOne(9) {
		t.Error("PushOne succeeded on a full ring")
	}
	dst := make([]uint64, 4)
	if !r.PopBulk(dst, 4) || dst[0] != 1 || dst[3] != 4 {
		t.Errorf("PopBulk gave %v", dst)
	}
	if r.PopBulk(dst, 1) {
		t.Error("PopBulk succeeded on an empty ring")
	}
	// It wraps: indices are masked, and the ring is reusable for ever.
	for i := 0; i < 100; i++ {
		if !r.PushOne(uint64(i)) {
			t.Fatalf("PushOne failed at %d", i)
		}
		got := r.Pop(1, nil)
		if len(got) != 1 || got[0] != uint64(i) {
			t.Fatalf("round %d gave %v", i, got)
		}
	}
}

// The burst must never be capped by what the last Fill happened to post. It
// was, for a while: Fill and Receive shared a scratch slice, Fill truncated
// it to what it posted, and Receive clamped its burst to that length -- so a
// five-frame top-up capped every burst after it at five packets. This is the
// test that failure mode fails.
func TestReceiveBurstNotCappedByLastFill(t *testing.T) {
	r := newRig(t, 256)
	rx := r.rx(t, poolA, 0, 256, 128)

	// Fill generously, deliver plenty, then top up by a tiny amount so the
	// last Fill posts a small number.
	rx.Fill(128)
	pkt := make([]byte, 60)
	for i := 0; i < 100; i++ {
		r.pmd.deliver(pkt)
	}
	got := rx.Receive(64)
	if len(got) != 64 {
		t.Fatalf("first burst took %d, want 64", len(got))
	}
	rx.Recycle(got)
	small := rx.Fill(3) // a tiny top-up, as a loop under a full supply does
	if small == 0 {
		t.Fatal("the tiny fill posted nothing; the test needs it to post a little")
	}
	got = rx.Receive(64)
	if len(got) <= small {
		t.Fatalf("burst after a %d-frame fill took %d packets; the burst is "+
			"being capped by the last fill", small, len(got))
	}
	rx.Recycle(got)
}

// A burst in which every mbuf is refused delivered no packets, so it is not a
// batch. Counting it would make packets-per-batch read lower than it is, and
// could make batches exceed packets, which the contract forbids and the
// conformance suite checks.
func TestRejectedBurstIsNotCountedAsABatch(t *testing.T) {
	r := newRig(t, 64)
	rx := r.rx(t, poolA, 0, 64, 32)
	rx.Fill(32)

	// Chained packets are refused by this backend; deliver only those.
	r.pmd.chained = true
	pkt := make([]byte, 60)
	r.pmd.deliver(pkt, pkt, pkt)

	if got := rx.Receive(64); len(got) != 0 {
		t.Fatalf("received %d packets, want none: they were all chained", len(got))
	}
	if b := rx.Stats.Batches.Load(); b != 0 {
		t.Errorf("Batches = %d after a burst that delivered nothing, want 0", b)
	}
	if c := rx.Stats.Chained.Load(); c == 0 {
		t.Error("the refusal was not counted as chained")
	}
}
