package queue

import (
	"errors"
	"testing"

	"github.com/atoonk/packetio"
)

// An ownership violation is not a counter: it kills the queue. A foreign
// completion leaves inFlight one too high forever, so after one the queue
// must say so through Dead and refuse further work, not shrink quietly.
func TestForeignCompletionLatchesTheQueue(t *testing.T) {
	r := newRig(t, 64)
	q := r.tx(t, poolA, 0, 64, 64)

	descs := append([]packetio.Desc(nil), q.Alloc(4)...)
	for i := range descs {
		descs[i].Len = 64
	}

	q.cfg.Returned.PushOne(testRegionVA + 1<<40)
	q.Complete(16)

	if err := q.Dead(); !errors.Is(err, packetio.ErrQueueFailed) {
		t.Fatalf("Dead() = %v, want ErrQueueFailed", err)
	}
	if n := q.Transmit(descs); n != 0 {
		t.Errorf("Transmit on a failed queue sent %d", n)
	}
	if got := q.Alloc(4); got != nil {
		t.Errorf("Alloc on a failed queue returned %d descriptors", len(got))
	}
}

// The receive side of the same rule: a delivered address from outside the
// region latches the queue, and Receive and Fill refuse from then on.
func TestForeignReceiveLatchesTheQueue(t *testing.T) {
	r := newRig(t, 64)
	q := r.rx(t, poolB, 0, 64, 32)
	q.cfg.PMD = &oneShotPMD{va: testRegionVA + 1<<40}

	if got := q.Receive(4); len(got) != 0 {
		t.Errorf("Receive delivered %d foreign packets", len(got))
	}
	if q.Stats.Foreign.Load() != 1 {
		t.Errorf("Foreign = %d, want 1", q.Stats.Foreign.Load())
	}
	if err := q.Dead(); !errors.Is(err, packetio.ErrQueueFailed) {
		t.Fatalf("Dead() = %v, want ErrQueueFailed", err)
	}
	if got := q.Receive(4); got != nil {
		t.Errorf("Receive on a failed queue returned %d", len(got))
	}
	if n := q.Fill(4); n != 0 {
		t.Errorf("Fill on a failed queue posted %d", n)
	}
}

// And the third door the driver can push a foreign address through.
func TestForeignReturnLatchesTheQueue(t *testing.T) {
	r := newRig(t, 64)
	q := r.rx(t, poolB, 0, 64, 32)

	q.cfg.Returned.PushOne(0x11)
	if n := q.DrainReturned(); n != 0 {
		t.Errorf("DrainReturned took back %d foreign frames", n)
	}
	if err := q.Dead(); !errors.Is(err, packetio.ErrQueueFailed) {
		t.Fatalf("Dead() = %v, want ErrQueueFailed", err)
	}
}

// A worker drives both queues while this goroutine reads every counter,
// which is what every example's report loop does. The test exists to fail
// under -race the moment any counter goes back to being a plain uint64.
func TestStatsAreSafeWhileTheQueueRuns(t *testing.T) {
	r := newRig(t, 256)
	tx := r.tx(t, poolA, 0, 128, 64)
	rx := r.rx(t, poolB, 128, 128, 64)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			descs := tx.Alloc(8)
			for j := range descs {
				descs[j].Len = 64
			}
			tx.Transmit(descs)
			tx.Complete(64)
			rx.Fill(8)
			rx.Recycle(rx.Receive(8))
			rx.DrainReturned()
		}
	}()
	for reading := true; reading; {
		select {
		case <-done:
			reading = false
		default:
		}
		_ = tx.Stats.Packets.Load() + tx.Stats.Bytes.Load() + tx.Stats.Completed.Load() +
			tx.Stats.Batches.Load() + tx.Stats.RingFull.Load() + tx.Stats.PoolEmpty.Load() +
			tx.Stats.BadDesc.Load() + tx.Stats.BadOffload.Load() + tx.Stats.Pokes.Load() +
			tx.Stats.Foreign.Load()
		_ = rx.Stats.Packets.Load() + rx.Stats.Bytes.Load() + rx.Stats.Filled.Load() +
			rx.Stats.Batches.Load() + rx.Stats.PoolEmpty.Load() + rx.Stats.Chained.Load() +
			rx.Stats.BadLen.Load() + rx.Stats.Foreign.Load()
		// Everything the dpdk wrapper's Stats reads besides the counters.
		_ = tx.NumInFlight() + tx.NumFreeSlots() + rx.NumOutstanding()
		_ = tx.PoolRejected() + rx.PoolRejected()
		_ = tx.Dead()
		_ = rx.Dead()
	}
	if err := tx.Dead(); err != nil {
		t.Errorf("the worker latched a failure: %v", err)
	}
	if err := rx.Dead(); err != nil {
		t.Errorf("the receive worker latched a failure: %v", err)
	}
}
