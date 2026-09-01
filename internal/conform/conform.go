// Package conform is the shared test suite every packetio backend must pass.
//
// The contract in the packetio package doc is prose, and prose does not fail a
// build. Every bug the backends have had in practice was a place where one of
// them quietly meant something different from the interface: frames returned
// to the pool that the caller still owned, a Reclaim that reclaimed nothing, a
// build callback whose zero return meant three different things. Those are the
// cases here.
//
// A backend's own test calls Run with a function that opens a device. It is in
// internal/ because it is a test of this module's backends, not something an
// out-of-tree backend can use -- if that is ever wanted, move it up.
package conform

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/atoonk/packetio"
)

// Required is set by PACKETIO_CONFORM_REQUIRED. When it is on, a suite that
// cannot open a device fails instead of skipping.
//
// This exists because of how AF_XDP went unverified for the whole life of this
// module. Its test opened a device in a way that backend always refuses, so
// every subtest skipped, "go test ./..." printed ok, and the suite reported
// nothing at all -- for months. The first time it actually ran it found a
// contract violation on the first check.
//
// A test that skips silently is indistinguishable from a test that passes, and
// the difference matters most exactly where it is hardest to see: a backend
// that needs hardware. So on a machine that has the hardware, say so, and a
// suite that skips is then a failure rather than a quiet nothing.
func Required() bool { return os.Getenv("PACKETIO_CONFORM_REQUIRED") != "" }

// cannotRun skips, or fails when the suite was required to run. It is for the
// conditions that stop a check part-way -- no traffic, no queue -- which are
// the ones that quietly turn a suite into a decoration.
func cannotRun(t *testing.T, msg string) {
	t.Helper()
	if Required() {
		t.Fatalf("%s, and PACKETIO_CONFORM_REQUIRED is set: this check was supposed "+
			"to run, so this is a failure rather than a skip", msg)
	}
	t.Skip(msg)
}

// cannotOpen skips, or fails when the suite was required to run.
func cannotOpen(t *testing.T) {
	t.Helper()
	const msg = "backend cannot open a device here"
	if Required() {
		t.Fatalf("%s, and PACKETIO_CONFORM_REQUIRED is set: this suite was "+
			"supposed to run. Either the hardware is not what was expected, or the "+
			"open function is wrong -- which is how AF_XDP stayed unverified.", msg)
	}
	t.Skip(msg)
}

// Device is what a backend hands the suite: a freshly opened device with at
// least one transmit queue, plus a way to close it. The suite never transmits
// anything the device is expected to accept, so a loopback or a down link is
// fine; what it checks is bookkeeping.
type Device = packetio.Device

// OpenFunc opens a device for one subtest. It returns nil to skip, for a
// backend that cannot open anything in this environment (no hardware, no
// privilege). Every device it returns is closed by the suite.
type OpenFunc func(t *testing.T) Device

// Run exercises the parts of the contract that every backend shares.
func Run(t *testing.T, open OpenFunc) {
	t.Helper()
	t.Run("AllocFreeRoundTrip", func(t *testing.T) { allocFreeRoundTrip(t, open) })
	t.Run("AllocGivesDistinctFramesInRegion", func(t *testing.T) { allocDistinct(t, open) })
	t.Run("FramesAreConserved", func(t *testing.T) { framesConserved(t, open) })
	t.Run("SendFuncBadLengthAbandonsTheBatch", func(t *testing.T) { sendFuncBadLength(t, open) })
	t.Run("SendFuncZeroLengthStops", func(t *testing.T) { sendFuncZero(t, open) })
	t.Run("ForeignDescriptorIsRefused", func(t *testing.T) { foreignDesc(t, open) })
	t.Run("TransmitRefusesADescriptorOutsideAFrame", func(t *testing.T) { transmitBadDesc(t, open) })
	t.Run("TransmitTakesAPrefix", func(t *testing.T) { transmitPrefix(t, open) })
	t.Run("ReclaimHandsFramesBack", func(t *testing.T) { reclaimWorks(t, open) })
	t.Run("CloseTwice", func(t *testing.T) { closeTwice(t, open) })
	t.Run("MethodsAfterCloseAreSafe", func(t *testing.T) { afterClose(t, open) })
	t.Run("CapabilitiesAreSelfConsistent", func(t *testing.T) { capabilities(t, open) })
	t.Run("CloseWakesAPoller", func(t *testing.T) { closeWakesPoller(t, open) })
	t.Run("ErrIsNilOnAHealthyQueue", func(t *testing.T) { errWhenHealthy(t, open) })
	t.Run("ReceiveStatsCountBatches", func(t *testing.T) { rxBatchesCounted(t, open) })
}

func device(t *testing.T, open OpenFunc) packetio.Device {
	t.Helper()
	d := open(t)
	if d == nil {
		cannotOpen(t)
	}
	t.Cleanup(func() { d.Close() })
	if d.NumTxQueues() == 0 {
		t.Skip("device has no transmit queue")
	}
	return d
}

func allocFreeRoundTrip(t *testing.T, open OpenFunc) {
	d := device(t, open)
	q := d.TxQueue(0)
	before := q.NumFreeFrames()
	if before == 0 {
		t.Fatal("a fresh queue has no free frames")
	}
	descs := q.Alloc(4)
	if len(descs) == 0 {
		t.Fatal("Alloc returned nothing from a full pool")
	}
	if got := q.NumFreeFrames(); got != before-len(descs) {
		t.Errorf("after Alloc(%d): %d free, want %d", len(descs), got, before-len(descs))
	}
	q.Free(descs)
	if got := q.NumFreeFrames(); got != before {
		t.Errorf("after Free: %d free, want %d", got, before)
	}
}

func allocDistinct(t *testing.T, open OpenFunc) {
	d := device(t, open)
	q := d.TxQueue(0)
	r := q.Region()
	descs := q.Alloc(16)
	if len(descs) == 0 {
		t.Skip("no frames to allocate")
	}
	seen := map[uint64]bool{}
	for i, desc := range descs {
		if seen[desc.Addr] {
			t.Fatalf("frame %d handed out twice", desc.Addr)
		}
		seen[desc.Addr] = true
		// The bug this exists for: a queue whose pool was built with the wrong
		// base hands out addresses outside the region. Writable then returns
		// nil and the queue silently does nothing forever.
		w := r.Writable(desc)
		if len(w) == 0 {
			t.Fatalf("descriptor %d (addr %d) is not writable: it is outside the region",
				i, desc.Addr)
		}
		if desc.Len != 0 {
			t.Errorf("Alloc returned a descriptor with Len %d, want 0", desc.Len)
		}
	}
	q.Free(descs)
}

// framesConserved runs the whole cycle many times and checks nothing leaks.
// One leaked frame per round is invisible in a single round.
func framesConserved(t *testing.T, open OpenFunc) {
	d := device(t, open)
	q := d.TxQueue(0)
	before := q.NumFreeFrames()

	for round := 0; round < 16; round++ {
		n, err := q.SendFunc(8, func(i int, frame []byte) int {
			if len(frame) < 64 {
				return 0
			}
			for j := 0; j < 64; j++ {
				frame[j] = byte(j)
			}
			return 64
		})
		if err != nil && !errors.Is(err, packetio.ErrClosed) {
			t.Fatalf("round %d: %v", round, err)
		}
		_ = n
		// Whatever was accepted has to be reclaimable, or the pool drains.
		q.Complete(1 << 20)
	}
	// Anything the device refused is still the caller's, so a queue that
	// cannot send at all ends with fewer free frames -- that is correct, and
	// the test only insists the pool never grows past where it started.
	if got := q.NumFreeFrames(); got > before {
		t.Errorf("the pool grew from %d to %d frames: something was returned twice", before, got)
	}
}

func sendFuncBadLength(t *testing.T, open OpenFunc) {
	d := device(t, open)
	q := d.TxQueue(0)
	before := q.NumFreeFrames()

	n, err := q.SendFunc(8, func(i int, frame []byte) int {
		if i == 3 {
			return len(frame) + 1 // past the end of the frame
		}
		return 64
	})
	if !errors.Is(err, packetio.ErrBadLength) {
		t.Errorf("got %v, want ErrBadLength", err)
	}
	if n != 0 {
		t.Errorf("reported %d packets sent; the batch should have been abandoned", n)
	}
	if got := settle(q); got != before {
		t.Errorf("frames leaked on the error path: %d free plus in flight, want %d", got, before)
	}
}

func sendFuncZero(t *testing.T, open OpenFunc) {
	d := device(t, open)
	q := d.TxQueue(0)
	before := q.NumFreeFrames()

	// Zero means "nothing more to send". It ends the batch and is not an error.
	if _, err := q.SendFunc(8, func(i int, frame []byte) int {
		if i >= 3 {
			return 0
		}
		return 64
	}); err != nil {
		t.Errorf("a zero length must not be an error: %v", err)
	}
	// Every frame is accounted for: free, or still owned by the NIC. On a
	// backend whose transmit is asynchronous the completion may not have
	// arrived yet, and insisting on "all free" would be a race, not a check.
	if got := settle(q); got != before {
		t.Errorf("frames unaccounted for: %d free plus in flight, want %d", got, before)
	}
}

// settle reclaims what it can and returns how many frames are accounted for:
// in the pool, or still with the NIC. A backend whose transmit is synchronous
// settles at once; one that DMAs needs a moment and may need the link to be
// up at all, which is why in-flight frames count as accounted for rather than
// leaked.
func settle(q packetio.TxQueue) int {
	for i := 0; i < 100; i++ {
		q.Complete(1 << 20)
		if q.NumInFlight() == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	return q.NumFreeFrames() + q.NumInFlight()
}

func foreignDesc(t *testing.T, open OpenFunc) {
	d := device(t, open)
	q := d.TxQueue(0)
	before := q.NumFreeFrames()
	// A double return is refused only in a checked build; the range check
	// below is on everywhere. See internal/pool/check_off.go.
	// An address no region ever handed out. Accepting it into the pool would
	// mean a later Alloc returns a frame that is not there.
	q.Free([]packetio.Desc{{Addr: 1 << 40, Len: 64}})
	if got := q.NumFreeFrames(); got > before {
		t.Errorf("a foreign descriptor was taken into the pool: %d free, want at most %d", got, before)
	}
}

// transmitBadDesc checks the rule in BACKENDS.md: a descriptor must name at
// least one byte inside one frame, and a backend refuses the first one that
// does not rather than passing the address to the kernel or the card.
//
// The dangerous case is not an address outside the region, which merely
// crashes. It is a length that runs past the end of its own frame, which is
// silent: the hardware fetches the bytes that follow, so the neighbouring
// frames -- other flows' packets -- go on the wire.
func transmitBadDesc(t *testing.T, open OpenFunc) {
	d := device(t, open)
	q := d.TxQueue(0)
	r := q.Region()
	frame := uint64(r.FrameSize())

	good := q.Alloc(1)
	if len(good) == 0 {
		t.Skip("no frames to allocate")
	}
	base := good[0].Addr
	q.Free(good)

	for _, tc := range []struct {
		name string
		d    packetio.Desc
	}{
		{"past the region", packetio.Desc{Addr: uint64(r.NumFrames()) * frame, Len: 64}},
		{"past its own frame", packetio.Desc{Addr: base, Len: uint32(frame) + 64}},
		{"address wraps", packetio.Desc{Addr: ^uint64(0), Len: 64}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := q.NumFreeFrames()
			if n := q.Transmit([]packetio.Desc{tc.d}); n != 0 {
				t.Errorf("Transmit took %d of a descriptor that is not inside one frame", n)
			}
			if got := q.NumFreeFrames(); got > before {
				t.Errorf("a refused descriptor reached the pool: %d free, want at most %d", got, before)
			}
		})
	}
}

// transmitPrefix checks that what Transmit takes is a prefix, and that the
// frames it did not take still belong to the caller: a backend that quietly
// frees the unaccepted suffix causes a double return the caller cannot see.
func transmitPrefix(t *testing.T, open OpenFunc) {
	d := device(t, open)
	q := d.TxQueue(0)
	r := q.Region()

	before := q.NumFreeFrames()
	descs := q.Alloc(8)
	if len(descs) < 2 {
		t.Skip("not enough frames to allocate")
	}
	for i := range descs {
		descs[i].Len = 64
		copy(r.Writable(descs[i]), make([]byte, 64))
	}
	// A descriptor in the middle that no backend may accept. Everything before
	// it may be taken; nothing at or after it may be.
	bad := len(descs) / 2
	kept := append([]packetio.Desc(nil), descs...)
	descs[bad] = packetio.Desc{Addr: ^uint64(0), Len: 64}

	n := q.Transmit(descs)
	if n > bad {
		t.Errorf("Transmit took %d descriptors, past the bad one at %d", n, bad)
	}
	// The suffix is still ours, so handing it back must be accepted, and the
	// books must balance once the NIC is done with the prefix.
	q.Free(kept[n:])
	// Every frame is either back in this queue's pool or still with the NIC.
	// One that is in neither was dropped; one counted twice was returned twice.
	if total := settle(q); total != before {
		t.Errorf("after a partial Transmit, %d frames accounted for, want %d", total, before)
	}
}

// reclaimWorks checks that Reclaim hands frames to the caller rather than
// returning them to this queue's pool. BACKENDS.md singles it out: a backend
// whose Reclaim returns nothing breaks every forwarder silently, because the
// receive queue never gets its frames back.
//
// A backend whose queues do not share a region is exempt: a frame cannot have
// come from elsewhere, so there is nothing to hand back.
func reclaimWorks(t *testing.T, open OpenFunc) {
	d := device(t, open)
	if !d.Capabilities().SharedRegion {
		t.Skip("queues do not share a region, so Reclaim has nothing to hand back")
	}
	q := d.TxQueue(0)
	r := q.Region()

	// A batch large enough that any backend's driver will have looked at its
	// completions by the end of it.
	//
	// Four was enough for the backends that own their own queue: they read a
	// completion whenever asked. It is not enough for a driver that keeps its
	// own counsel. DPDK's mlx5 poll-mode driver asks the card for a completion
	// only once 32 packets have gone since the last one (MLX5_TX_COMP_THRESH),
	// and until one arrives there is nothing to read however often it is
	// asked -- measured on a ConnectX-6 Dx, where a four-packet batch stayed
	// with the driver indefinitely and a sixty-four-packet one came straight
	// back. That is a property of the hardware and the driver, not a bug in
	// the backend, so the suite sends enough to clear it.
	const batch = 64
	descs := q.Alloc(batch)
	if len(descs) == 0 {
		t.Skip("no frames to allocate")
	}
	for i := range descs {
		descs[i].Len = 64
	}
	n := q.Transmit(descs)
	q.Free(descs[n:])
	if n == 0 {
		t.Skip("the queue accepted nothing to transmit")
	}

	var back []packetio.Desc
	for i := 0; i < 100 && len(back) < n; i++ {
		back = q.Reclaim(1<<20, back)
		if len(back) == n {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(back) == 0 {
		t.Fatalf("Reclaim handed back nothing after transmitting %d frames; a forwarder "+
			"using it would starve its receive queue", n)
	}
	for _, b := range back {
		if r.Writable(b) == nil {
			t.Errorf("Reclaim handed back a descriptor outside the region: %+v", b)
		}
	}
	// They are ours now, so they go back by hand.
	q.Free(back)
}

// closeWakesPoller checks that Close can be called while another goroutine is
// in Poll, which BACKENDS.md requires: without it a program can never shut
// down. It is also where a backend that reads queue memory in Poll and unmaps
// it in Close will fault, which no race detector catches.
func closeWakesPoller(t *testing.T, open OpenFunc) {
	d := open(t)
	if d == nil {
		cannotOpen(t)
	}
	if d.NumRxQueues() == 0 {
		d.Close()
		t.Skip("device has no receive queue")
	}
	q := d.RxQueue(0)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// A long timeout: this must be ended by Close, not by expiry.
		q.Poll(10 * time.Second)
	}()

	// Give the poller time to be inside Poll rather than not yet started.
	time.Sleep(50 * time.Millisecond)
	if err := d.Close(); err != nil {
		t.Errorf("Close while a goroutine polls: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Poll did not return after Close; a program using this backend cannot shut down")
	}
}

// errWhenHealthy checks that Err says nothing is wrong with a queue that has
// just been opened, and says ErrClosed once it is closed. A backend that always
// returns nil, closed or not, gives a caller no way to tell a dead queue from a
// quiet one.
func errWhenHealthy(t *testing.T, open OpenFunc) {
	d := open(t)
	if d == nil {
		cannotOpen(t)
	}
	var tx packetio.TxQueue
	var rx packetio.RxQueue
	if d.NumTxQueues() > 0 {
		tx = d.TxQueue(0)
		if err := tx.Err(); err != nil {
			t.Errorf("a freshly opened transmit queue reports %v", err)
		}
	}
	if d.NumRxQueues() > 0 {
		rx = d.RxQueue(0)
		if err := rx.Err(); err != nil {
			t.Errorf("a freshly opened receive queue reports %v", err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if tx != nil {
		if err := tx.Err(); !errors.Is(err, packetio.ErrClosed) {
			t.Errorf("a closed transmit queue reports %v, want ErrClosed", err)
		}
	}
	if rx != nil {
		if err := rx.Err(); !errors.Is(err, packetio.ErrClosed) {
			t.Errorf("a closed receive queue reports %v, want ErrClosed", err)
		}
	}
}

func closeTwice(t *testing.T, open OpenFunc) {
	d := open(t)
	if d == nil {
		cannotOpen(t)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func afterClose(t *testing.T, open OpenFunc) {
	d := open(t)
	if d == nil {
		cannotOpen(t)
	}
	if d.NumTxQueues() == 0 {
		d.Close()
		t.Skip("device has no transmit queue")
	}
	tx := d.TxQueue(0)
	var rx packetio.RxQueue
	if d.NumRxQueues() > 0 {
		rx = d.RxQueue(0)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// None of these may touch unmapped memory or panic. Returning zero, or an
	// error, are both fine; crashing is not.
	if n := tx.Transmit(tx.Alloc(4)); n != 0 {
		t.Errorf("Transmit after Close sent %d frames", n)
	}
	tx.Free(nil)
	tx.Complete(16)
	_ = tx.Reclaim(16, nil)
	_, _ = tx.Stats()
	if rx != nil {
		if _, err := rx.Poll(0); err == nil {
			t.Error("Poll after Close returned no error")
		}
		if got := rx.Receive(16); len(got) != 0 {
			t.Errorf("Receive after Close returned %d descriptors", len(got))
		}
		rx.Recycle(nil)
		_, _ = rx.Stats()
	}
}

func capabilities(t *testing.T, open OpenFunc) {
	d := device(t, open)
	c := d.Capabilities()
	if c.Backend == "" {
		t.Error("Capabilities.Backend is empty")
	}
	if c.MaxFrameSize <= 0 {
		t.Errorf("MaxFrameSize is %d", c.MaxFrameSize)
	}
	// MaxQueues is a ceiling, not the number opened.
	if c.MaxQueues < d.NumTxQueues() || c.MaxQueues < d.NumRxQueues() {
		t.Errorf("MaxQueues %d is below the %d tx / %d rx queues actually open",
			c.MaxQueues, d.NumTxQueues(), d.NumRxQueues())
	}
	if c.RSS && d.NumRxQueues() < 2 {
		t.Error("RSS is true with fewer than two receive queues")
	}
	r := d.TxQueue(0).Region()
	if r.FrameSize() <= 0 || r.NumFrames() <= 0 {
		t.Errorf("region reports %d frames of %d bytes", r.NumFrames(), r.FrameSize())
	}
	if got := len(r.Bytes()); got < r.NumFrames()*r.FrameSize() {
		t.Errorf("region is %d bytes, too small for %d frames of %d",
			got, r.NumFrames(), r.FrameSize())
	}
	// A descriptor naming nothing must be refused, not panic.
	if f := r.Frame(packetio.Desc{Addr: 1 << 40, Len: 64}); f != nil {
		t.Error("Region.Frame accepted an address outside the region")
	}
	if w := r.Writable(packetio.Desc{Addr: 1 << 40}); w != nil {
		t.Error("Region.Writable accepted an address outside the region")
	}
}

// RunSteering checks a backend's handling of receive filters: a filter it
// accepts is reported, and one it cannot express is refused at Open with
// ErrUnsupported rather than installed as something wider. openWith opens a
// device with the given filter, returning nil to skip and the error Open gave.
func RunSteering(t *testing.T, openWith func(t *testing.T, f packetio.SteeringFilter) (Device, error)) {
	t.Helper()
	t.Run("PromiscuousIsAccepted", func(t *testing.T) {
		d, err := openWith(t, packetio.SteeringFilter{Promiscuous: true})
		if d == nil && err == nil {
			t.Skip("backend cannot open a device here")
		}
		if err != nil {
			t.Fatalf("a promiscuous filter was refused: %v", err)
		}
		d.Close()
	})
	t.Run("ContradictionIsRefused", func(t *testing.T) {
		// A bare port with no protocol means nothing on any backend.
		d, err := openWith(t, packetio.SteeringFilter{Match: []packetio.Match{{Kind: packetio.MatchKindDstPort, Port: 9}}})
		if d != nil {
			d.Close()
		}
		if d == nil && err == nil {
			t.Skip("backend cannot open a device here")
		}
		if !errors.Is(err, packetio.ErrUnsupported) {
			t.Errorf("got %v, want ErrUnsupported", err)
		}
	})
}

// A receive queue must count the calls that returned packets, so that packets
// divided by batches is the effective batch size.
//
// This is the counter whose absence hid a bug that cost one backend half its
// receive rate: its bursts had collapsed to a handful of packets each, and
// nothing in the statistics said so -- packets, bytes and drops look identical
// whether they arrived sixty at a time or five. A backend that receives
// without reporting batches leaves its callers with no way to see that.
func rxBatchesCounted(t *testing.T, open OpenFunc) {
	d := device(t, open)
	if d.NumRxQueues() == 0 {
		t.Skip("no receive queues")
	}
	rx := d.RxQueue(0)

	before, err := rx.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	// Nothing may arrive here -- a quiet link is the normal case for this
	// suite -- so what is checked is the relationship, not a rate: every
	// backend that delivered packets must have counted the calls that
	// delivered them.
	rx.Fill(rx.NumFreeFillSlots())
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		descs := rx.Receive(64)
		if len(descs) > 0 {
			rx.Recycle(descs)
		}
		rx.Fill(rx.NumFreeFillSlots())
	}

	after, err := rx.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if after.Packets < before.Packets || after.Batches < before.Batches {
		// Some backends read the kernel's counters destructively, so a
		// difference is not always meaningful. Say so rather than subtract
		// into a very large number and assert on it.
		t.Skip("this backend's receive counters do not accumulate")
	}
	got := after.Packets - before.Packets
	batches := after.Batches - before.Batches
	if got == 0 {
		// Nothing arrived, which is the normal case for this suite on a quiet
		// link. A skip rather than a silent pass: the difference matters when
		// someone reads the output and believes the counter was exercised.
		t.Skip("no packets arrived, so there is nothing to count")
	}
	if batches == 0 {
		t.Errorf("%d packets received and no batches counted; RxStats.Batches "+
			"is how a caller sees the effective batch size", got)
	}
	if batches > got {
		t.Errorf("%d batches for %d packets: a batch is a call that returned "+
			"packets, so it cannot exceed them", batches, got)
	}
}

// RunTimestamps checks a backend that claims Capabilities.RxTimestamps: the
// interface is really there, and the times it reports are usable.
//
// It needs traffic, which the suite cannot make on its own, so send is the
// caller's: it puts packets on the wire and returns how many, or zero if it
// cannot. A run that sees nothing skips rather than passes, on the rule that a
// skipped test and a passing test must not print the same word.
func RunTimestamps(t *testing.T, open OpenFunc, send func(t *testing.T, d Device) int) {
	t.Helper()
	d := open(t)
	if d == nil {
		cannotOpen(t)
		return
	}
	defer d.Close()

	if d.NumRxQueues() == 0 {
		cannotRun(t, "device has no receive queue")
		return
	}
	if !d.Capabilities().RxTimestamps {
		// Not claiming it is a complete answer; claiming it and not having it
		// is what this checks. A queue may still carry the method -- Go
		// interfaces are satisfied by the type, not the device -- but then it
		// must refuse rather than invent times, which is checked below.
		if rq, ok := d.RxQueue(0).(packetio.TimestampReceiver); ok {
			if descs, ts := rq.ReceiveTimestamps(8); descs != nil || ts != nil {
				t.Error("the device reports no timestamps, but ReceiveTimestamps returned some anyway")
			}
		}
		t.Skip("backend does not stamp received packets")
	}
	rq, ok := d.RxQueue(0).(packetio.TimestampReceiver)
	if !ok {
		t.Fatal("Capabilities.RxTimestamps is true but the queue is not a TimestampReceiver")
	}
	rq.Fill(rq.NumFreeFillSlots())

	if send(t, d) == 0 {
		cannotRun(t, "no traffic to timestamp")
		return
	}

	var descs []packetio.Desc
	var ts []uint64
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rq.Poll(100 * time.Millisecond)
		if descs, ts = rq.ReceiveTimestamps(64); len(descs) > 0 {
			break
		}
		rq.Fill(rq.NumFreeFillSlots())
	}
	if len(descs) == 0 {
		cannotRun(t, "nothing arrived to timestamp")
		return
	}
	defer func() {
		if descs != nil {
			rq.Recycle(descs)
		}
	}()

	if len(ts) != len(descs) {
		t.Fatalf("%d timestamps for %d descriptors: the slices must match", len(ts), len(descs))
	}
	inversions := 0
	for i, v := range ts {
		// Zero is the one value a timestamp may never take: it would say a
		// packet arrived at the epoch, and a caller cannot tell it from a
		// backend that filled nothing in.
		if v == 0 {
			t.Errorf("packet %d has a zero timestamp", i)
		}
		if i > 0 && v < ts[i-1] {
			inversions++
		}
	}
	// Arrival order is not delivery order: a NIC stamps at the port and
	// places afterwards, so a card that is dropping delivers some packets out
	// of stamp order. A few inversions are the hardware being honest. Most of
	// them being inversions is a backend reading the wrong field, which is the
	// failure worth catching.
	if len(ts) > 4 && inversions > len(ts)/2 {
		t.Errorf("%d of %d timestamps go backwards: this is not a clock", inversions, len(ts))
	}
	// The plain path must still work alongside the extended one. It shares
	// the queue's scratch, so a backend that got the sharing wrong hands back
	// descriptors that do not match what it just reported.
	rq.Recycle(descs)
	descs = nil
	rq.Fill(rq.NumFreeFillSlots())
	if again := rq.Receive(8); again == nil && rq.Err() != nil {
		t.Errorf("Receive stopped working after ReceiveTimestamps: %v", rq.Err())
	} else {
		rq.Recycle(again)
	}
}
