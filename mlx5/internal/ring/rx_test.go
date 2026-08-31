package ring

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/mlx5/internal/mocknic"
	"github.com/atoonk/packetio/mlx5/internal/wqe"
)

// rxRig is a receive ring wired to the software NIC over shared queue memory.
type rxRig struct {
	t         *testing.T
	rx        *Rx
	nic       *mocknic.RxNIC
	region    []byte
	frameSize int
	frames    int
	regionVA  uint64

	// owner tracks who holds each frame, so a buffer handed to the NIC twice
	// or handed back twice is caught where it happens.
	owner map[uint64]string
}

func newRxRig(t *testing.T, entries, cqEntries, frames, frameSize int) *rxRig {
	t.Helper()
	r := &rxRig{
		t: t, frameSize: frameSize, frames: frames,
		region:   make([]byte, frames*frameSize),
		regionVA: 0x7f0000000000,
		owner:    map[uint64]string{},
	}
	rq := make([]byte, entries*16)
	cq := make([]byte, cqEntries*wqe.CQESize)
	rqDbrec := new(uint32)
	cqDbrec := new(uint32)

	nic, err := mocknic.NewRx(mocknic.RxConfig{
		RQ: rq, CQ: cq, RQDbrec: rqDbrec, CQDbrec: cqDbrec,
		Region: r.region, RegionVA: r.regionVA, LKey: 0x99,
	})
	if err != nil {
		t.Fatalf("mocknic.NewRx: %v", err)
	}
	r.nic = nic

	rx, err := NewRx(RxConfig{
		RQ: rq, CQ: cq, RQDbrec: rqDbrec, CQDbrec: cqDbrec,
		Stride: 16, LKey: 0x99,
		Region: r.region, RegionVA: r.regionVA,
		BufferSize: uint32(frameSize),
	})
	if err != nil {
		t.Fatalf("NewRx: %v", err)
	}
	r.rx = rx
	for i := 0; i < frames; i++ {
		r.owner[uint64(i*frameSize)] = "pool"
	}
	return r
}

// fill posts buffers, recording that the NIC now holds them.
func (r *rxRig) fill(frames ...int) int {
	r.t.Helper()
	descs := make([]packetio.Desc, 0, len(frames))
	for _, f := range frames {
		addr := uint64(f * r.frameSize)
		if r.owner[addr] != "pool" {
			r.t.Fatalf("frame %#x was posted while %s held it", addr, r.owner[addr])
		}
		descs = append(descs, packetio.Desc{Addr: addr})
	}
	n := r.rx.Fill(descs)
	r.rx.Sync()
	for _, d := range descs[:n] {
		r.owner[d.Addr] = "nic"
	}
	return n
}

// receive takes packets, recording that the application now holds their frames.
func (r *rxRig) receive(max int) []packetio.Desc {
	r.t.Helper()
	got := r.rx.Receive(max, nil)
	r.rx.Sync()
	for _, d := range got {
		if r.owner[d.Addr] != "nic" {
			r.t.Fatalf("frame %#x came back from the NIC while %s held it", d.Addr, r.owner[d.Addr])
		}
		r.owner[d.Addr] = "app"
	}
	for _, d := range r.rx.Failed() {
		if r.owner[d.Addr] != "nic" {
			r.t.Fatalf("failed frame %#x came back while %s held it", d.Addr, r.owner[d.Addr])
		}
		r.owner[d.Addr] = "pool" // straight back, as the queue would
	}
	return got
}

func (r *rxRig) recycle(descs []packetio.Desc) {
	r.t.Helper()
	for _, d := range descs {
		if r.owner[d.Addr] != "app" {
			r.t.Fatalf("frame %#x was recycled while %s held it", d.Addr, r.owner[d.Addr])
		}
		r.owner[d.Addr] = "pool"
	}
}

func (r *rxRig) checkNIC() {
	r.t.Helper()
	if len(r.nic.Violations) > 0 {
		for _, v := range r.nic.Violations {
			r.t.Errorf("hardware would reject this: %s", v)
		}
		r.t.FailNow()
	}
}

func packets(sizes ...int) [][]byte {
	out := make([][]byte, 0, len(sizes))
	for i, n := range sizes {
		p := make([]byte, n)
		copy(p, fmt.Sprintf("packet%02d|", i))
		for j := len("packet00|"); j < n; j++ {
			p[j] = byte(i*31 + j)
		}
		out = append(out, p)
	}
	return out
}

// The plain case: post buffers, take the packets, give the buffers back.
func TestFillAndReceive(t *testing.T) {
	r := newRxRig(t, 16, 16, 32, 2048)

	if n := r.fill(0, 1, 2, 3); n != 4 {
		t.Fatalf("posted %d buffers, want 4", n)
	}
	if got := r.rx.Outstanding(); got != 4 {
		t.Fatalf("%d buffers outstanding, want 4", got)
	}

	pkts := packets(64, 128, 60, 1500)
	if n := r.nic.Deliver(pkts); n != 4 {
		t.Fatalf("the model delivered %d packets, want 4", n)
	}
	r.checkNIC()

	got := r.receive(16)
	if len(got) != 4 {
		t.Fatalf("received %d packets, want 4", len(got))
	}
	for i, d := range got {
		if int(d.Len) != len(pkts[i]) {
			t.Errorf("packet %d is %d bytes, want %d", i, d.Len, len(pkts[i]))
		}
		if body := r.region[d.Addr : d.Addr+uint64(d.Len)]; !bytes.Equal(body, pkts[i]) {
			t.Errorf("packet %d does not match what was sent", i)
		}
		if d.Options&packetio.OptChecksumOK == 0 {
			t.Errorf("packet %d does not report its checksums as good", i)
		}
	}
	if r.rx.Outstanding() != 0 {
		t.Errorf("%d buffers still outstanding", r.rx.Outstanding())
	}
	r.recycle(got)
}

// A buffer may only be posted into a slot whose previous occupant has been
// reported on. If the queue let software run further ahead than that, the NIC
// would be handed an address it is already writing to.
func TestFillStopsAtTheQueueSize(t *testing.T) {
	const entries = 8
	r := newRxRig(t, entries, entries, 32, 2048)

	all := make([]int, 16)
	for i := range all {
		all[i] = i
	}
	if n := r.fill(all...); n != entries {
		t.Fatalf("posted %d buffers into a queue of %d", n, entries)
	}
	if got := r.rx.FreeSlots(); got != 0 {
		t.Fatalf("%d slots free in a full queue", got)
	}
	if n := r.rx.Fill([]packetio.Desc{{Addr: 0}}); n != 0 {
		t.Fatalf("a full queue accepted %d more buffers", n)
	}

	// Draining some makes room again.
	r.nic.Deliver(packets(64, 64, 64))
	r.checkNIC()
	got := r.receive(16)
	if len(got) != 3 {
		t.Fatalf("received %d packets, want 3", len(got))
	}
	r.recycle(got)
	if free := r.rx.FreeSlots(); free != 3 {
		t.Fatalf("%d slots free after taking 3 packets, want 3", free)
	}
}

// A packet that arrives with nowhere to put it is lost at the port, which is
// what the receive counters on a busy machine are showing.
func TestPacketsWithNoBufferAreLost(t *testing.T) {
	r := newRxRig(t, 8, 8, 32, 2048)
	r.fill(0, 1)

	if n := r.nic.Deliver(packets(64, 64, 64, 64)); n != 2 {
		t.Fatalf("the model delivered %d packets into 2 buffers", n)
	}
	r.checkNIC()
	if got := r.receive(16); len(got) != 2 {
		t.Fatalf("received %d packets, want 2", len(got))
	}
}

// Many laps of the queue, so that nothing depends on the indices starting at
// zero or staying small.
func TestManyLapsOfTheReceiveQueue(t *testing.T) {
	const entries = 8
	r := newRxRig(t, entries, entries, entries, 2048)

	free := make([]int, 0, entries)
	for i := 0; i < entries; i++ {
		free = append(free, i)
	}

	total := 0
	for lap := 0; lap < 300; lap++ {
		n := 1 + lap%entries
		if n > len(free) {
			n = len(free)
		}
		posted := r.fill(free[:n]...)
		free = free[posted:]

		sizes := make([]int, posted)
		for i := range sizes {
			sizes[i] = 60 + i
		}
		if d := r.nic.Deliver(packets(sizes...)); d != posted {
			t.Fatalf("lap %d: delivered %d of %d", lap, d, posted)
		}
		r.checkNIC()

		got := r.receive(entries)
		if len(got) != posted {
			t.Fatalf("lap %d: posted %d buffers, received %d packets", lap, posted, len(got))
		}
		for i, d := range got {
			if int(d.Len) != 60+i {
				t.Fatalf("lap %d packet %d: %d bytes, want %d", lap, i, d.Len, 60+i)
			}
		}
		r.recycle(got)
		for _, d := range got {
			free = append(free, int(d.Addr/uint64(r.frameSize)))
		}
		total += posted
	}
	if total < 1000 {
		t.Fatalf("only %d packets went through", total)
	}
	if len(free) != entries {
		t.Fatalf("%d frames came back, want %d", len(free), entries)
	}
}

// A failed packet still consumes its buffer, and that buffer has to find its
// way back to the pool or the queue bleeds frames one failure at a time.
func TestFailedPacketsReturnTheirBuffers(t *testing.T) {
	r := newRxRig(t, 16, 16, 32, 2048)
	r.nic.FailEvery = 2 // every second packet fails

	r.fill(0, 1, 2, 3)
	r.nic.Deliver(packets(64, 64, 64, 64))
	r.checkNIC()

	got := r.receive(16)
	if len(got) != 2 {
		t.Fatalf("received %d good packets, want 2", len(got))
	}
	failed := r.rx.Failed()
	if len(failed) != 2 {
		t.Fatalf("%d buffers came back from failed packets, want 2", len(failed))
	}
	if r.rx.Outstanding() != 0 {
		t.Errorf("%d buffers still outstanding after every packet was reported", r.rx.Outstanding())
	}
	if r.rx.Stats.Errors.Load() != 2 {
		t.Errorf("counted %d errors, want 2", r.rx.Stats.Errors.Load())
	}
	r.recycle(got)

	// Every frame is accounted for: two given to the application, two straight
	// back to the pool.
	for addr, who := range r.owner {
		if who == "nic" {
			t.Errorf("frame %#x was left with the NIC", addr)
		}
	}
}

// A completion naming a buffer other than the oldest outstanding one means
// software and hardware have lost track of each other. Believing it would hand
// the application a frame the NIC is still writing.
func TestCompletionForTheWrongBufferIsRejected(t *testing.T) {
	r := newRxRig(t, 16, 16, 32, 2048)
	r.fill(0, 1, 2)

	// Write a completion by hand, naming a buffer that is not next in line.
	cq, err := wqe.NewCQ(r.nic.CQBytes(), wqe.CQESize)
	if err != nil {
		t.Fatal(err)
	}
	e := cq.Entry(0)
	for i := range e {
		e[i] = 0
	}
	e[60], e[61] = 0x00, 0x07 // buffer 7, when buffer 0 is next
	e[63] = wqe.CQERespSend<<4 | uint8(cq.ExpectedOwner(0))

	if got := r.rx.Receive(16, nil); len(got) != 0 {
		t.Fatalf("a completion for the wrong buffer produced %d packets", len(got))
	}
	r.rx.Sync()
	if r.rx.Stats.Errors.Load() == 0 {
		t.Error("the mismatch was not counted")
	}
	if r.rx.Outstanding() != 3 {
		t.Errorf("%d buffers outstanding, want the 3 still posted", r.rx.Outstanding())
	}
}

// A completion arriving when nothing is outstanding must be refused rather than
// used to index the record of what was posted.
func TestCompletionWithNothingOutstandingIsRejected(t *testing.T) {
	r := newRxRig(t, 16, 16, 32, 2048)

	cq, err := wqe.NewCQ(r.nic.CQBytes(), wqe.CQESize)
	if err != nil {
		t.Fatal(err)
	}
	e := cq.Entry(0)
	for i := range e {
		e[i] = 0
	}
	e[63] = wqe.CQERespSend<<4 | uint8(cq.ExpectedOwner(0))

	if got := r.rx.Receive(16, nil); len(got) != 0 {
		t.Fatalf("a completion with nothing posted produced %d packets", len(got))
	}
	r.rx.Sync()
	if r.rx.Stats.Errors.Load() == 0 {
		t.Error("the stray completion was not counted")
	}
}

func TestReceiveOnAnEmptyQueue(t *testing.T) {
	r := newRxRig(t, 16, 16, 32, 2048)
	r.fill(0, 1)
	for i := 0; i < 5; i++ {
		if r.rx.Ready() {
			t.Fatal("a completion is claimed to be waiting on an idle queue")
		}
		if got := r.rx.Receive(16, nil); len(got) != 0 {
			t.Fatalf("received %d packets from an idle queue", len(got))
		}
	}
	if r.rx.Stats.Errors.Load() != 0 {
		t.Error("polling an idle queue reported an error")
	}
}

func TestReceivePathDoesNotAllocate(t *testing.T) {
	r := newRxRig(t, 64, 64, 64, 2048)
	descs := make([]packetio.Desc, 32)
	for i := range descs {
		descs[i] = packetio.Desc{Addr: uint64(i * r.frameSize)}
	}
	out := make([]packetio.Desc, 0, 64)
	pkts := packets(64, 64, 64, 64, 64, 64, 64, 64)

	allocs := testing.AllocsPerRun(100, func() {
		r.rx.Fill(descs[:8])
		r.nic.Deliver(pkts)
		out = r.rx.Receive(32, out[:0])
	})
	if allocs != 0 {
		t.Fatalf("the receive path allocated %v times per run, want 0", allocs)
	}
}

func TestNewRxRejectsBadConfigurations(t *testing.T) {
	rq := make([]byte, 8*16)
	cq := make([]byte, 8*wqe.CQESize)
	db := new(uint32)
	base := RxConfig{RQ: rq, CQ: cq, RQDbrec: db, CQDbrec: db, Stride: 16, BufferSize: 2048}

	t.Run("entries longer than one buffer", func(t *testing.T) {
		cfg := base
		cfg.Stride = 64
		if _, err := NewRx(cfg); err == nil {
			t.Error("accepted an entry holding more than one buffer")
		}
	})
	t.Run("no doorbell record", func(t *testing.T) {
		cfg := base
		cfg.RQDbrec = nil
		if _, err := NewRx(cfg); err == nil {
			t.Error("accepted a queue with no doorbell record")
		}
	})
	t.Run("completion queue too small", func(t *testing.T) {
		cfg := base
		cfg.CQ = make([]byte, 4*wqe.CQESize)
		if _, err := NewRx(cfg); err == nil {
			t.Error("accepted a completion queue smaller than the receive queue")
		}
	})
	t.Run("buffers of no bytes", func(t *testing.T) {
		cfg := base
		cfg.BufferSize = 0
		if _, err := NewRx(cfg); err == nil {
			t.Error("accepted buffers of no bytes")
		}
	})
	t.Run("queue size not a power of two", func(t *testing.T) {
		cfg := base
		cfg.RQ = make([]byte, 3*16)
		if _, err := NewRx(cfg); err == nil {
			t.Error("accepted a queue whose size is not a power of two")
		}
	})
}

// An oversize byte count is refused rather than believed. It is the only
// hardware-written field in a completion that used to be taken on trust, and
// believing it hands the caller a descriptor running past its frame -- which on
// a forwarding path becomes an instruction to put the neighbouring frames on
// the wire.
func TestReceiveRefusesAnOversizeByteCount(t *testing.T) {
	r := newRxRig(t, 8, 8, 8, 2048)
	r.fill(0, 1)

	// Deliver one ordinary packet, then rewrite its completion to claim more
	// bytes than the buffer can hold.
	if n := r.nic.Deliver([][]byte{make([]byte, 64)}); n != 1 {
		t.Fatalf("delivered %d packets, want 1", n)
	}
	cq := r.nic.CQBytes()
	binary.BigEndian.PutUint32(cq[44:], 4096) // byte_cnt, twice the frame

	got := r.rx.Receive(4, nil)
	r.rx.Sync()
	for _, d := range got {
		if d.Len > uint32(r.frameSize) {
			t.Errorf("Receive returned a %d-byte descriptor in a %d-byte frame",
				d.Len, r.frameSize)
		}
	}
	if n := r.rx.Stats.Oversize.Load(); n != 1 {
		t.Errorf("Oversize = %d, want 1", n)
	}
	// The frame must still come back rather than leak.
	if len(r.rx.Failed()) != 1 {
		t.Errorf("%d frames put aside as failed, want 1", len(r.rx.Failed()))
	}
}

// A completion the decoder cannot read takes the queue out of service and says
// so, rather than being re-read on every call forever while Receive returns
// nothing and the caller cannot tell why.
func TestReceiveLatchesAnUndecodableCompletion(t *testing.T) {
	r := newRxRig(t, 8, 8, 8, 2048)
	r.fill(0, 1)
	if n := r.nic.Deliver([][]byte{make([]byte, 64)}); n != 1 {
		t.Fatalf("delivered %d packets, want 1", n)
	}
	// Set a compressed format in op_own, which this decoder does not handle.
	cq := r.nic.CQBytes()
	cq[63] |= 1 << 2

	if got := r.rx.Receive(4, nil); len(got) != 0 {
		t.Errorf("Receive returned %d descriptors from an undecodable entry", len(got))
	}
	if err := r.rx.Err(); err == nil {
		t.Fatal("the queue is still reported healthy after an undecodable completion")
	}
	// And it stays failed rather than spinning on the same entry.
	r.rx.Sync()
	before := r.rx.Stats.Errors.Load()
	r.rx.Receive(4, nil)
	r.rx.Sync()
	if after := r.rx.Stats.Errors.Load(); after != before {
		t.Errorf("errors went from %d to %d: the queue is re-reading the same entry", before, after)
	}
}
