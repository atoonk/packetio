package ring

import (
	"bytes"
	"fmt"
	"testing"
	"unsafe"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/mlx5/internal/mocknic"
	"github.com/atoonk/packetio/mlx5/internal/wqe"
)

// rig is a send ring wired to the software NIC over shared queue memory, which
// is how the hardware is arranged: the two agree on nothing but these bytes.
type rig struct {
	t         *testing.T
	tx        *Tx
	nic       *mocknic.TxNIC
	region    []byte
	frameSize int
	frames    int
	regionVA  uint64

	// held tracks which frames the NIC owns, so a frame handed out twice or
	// returned twice is caught the moment it happens.
	held map[uint64]int
}

type rigOpts struct {
	blocks      int // send queue size, in work queue buffer blocks
	cqEntries   int
	frames      int
	frameSize   int
	inlineLen   int
	signalEvery int
	multiPacket bool
	mpwPointer  bool
	mpwMaxLen   int
}

func newRig(t *testing.T, o rigOpts) *rig {
	t.Helper()
	if o.blocks == 0 {
		o.blocks = 16
	}
	if o.cqEntries == 0 {
		o.cqEntries = o.blocks
	}
	if o.frames == 0 {
		o.frames = 64
	}
	if o.frameSize == 0 {
		o.frameSize = 2048
	}
	if o.inlineLen == 0 && !o.multiPacket {
		o.inlineLen = wqe.EthL2InlineHeaderSize
	}

	r := &rig{
		t:         t,
		frameSize: o.frameSize,
		frames:    o.frames,
		region:    make([]byte, o.frames*o.frameSize),
		regionVA:  0x7f0000000000,
		held:      map[uint64]int{},
	}

	sq := make([]byte, o.blocks*wqe.WQEBB)
	cq := make([]byte, o.cqEntries*wqe.CQESize)
	dbrec := new([2]uint32)
	cqDbrec := new(uint32)
	uar := new(uint64)

	nic, err := mocknic.NewTx(mocknic.TxConfig{
		SQ: sq, CQ: cq,
		SQDbrec: &dbrec[wqe.SndDBR], CQDbrec: cqDbrec, UAR: uar,
		Region: r.region, RegionVA: r.regionVA,
		LKey: 0x1234, QPN: 0x42,
	})
	if err != nil {
		t.Fatalf("mocknic.NewTx: %v", err)
	}
	r.nic = nic

	tx, err := NewTx(TxConfig{
		SQ: sq, CQ: cq,
		SQDbrec: &dbrec[wqe.SndDBR], CQDbrec: cqDbrec, UAR: unsafe.Pointer(uar),
		QPN: 0x42, LKey: 0x1234,
		Region: r.region, RegionVA: r.regionVA, FrameSize: o.frameSize,
		InlineLen: o.inlineLen, SignalEvery: o.signalEvery,
		MultiPacket: o.multiPacket, MultiPacketMaxLen: o.mpwMaxLen,
		MultiPacketPointer: o.mpwPointer,
	})
	if err != nil {
		t.Fatalf("NewTx: %v", err)
	}
	r.tx = tx
	return r
}

// desc builds a descriptor for frame i and fills it with a recognisable
// pattern, so a packet that comes out of the model can be traced to the frame
// it was built in.
func (r *rig) desc(frame int, length int) packetio.Desc {
	addr := uint64(frame * r.frameSize)
	b := r.region[addr : addr+uint64(length)]
	for i := range b {
		b[i] = byte(frame)
	}
	// A recognisable header so the inline and pointed-at halves can be told
	// apart if they are ever assembled in the wrong order.
	copy(b, fmt.Sprintf("frame%03d|", frame))
	return packetio.Desc{Addr: addr, Len: uint32(length)}
}

// post hands descriptors to the ring, recording which frames the NIC now owns.
func (r *rig) post(descs []packetio.Desc) int {
	r.t.Helper()
	n := r.tx.Post(descs)
	r.tx.Sync()
	for _, d := range descs[:n] {
		if r.held[d.Addr] > 0 {
			r.t.Fatalf("frame %#x posted while the NIC already owned it", d.Addr)
		}
		r.held[d.Addr]++
	}
	return n
}

// complete reaps completions, recording which frames came back.
func (r *rig) complete(max int) []packetio.Desc {
	r.t.Helper()
	got := r.tx.Complete(max, nil)
	r.tx.Sync()
	for _, d := range got {
		r.held[d.Addr]--
		if r.held[d.Addr] < 0 {
			r.t.Fatalf("frame %#x returned while the NIC did not own it", d.Addr)
		}
	}
	return got
}

func (r *rig) checkNIC() {
	r.t.Helper()
	if len(r.nic.Violations) > 0 {
		for _, v := range r.nic.Violations {
			r.t.Errorf("hardware would reject this: %s", v)
		}
		r.t.FailNow()
	}
}

// wantPackets checks that the model transmitted exactly the frames expected, in
// order and byte for byte.
func (r *rig) wantPackets(frames []int, length int) {
	r.t.Helper()
	if len(r.nic.Packets) != len(frames) {
		r.t.Fatalf("transmitted %d packets, want %d", len(r.nic.Packets), len(frames))
	}
	for i, frame := range frames {
		want := r.region[frame*r.frameSize : frame*r.frameSize+length]
		if got := r.nic.Packets[i].Bytes; !bytes.Equal(got, want) {
			r.t.Fatalf("packet %d: got % x, want % x", i, got[:min(32, len(got))], want[:min(32, len(want))])
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// The plain case: post a batch, let the NIC run, get every frame back.
func TestPostAndComplete(t *testing.T) {
	r := newRig(t, rigOpts{})

	descs := make([]packetio.Desc, 4)
	for i := range descs {
		descs[i] = r.desc(i, 64)
	}
	if n := r.post(descs); n != 4 {
		t.Fatalf("posted %d, want 4", n)
	}
	if got := r.tx.InFlight(); got != 4 {
		t.Fatalf("in flight %d, want 4", got)
	}

	r.nic.Poll()
	r.checkNIC()
	r.wantPackets([]int{0, 1, 2, 3}, 64)

	got := r.complete(64)
	if len(got) != 4 {
		t.Fatalf("completed %d frames, want 4", len(got))
	}
	for i, d := range got {
		if d.Addr != descs[i].Addr || d.Len != descs[i].Len {
			t.Errorf("frame %d came back as %+v, want %+v", i, d, descs[i])
		}
	}
	if r.tx.InFlight() != 0 {
		t.Errorf("still %d in flight", r.tx.InFlight())
	}
	r.checkNIC()
}

// Only the last entry of a batch asks for a completion; the rest complete
// silently and are accounted for by it. Anything else wastes completion queue
// bandwidth, which is most of what batching is for.
func TestOneCompletionPerBatch(t *testing.T) {
	r := newRig(t, rigOpts{blocks: 64, cqEntries: 64})

	descs := make([]packetio.Desc, 16)
	for i := range descs {
		descs[i] = r.desc(i, 64)
	}
	r.post(descs)
	r.nic.Poll()
	r.checkNIC()

	got := r.complete(64)
	if len(got) != 16 {
		t.Fatalf("one completion released %d frames, want 16", len(got))
	}
	if n := r.tx.Stats.Completions.Load(); n != 1 {
		t.Errorf("read %d completions for a batch of 16, want 1", n)
	}
	if n := r.tx.Stats.Batches.Load(); n != 1 {
		t.Errorf("published %d batches, want 1", n)
	}
}

// A caller can ask for completions more often, which costs completion queue
// entries but returns frames sooner.
func TestSignalEvery(t *testing.T) {
	r := newRig(t, rigOpts{blocks: 64, cqEntries: 64, signalEvery: 4})

	descs := make([]packetio.Desc, 16)
	for i := range descs {
		descs[i] = r.desc(i, 64)
	}
	r.post(descs)
	r.nic.Poll()
	r.checkNIC()

	if got := r.tx.Completable(); got != 16 {
		t.Errorf("Completable = %d, want 16", got)
	}
	got := r.complete(64)
	if len(got) != 16 {
		t.Fatalf("released %d frames, want 16", len(got))
	}
	if n := r.tx.Stats.Completions.Load(); n != 4 {
		t.Errorf("read %d completions, want 4", n)
	}
}

// The queue is a ring and the indices are counters that wrap; running many laps
// is the only way to be sure nothing depends on starting from zero.
func TestManyLapsOfTheRing(t *testing.T) {
	const blocks = 8
	r := newRig(t, rigOpts{blocks: blocks, cqEntries: blocks, frames: 8})

	sent := 0
	for lap := 0; lap < 200; lap++ {
		batch := 1 + lap%blocks
		descs := make([]packetio.Desc, 0, batch)
		for i := 0; i < batch; i++ {
			descs = append(descs, r.desc(i, 64+i))
		}
		n := r.post(descs)
		if n == 0 {
			t.Fatalf("lap %d: could not post anything with nothing in flight", lap)
		}
		sent += n
		r.nic.Poll()
		r.checkNIC()
		got := r.complete(64)
		if len(got) != n {
			t.Fatalf("lap %d: posted %d, got back %d", lap, n, len(got))
		}
	}
	if r.nic.Sent() != sent {
		t.Fatalf("model transmitted %d packets, driver posted %d", r.nic.Sent(), sent)
	}
	for addr, n := range r.held {
		if n != 0 {
			t.Fatalf("frame %#x left with a hold count of %d", addr, n)
		}
	}
}

// A full queue must refuse work rather than overwrite entries the NIC has not
// read, and must recover once completions come back.
func TestQueueFullRefusesWork(t *testing.T) {
	const blocks = 8
	r := newRig(t, rigOpts{blocks: blocks, cqEntries: blocks, frames: 32})
	r.nic.BlocksPerPoll = 1 // leave work outstanding on purpose

	descs := make([]packetio.Desc, 32)
	for i := range descs {
		descs[i] = r.desc(i, 64)
	}

	n := r.post(descs)
	if n != blocks {
		t.Fatalf("posted %d into a queue of %d, want %d", n, blocks, blocks)
	}
	if got := r.tx.FreeSlots(); got != 0 {
		t.Fatalf("free slots %d with a full queue", got)
	}
	if again := r.post(descs[n:]); again != 0 {
		t.Fatalf("a full queue accepted %d more packets", again)
	}
	if r.tx.Stats.RingFull.Load() == 0 {
		t.Error("a refused post was not counted")
	}

	// Let the NIC drain it one block at a time; the driver must recover.
	for i := 0; i < blocks; i++ {
		r.nic.Poll()
	}
	r.checkNIC()
	got := r.complete(64)
	if len(got) != blocks {
		t.Fatalf("released %d frames, want %d", len(got), blocks)
	}
	if r.post(descs[n:]) == 0 {
		t.Fatal("the queue did not recover after completions")
	}
	r.checkNIC()
}

// Complete must not hand back more than it was asked for, and what it does hand
// back must be the oldest frames.
func TestCompleteHonoursItsLimit(t *testing.T) {
	r := newRig(t, rigOpts{blocks: 64, cqEntries: 64, signalEvery: 1})

	descs := make([]packetio.Desc, 8)
	for i := range descs {
		descs[i] = r.desc(i, 64)
	}
	r.post(descs)
	r.nic.Poll()
	r.checkNIC()

	got := r.complete(3)
	if len(got) != 3 {
		t.Fatalf("released %d frames for a limit of 3", len(got))
	}
	for i, d := range got {
		if d.Addr != descs[i].Addr {
			t.Errorf("released frame %d out of order", i)
		}
	}
	if r.tx.InFlight() != 5 {
		t.Errorf("in flight %d, want 5", r.tx.InFlight())
	}
	rest := r.complete(64)
	if len(rest) != 5 {
		t.Fatalf("released %d frames, want the remaining 5", len(rest))
	}
}

// Zero and negative limits are a caller asking for nothing, not an invitation
// to drain the queue.
func TestCompleteWithNoLimitDoesNothing(t *testing.T) {
	r := newRig(t, rigOpts{})
	descs := []packetio.Desc{r.desc(0, 64)}
	r.post(descs)
	r.nic.Poll()

	if got := r.tx.Complete(0, nil); len(got) != 0 {
		t.Fatalf("Complete(0) released %d frames", len(got))
	}
	if got := r.tx.Complete(-1, nil); len(got) != 0 {
		t.Fatalf("Complete(-1) released %d frames", len(got))
	}
	if r.tx.InFlight() != 1 {
		t.Fatalf("in flight %d, want 1", r.tx.InFlight())
	}
}

// When the hardware reports a failure, the frames still in the queue have to
// come back: they are flushed rather than sent, and holding on to them would
// leak the pool a queue at a time.
//
// The entry that fails is reported whether or not it asked for a completion,
// which is why every entry records what it did and not only the ones that are
// signalled. A driver that only kept records for signalled entries would be
// unable to say which frames a failure covers at the moment it is told.
func TestHardwareFailureReleasesEverythingInFlight(t *testing.T) {
	r := newRig(t, rigOpts{blocks: 32, cqEntries: 32})
	r.nic.FailAfter = 3
	r.nic.Syndrome = wqe.SyndromeLocalProt

	descs := make([]packetio.Desc, 8)
	for i := range descs {
		descs[i] = r.desc(i, 64)
	}
	r.post(descs)

	// The failure is reported first, naming the third entry, which never asked
	// for a completion: one batch means one signalled entry, the eighth.
	r.nic.Poll()
	r.checkNIC()
	got := r.complete(64)
	if len(got) != 3 {
		t.Fatalf("the failure released %d frames, want the 3 it covers", len(got))
	}
	info := r.tx.Failed()
	if info == nil {
		t.Fatal("the failure was not reported")
	}
	if info.Syndrome != wqe.SyndromeLocalProt {
		t.Errorf("syndrome %#x, want %#x", info.Syndrome, wqe.SyndromeLocalProt)
	}
	if r.tx.Stats.Errors.Load() == 0 {
		t.Error("the failure was not counted")
	}

	// Then the rest of the queue is flushed, and those frames come back too.
	r.nic.Poll()
	r.checkNIC()
	got = r.complete(64)
	if len(got) != 5 {
		t.Fatalf("the flush released %d frames, want the remaining 5", len(got))
	}
	if r.tx.InFlight() != 0 {
		t.Errorf("still %d in flight after a failure", r.tx.InFlight())
	}

	// A failed queue takes no more work: it would never be sent.
	if n := r.tx.Post(descs); n != 0 {
		t.Errorf("a failed queue accepted %d packets", n)
	}
	r.tx.Sync()
	if got := r.tx.FreeSlots(); got != 0 {
		t.Errorf("a failed queue reports %d free slots", got)
	}
}

// A completion whose work queue entry counter names something that is not
// outstanding must release nothing. This is the failure mode that matters most:
// believing it would hand the application frames the NIC is still reading, and
// the only symptom would be a corrupted packet on the wire that no counter
// anywhere records.
//
// The dangerous forgery is not a nonsense counter but a plausible one. The
// hardware reports only the low 16 bits of a block index, and the table that
// says what an entry did is indexed by the queue size, so a counter one whole
// queue further on lands on a live entry. Nothing about the value looks wrong;
// only the fact that it is further ahead than anything outstanding gives it
// away.
func TestStaleCompletionReleasesNothing(t *testing.T) {
	const blocks = 16
	forge := func(t *testing.T, r *rig, counter uint16) []packetio.Desc {
		t.Helper()
		cq, err := wqe.NewCQ(r.nic.CQBytes(), wqe.CQESize)
		if err != nil {
			t.Fatal(err)
		}
		ci := r.tx.CQConsumerIndex()
		e := cq.Entry(ci)
		for i := range e {
			e[i] = 0
		}
		e[60], e[61] = byte(counter>>8), byte(counter)
		e[63] = wqe.CQEReq<<4 | uint8(cq.ExpectedOwner(ci))
		out := r.tx.Complete(64, nil)
		r.tx.Sync()
		return out
	}

	setup := func(t *testing.T) *rig {
		t.Helper()
		r := newRig(t, rigOpts{blocks: blocks, cqEntries: blocks})
		descs := make([]packetio.Desc, 4)
		for i := range descs {
			descs[i] = r.desc(i, 64)
		}
		// One batch through to completion, so the counters are not at zero.
		r.post(descs)
		r.nic.Poll()
		r.checkNIC()
		if got := r.complete(64); len(got) != 4 {
			t.Fatalf("released %d frames, want 4", len(got))
		}
		// A second batch left in flight, with nothing completed for it yet.
		r.nic.BlocksPerPoll = -1 // the model must not run
		for i := range descs {
			descs[i] = r.desc(4+i, 64)
		}
		r.post(descs)
		return r
	}

	t.Run("one lap ahead of a live entry", func(t *testing.T) {
		r := setup(t)
		// The oldest outstanding entry, named one whole queue later. Masked to
		// the queue size it is indistinguishable from the real thing.
		counter := uint16(r.tx.SQHead() + blocks)
		if got := forge(t, r, counter); len(got) != 0 {
			t.Fatalf("a completion a lap ahead released %d frames", len(got))
		}
		if r.tx.InFlight() != 4 {
			t.Errorf("in flight %d, want the 4 still outstanding", r.tx.InFlight())
		}
		if r.tx.Stats.Errors.Load() == 0 {
			t.Error("the forged completion was not counted as an error")
		}
	})

	t.Run("an entry that was never posted", func(t *testing.T) {
		r := setup(t)
		if got := forge(t, r, 0xffff); len(got) != 0 {
			t.Fatalf("a completion for an unposted entry released %d frames", len(got))
		}
		if r.tx.InFlight() != 4 {
			t.Errorf("in flight %d, want the 4 still outstanding", r.tx.InFlight())
		}
	})

	t.Run("an entry from a previous lap", func(t *testing.T) {
		r := setup(t)
		// Block 0 completed long ago; its record still says so.
		if got := forge(t, r, 0); len(got) != 0 {
			t.Fatalf("a completion from a previous lap released %d frames", len(got))
		}
		if r.tx.InFlight() != 4 {
			t.Errorf("in flight %d, want the 4 still outstanding", r.tx.InFlight())
		}
	})
}

// An empty completion queue is the common case on a quiet queue and must be
// cheap and harmless.
func TestCompleteOnAnEmptyQueue(t *testing.T) {
	r := newRig(t, rigOpts{})
	for i := 0; i < 10; i++ {
		if got := r.tx.Complete(64, nil); len(got) != 0 {
			t.Fatalf("released %d frames from an empty queue", len(got))
		}
		if got := r.tx.Completable(); got != 0 {
			t.Fatalf("Completable = %d on an empty queue", got)
		}
	}
	if r.tx.Stats.Errors.Load() != 0 {
		t.Error("polling an empty queue reported an error")
	}
}

// Posting and completing are the packet path.
func TestPostAndCompleteDoNotAllocate(t *testing.T) {
	r := newRig(t, rigOpts{blocks: 64, cqEntries: 64, frames: 64})
	r.nic.Discard = true // otherwise this measures the model, not the driver
	descs := make([]packetio.Desc, 32)
	for i := range descs {
		descs[i] = r.desc(i, 64)
	}
	out := make([]packetio.Desc, 0, 64)

	allocs := testing.AllocsPerRun(100, func() {
		r.tx.Post(descs)
		r.nic.Poll()
		out = r.tx.Complete(64, out[:0])
	})
	if allocs != 0 {
		t.Fatalf("the packet path allocated %v times per run, want 0", allocs)
	}
}

func TestNewTxRejectsBadConfigurations(t *testing.T) {
	sq := make([]byte, 8*wqe.WQEBB)
	cq := make([]byte, 8*wqe.CQESize)
	dbrec := new([2]uint32)
	uar := new(uint64)
	base := TxConfig{
		SQ: sq, CQ: cq, SQDbrec: &dbrec[1], CQDbrec: &dbrec[0],
		UAR: unsafe.Pointer(uar), InlineLen: wqe.EthL2InlineHeaderSize,
		FrameSize: 2048,
	}

	t.Run("no doorbell record", func(t *testing.T) {
		cfg := base
		cfg.SQDbrec = nil
		if _, err := NewTx(cfg); err == nil {
			t.Error("accepted a queue with no doorbell record")
		}
	})
	t.Run("no user access region", func(t *testing.T) {
		cfg := base
		cfg.UAR = nil
		if _, err := NewTx(cfg); err == nil {
			t.Error("accepted a queue with no user access region")
		}
	})
	t.Run("completion queue too small", func(t *testing.T) {
		cfg := base
		cfg.CQ = make([]byte, 4*wqe.CQESize)
		if _, err := NewTx(cfg); err == nil {
			t.Error("accepted a completion queue smaller than the send queue")
		}
	})
	t.Run("entry larger than the queue", func(t *testing.T) {
		cfg := base
		cfg.SQ = make([]byte, wqe.WQEBB)
		cfg.CQ = make([]byte, wqe.CQESize)
		cfg.InlineLen = 200
		if _, err := NewTx(cfg); err == nil {
			t.Error("accepted an entry that does not fit the queue")
		}
	})
}

// Entries larger than one block are what a longer inline header, a chained
// packet or a multi-packet entry all produce, so the accounting must not assume
// one block per packet. With a 40-byte inline header an entry is two blocks,
// and half of them straddle the end of a queue whose size is odd in blocks.
func TestMultiBlockEntries(t *testing.T) {
	const blocks = 16
	r := newRig(t, rigOpts{blocks: blocks, cqEntries: blocks, inlineLen: 40, frames: 16})

	if free := r.tx.FreeSlots(); free != blocks/2 {
		t.Fatalf("a queue of %d blocks holds %d two-block entries, want %d", blocks, free, blocks/2)
	}

	for lap := 0; lap < 50; lap++ {
		batch := 1 + lap%(blocks/2)
		descs := make([]packetio.Desc, 0, batch)
		for i := 0; i < batch; i++ {
			descs = append(descs, r.desc(i, 64+i))
		}
		n := r.post(descs)
		if n != batch {
			t.Fatalf("lap %d: posted %d of %d", lap, n, batch)
		}
		r.nic.Poll()
		r.checkNIC()
		if got := r.complete(64); len(got) != n {
			t.Fatalf("lap %d: posted %d, got back %d", lap, n, len(got))
		}
	}
	// The model checked every entry's structure as it went; check the bytes of
	// the last packet too, since a two-block entry splits a packet across the
	// inline header and the data segment differently from a one-block one.
	last := r.nic.Packets[len(r.nic.Packets)-1]
	if last.Inlined != 40 {
		t.Errorf("the last packet inlined %d bytes, want 40", last.Inlined)
	}
	frame := int(last.Addr-r.regionVA) / r.frameSize
	want := r.region[frame*r.frameSize : frame*r.frameSize+len(last.Bytes)]
	if !bytes.Equal(last.Bytes, want) {
		t.Errorf("last packet = % x, want % x", last.Bytes, want)
	}
}

// A long run with batch sizes, poll rates and completion limits all varying is
// the closest thing to what a real workload does to the indices. The invariant
// it enforces is the only one that matters: at every moment, every frame is
// owned by exactly one of the pool, the driver or the NIC.
func TestRandomisedOwnership(t *testing.T) {
	const (
		blocks = 32
		frames = 64
	)
	r := newRig(t, rigOpts{blocks: blocks, cqEntries: blocks, frames: frames})

	// A tiny deterministic generator, so a failure is reproducible.
	seed := uint32(12345)
	next := func(n int) int {
		seed = seed*1664525 + 1013904223
		return int(seed>>16) % n
	}

	free := make([]int, 0, frames)
	for i := 0; i < frames; i++ {
		free = append(free, i)
	}
	inFlight := map[uint64]int{}
	posted, completed := 0, 0

	descs := make([]packetio.Desc, 0, frames)
	for step := 0; step < 5000; step++ {
		switch next(4) {
		case 0, 1: // post a batch
			want := next(8) + 1
			if want > len(free) {
				want = len(free)
			}
			descs = descs[:0]
			for i := 0; i < want; i++ {
				descs = append(descs, r.desc(free[len(free)-1-i], 64+next(64)))
			}
			n := r.tx.Post(descs)
			for _, d := range descs[:n] {
				if _, dup := inFlight[d.Addr]; dup {
					t.Fatalf("step %d: frame %#x posted twice", step, d.Addr)
				}
				inFlight[d.Addr] = step
			}
			free = free[:len(free)-n]
			posted += n

		case 2: // let the NIC run, sometimes only partly
			r.nic.BlocksPerPoll = next(5)
			r.nic.Poll()
			r.checkNIC()

		case 3: // reap some completions
			max := next(16) + 1
			for _, d := range r.tx.Complete(max, nil) {
				if _, ok := inFlight[d.Addr]; !ok {
					t.Fatalf("step %d: frame %#x returned but was not in flight", step, d.Addr)
				}
				delete(inFlight, d.Addr)
				free = append(free, int(d.Addr/uint64(r.frameSize)))
				completed++
			}
		}

		if got := r.tx.InFlight(); got != len(inFlight) {
			t.Fatalf("step %d: the ring says %d in flight, the oracle says %d", step, got, len(inFlight))
		}
		if len(free)+len(inFlight) != frames {
			t.Fatalf("step %d: %d free plus %d in flight is not %d", step, len(free), len(inFlight), frames)
		}
	}

	// Drain, and everything must come back.
	r.nic.BlocksPerPoll = 0
	for i := 0; i < 64 && len(inFlight) > 0; i++ {
		r.nic.Poll()
		for _, d := range r.tx.Complete(frames, nil) {
			delete(inFlight, d.Addr)
			completed++
		}
	}
	r.checkNIC()
	if len(inFlight) != 0 {
		t.Fatalf("%d frames never came back", len(inFlight))
	}
	if posted != completed {
		t.Fatalf("posted %d packets, completed %d", posted, completed)
	}
	if posted < 1000 {
		t.Fatalf("only %d packets went through; the test is not exercising much", posted)
	}
	t.Logf("%d packets through a %d-block queue", posted, blocks)
}

// Posting nothing must do nothing. A doorbell rung for an empty batch tells the
// NIC to look at a producer index it already has, using eight bytes read from
// an entry it may still be working on.
func TestPostNothingRingsNoDoorbell(t *testing.T) {
	r := newRig(t, rigOpts{})

	if n := r.tx.Post(nil); n != 0 {
		t.Fatalf("Post(nil) took %d packets", n)
	}
	if n := r.tx.Post([]packetio.Desc{}); n != 0 {
		t.Fatalf("Post of an empty batch took %d packets", n)
	}
	if got := r.tx.Stats.Batches.Load(); got != 0 {
		t.Errorf("published %d batches for no packets", got)
	}
	r.nic.Poll()
	r.checkNIC()
	if r.nic.Sent() != 0 {
		t.Errorf("the model transmitted %d packets", r.nic.Sent())
	}

	// And a real batch afterwards still works.
	if n := r.post([]packetio.Desc{r.desc(0, 64)}); n != 1 {
		t.Fatalf("posted %d, want 1", n)
	}
	r.nic.Poll()
	r.checkNIC()
	if r.nic.Sent() != 1 {
		t.Fatalf("the model transmitted %d packets, want 1", r.nic.Sent())
	}
}

// A packet carried entirely inside its work queue entry must have no data
// segment. A ConnectX-6 Dx answers a data segment describing zero bytes with a
// local length error and takes the queue pair out of service, so this is not a
// matter of taste; the model rejects it the same way.
func TestFullyInlinePacketsHaveNoDataSegment(t *testing.T) {
	// Inline more than the packets are long, so every one of them is carried
	// whole inside its entry.
	r := newRig(t, rigOpts{blocks: 32, cqEntries: 32, inlineLen: 128, frames: 16})

	descs := make([]packetio.Desc, 6)
	for i := range descs {
		descs[i] = r.desc(i, 60+i)
	}
	if n := r.post(descs); n != len(descs) {
		t.Fatalf("posted %d of %d", n, len(descs))
	}
	r.nic.Poll()
	r.checkNIC() // the model would have complained about an empty data segment

	if len(r.nic.Packets) != len(descs) {
		t.Fatalf("the model transmitted %d packets, want %d", len(r.nic.Packets), len(descs))
	}
	for i, p := range r.nic.Packets {
		if p.Inlined != 60+i {
			t.Errorf("packet %d inlined %d of its %d bytes", i, p.Inlined, 60+i)
		}
		want := r.region[i*r.frameSize : i*r.frameSize+60+i]
		if !bytes.Equal(p.Bytes, want) {
			t.Errorf("packet %d: got % x, want % x", i, p.Bytes, want)
		}
	}
	if got := r.complete(64); len(got) != len(descs) {
		t.Fatalf("released %d frames, want %d", len(got), len(descs))
	}
}

// Entries of different sizes in one batch: the queue must advance by what each
// one actually took, not by what the largest could take. Getting this wrong
// desynchronises software from the hardware, which then reads the middle of an
// entry as the start of the next one.
func TestMixedEntrySizes(t *testing.T) {
	// With an 80-byte inline header a packet longer than that needs a data
	// segment and two blocks, while a shorter one fits in one.
	r := newRig(t, rigOpts{blocks: 64, cqEntries: 64, inlineLen: 80, frames: 32})

	sizes := []int{60, 200, 64, 300, 70, 90}
	descs := make([]packetio.Desc, 0, len(sizes))
	for i, sz := range sizes {
		descs = append(descs, r.desc(i, sz))
	}
	if n := r.post(descs); n != len(descs) {
		t.Fatalf("posted %d of %d", n, len(descs))
	}
	r.nic.Poll()
	r.checkNIC()

	if len(r.nic.Packets) != len(sizes) {
		t.Fatalf("the model transmitted %d packets, want %d", len(r.nic.Packets), len(sizes))
	}
	for i, sz := range sizes {
		p := r.nic.Packets[i]
		if len(p.Bytes) != sz {
			t.Fatalf("packet %d is %d bytes, want %d", i, len(p.Bytes), sz)
		}
		want := r.region[i*r.frameSize : i*r.frameSize+sz]
		if !bytes.Equal(p.Bytes, want) {
			t.Errorf("packet %d does not match its frame", i)
		}
		if sz <= 80 && p.Inlined != sz {
			t.Errorf("packet %d of %d bytes inlined %d; it should be carried whole", i, sz, p.Inlined)
		}
		if sz > 80 && p.Inlined != 80 {
			t.Errorf("packet %d of %d bytes inlined %d, want 80", i, sz, p.Inlined)
		}
	}
	if got := r.complete(64); len(got) != len(descs) {
		t.Fatalf("released %d frames, want %d", len(got), len(descs))
	}
	if r.tx.InFlight() != 0 {
		t.Errorf("%d frames still in flight", r.tx.InFlight())
	}
}

// Several packets in one entry is what lets a device transmit small packets at
// line rate: the packets are copied in behind their lengths, so the NIC fetches
// one descriptor instead of a descriptor and then each packet.
func TestMultiPacketEntries(t *testing.T) {
	r := newRig(t, rigOpts{
		blocks: 64, cqEntries: 64, frames: 64,
		multiPacket: true, mpwMaxLen: 128,
	})
	if !r.tx.MultiPacket() {
		t.Fatal("the ring is not carrying several packets per entry")
	}

	descs := make([]packetio.Desc, 12)
	for i := range descs {
		descs[i] = r.desc(i, 60+i)
	}
	if n := r.post(descs); n != len(descs) {
		t.Fatalf("posted %d of %d", n, len(descs))
	}
	r.nic.Poll()
	r.checkNIC()

	// Every packet arrives, in order, byte for byte.
	if len(r.nic.Packets) != len(descs) {
		t.Fatalf("the model transmitted %d packets, want %d", len(r.nic.Packets), len(descs))
	}
	for i := range descs {
		want := r.region[i*r.frameSize : i*r.frameSize+60+i]
		if !bytes.Equal(r.nic.Packets[i].Bytes, want) {
			t.Fatalf("packet %d does not match its frame", i)
		}
	}

	// And they were carried by far fewer entries than there were packets,
	// which is the whole point.
	entries := map[uint32]bool{}
	for _, p := range r.nic.Packets {
		entries[p.Block] = true
	}
	if len(entries) >= len(descs) {
		t.Errorf("%d packets took %d entries; they were not shared", len(descs), len(entries))
	}

	if got := r.complete(64); len(got) != len(descs) {
		t.Fatalf("released %d frames, want %d", len(got), len(descs))
	}
	if n := r.tx.Stats.Completions.Load(); n >= uint64(len(descs)) {
		t.Errorf("read %d completions for %d packets; one entry should report once", n, len(descs))
	}
}

// A packet too long to be worth copying goes in an entry of its own, and the
// packets around it still share theirs.
func TestMultiPacketFallsBackForLongPackets(t *testing.T) {
	r := newRig(t, rigOpts{
		blocks: 128, cqEntries: 128, frames: 32, frameSize: 4096,
		multiPacket: true, mpwMaxLen: 128,
	})

	sizes := []int{60, 60, 1500, 60, 60}
	descs := make([]packetio.Desc, 0, len(sizes))
	for i, sz := range sizes {
		descs = append(descs, r.desc(i, sz))
	}
	if n := r.post(descs); n != len(descs) {
		t.Fatalf("posted %d of %d", n, len(descs))
	}
	r.nic.Poll()
	r.checkNIC()

	if len(r.nic.Packets) != len(sizes) {
		t.Fatalf("transmitted %d packets, want %d", len(r.nic.Packets), len(sizes))
	}
	for i, sz := range sizes {
		if got := len(r.nic.Packets[i].Bytes); got != sz {
			t.Fatalf("packet %d is %d bytes, want %d", i, got, sz)
		}
		want := r.region[i*r.frameSize : i*r.frameSize+sz]
		if !bytes.Equal(r.nic.Packets[i].Bytes, want) {
			t.Fatalf("packet %d does not match its frame", i)
		}
	}
	if got := r.complete(64); len(got) != len(descs) {
		t.Fatalf("released %d frames, want %d", len(got), len(descs))
	}
}

// The same ownership rules apply when one entry accounts for many packets: a
// completion releases exactly the packets its entry carried, and no others.
func TestMultiPacketOwnershipOverManyLaps(t *testing.T) {
	const blocks = 32
	r := newRig(t, rigOpts{
		blocks: blocks, cqEntries: blocks, frames: 32,
		multiPacket: true, mpwMaxLen: 64,
	})

	sent := 0
	for lap := 0; lap < 200; lap++ {
		batch := 1 + lap%12
		descs := make([]packetio.Desc, 0, batch)
		for i := 0; i < batch; i++ {
			descs = append(descs, r.desc(i, 60))
		}
		n := r.post(descs)
		if n == 0 {
			t.Fatalf("lap %d: posted nothing with nothing in flight", lap)
		}
		sent += n
		r.nic.Poll()
		r.checkNIC()
		if got := r.complete(64); len(got) != n {
			t.Fatalf("lap %d: posted %d, got back %d", lap, n, len(got))
		}
	}
	if r.nic.Sent() != sent {
		t.Fatalf("the model transmitted %d packets, the driver posted %d", r.nic.Sent(), sent)
	}
	for addr, n := range r.held {
		if n != 0 {
			t.Fatalf("frame %#x left with a hold count of %d", addr, n)
		}
	}
	t.Logf("%d packets through in %d entries", sent, r.tx.Stats.Batches.Load())
}

func TestMultiPacketDoesNotAllocate(t *testing.T) {
	r := newRig(t, rigOpts{
		blocks: 256, cqEntries: 256, frames: 64,
		multiPacket: true, mpwMaxLen: 64,
	})
	r.nic.Discard = true
	descs := make([]packetio.Desc, 32)
	for i := range descs {
		descs[i] = r.desc(i, 60)
	}
	out := make([]packetio.Desc, 0, 64)

	allocs := testing.AllocsPerRun(100, func() {
		r.tx.Post(descs)
		r.nic.Poll()
		out = r.tx.Complete(64, out[:0])
	})
	if allocs != 0 {
		t.Fatalf("the multi-packet path allocated %v times per run, want 0", allocs)
	}
}

// A device that insists on an inline Ethernet header cannot be given entries
// whose Ethernet segment has no room for one.
func TestMultiPacketRefusedWhenAnInlineHeaderIsRequired(t *testing.T) {
	sq := make([]byte, 32*wqe.WQEBB)
	cq := make([]byte, 32*wqe.CQESize)
	dbrec := new([2]uint32)
	uar := new(uint64)
	_, err := NewTx(TxConfig{
		SQ: sq, CQ: cq, SQDbrec: &dbrec[1], CQDbrec: &dbrec[0], UAR: unsafe.Pointer(uar),
		FrameSize: 2048,
		InlineLen: wqe.EthL2InlineHeaderSize, MultiPacket: true, MultiPacketMaxLen: 64,
	})
	if err == nil {
		t.Fatal("accepted multi-packet entries on a device that requires an inline header")
	}
}

// A descriptor that does not name bytes inside one frame is refused, and the
// batch stops there. This is the rule in BACKENDS.md, and it is load-bearing:
// an address past the region panics a process that runs as root, and a length
// running past its own frame tells the NIC to fetch the neighbouring frames and
// put them on the wire.
func TestPostRefusesDescriptorsOutsideOneFrame(t *testing.T) {
	for _, tc := range []struct {
		name string
		bad  packetio.Desc
	}{
		{"past the region", packetio.Desc{Addr: 64 * 2048, Len: 64}},
		{"length past the region", packetio.Desc{Addr: 63 * 2048, Len: 4096}},
		{"length past its own frame", packetio.Desc{Addr: 2048 + 1600, Len: 1024}},
		{"zero length", packetio.Desc{Addr: 2048, Len: 0}},
		{"address wraps", packetio.Desc{Addr: ^uint64(0), Len: 64}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, rigOpts{frames: 64, frameSize: 2048})

			// Good, bad, good: the prefix before the bad one is taken and
			// nothing after it is, so a short return points at the offender.
			descs := []packetio.Desc{r.desc(0, 64), tc.bad, r.desc(2, 64)}
			n := r.tx.Post(descs)
			r.tx.Sync()
			if n != 1 {
				t.Fatalf("Post took %d descriptors, want 1 (the prefix before the bad one)", n)
			}
			if got := r.tx.Stats.BadDesc.Load(); got != 1 {
				t.Errorf("BadDesc = %d, want 1", got)
			}
		})
	}
}

// The same rule holds for multi-packet entries, which take a different path
// through Post.
func TestPostMultiRefusesDescriptorsOutsideOneFrame(t *testing.T) {
	r := newRig(t, rigOpts{frames: 64, frameSize: 2048, multiPacket: true, mpwMaxLen: 256})
	descs := []packetio.Desc{r.desc(0, 64), {Addr: 2048 + 1600, Len: 1024}, r.desc(2, 64)}
	n := r.tx.Post(descs)
	r.tx.Sync()
	if n != 1 {
		t.Fatalf("Post took %d descriptors, want 1", n)
	}
	if got := r.tx.Stats.BadDesc.Load(); got != 1 {
		t.Errorf("BadDesc = %d, want 1", got)
	}
}

// Pointer entries: several packets behind one control segment, none of them
// copied. The model reads each through its data segment, so a wrong address or
// key would fail here, and the ownership rules are exactly those of the
// copying form.
func TestMultiPacketPointerEntries(t *testing.T) {
	r := newRig(t, rigOpts{
		blocks: 64, cqEntries: 64, frames: 128,
		multiPacket: true, mpwPointer: true,
	})
	if !r.tx.MultiPacket() {
		t.Fatal("the ring is not carrying several packets per entry")
	}

	descs := make([]packetio.Desc, 70) // more than one entry can hold
	for i := range descs {
		descs[i] = r.desc(i, 60+i%40)
	}
	if n := r.post(descs); n != len(descs) {
		t.Fatalf("posted %d of %d", n, len(descs))
	}
	r.nic.Poll()
	r.checkNIC()

	if len(r.nic.Packets) != len(descs) {
		t.Fatalf("the model transmitted %d packets, want %d", len(r.nic.Packets), len(descs))
	}
	for i := range descs {
		want := r.region[i*r.frameSize : i*r.frameSize+60+i%40]
		if !bytes.Equal(r.nic.Packets[i].Bytes, want) {
			t.Fatalf("packet %d does not match its frame", i)
		}
		if r.nic.Packets[i].Inlined != 0 {
			t.Fatalf("packet %d carried %d inlined bytes; pointer entries copy nothing",
				i, r.nic.Packets[i].Inlined)
		}
	}

	// 70 packets at 58 per entry is two entries, not seventy.
	entries := map[uint32]bool{}
	for _, p := range r.nic.Packets {
		entries[p.Block] = true
	}
	if len(entries) != 2 {
		t.Errorf("%d packets took %d entries, want 2", len(descs), len(entries))
	}

	if got := r.complete(128); len(got) != len(descs) {
		t.Fatalf("released %d frames, want %d", len(got), len(descs))
	}
}

// A long packet needs no fallback in pointer mode: it is one data segment like
// any other, so a mixture of sizes shares entries freely.
func TestMultiPacketPointerCarriesLongPackets(t *testing.T) {
	r := newRig(t, rigOpts{
		blocks: 64, cqEntries: 64, frames: 32, frameSize: 4096,
		multiPacket: true, mpwPointer: true,
	})
	descs := []packetio.Desc{
		r.desc(0, 60), r.desc(1, 3000), r.desc(2, 60), r.desc(3, 1500),
	}
	if n := r.post(descs); n != len(descs) {
		t.Fatalf("posted %d of %d", n, len(descs))
	}
	r.nic.Poll()
	r.checkNIC()
	if len(r.nic.Packets) != len(descs) {
		t.Fatalf("transmitted %d packets, want %d", len(r.nic.Packets), len(descs))
	}
	entries := map[uint32]bool{}
	for _, p := range r.nic.Packets {
		entries[p.Block] = true
	}
	if len(entries) != 1 {
		t.Errorf("four packets took %d entries, want all in 1", len(entries))
	}
	if got := r.complete(64); len(got) != len(descs) {
		t.Fatalf("released %d frames, want %d", len(got), len(descs))
	}
}

// Frames are conserved across many laps of the queue in pointer mode, exactly
// as the copying form's lap test demands.
func TestMultiPacketPointerOwnershipOverManyLaps(t *testing.T) {
	r := newRig(t, rigOpts{
		blocks: 16, cqEntries: 16, frames: 64,
		multiPacket: true, mpwPointer: true,
	})
	r.nic.Discard = true
	var out []packetio.Desc
	next := 0
	for round := 0; round < 500; round++ {
		batch := make([]packetio.Desc, 8)
		for i := range batch {
			batch[i] = r.desc(next%r.frames, 60)
			next++
		}
		posted := r.post(batch)
		r.nic.Poll()
		out = r.complete(64)
		_ = out
		if posted == 0 {
			t.Fatalf("round %d posted nothing", round)
		}
	}
	r.checkNIC()
	if got := r.tx.Stats.BadDesc.Load(); got != 0 {
		t.Fatalf("%d descriptors refused", got)
	}
}
