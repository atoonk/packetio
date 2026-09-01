//go:build linux && cgo && mlx5 && (amd64 || arm64)

package mlx5

import (
	"os"
	"testing"
)

// Whether each transmit queue gets a doorbell register of its own, or they
// share. Written while chasing the pointer-descriptor plateau of 28 August
// 2026 -- it exonerated the doorbells (eight queues, eight registers) and it
// stays because the question will come back on other hardware. Doorbells are uncached writes to a device register; several cores
// hammering one register serialise on it, which would cap a multi-core
// transmit rate no matter how cheap the packet path is.
func TestUARsPerQueue(t *testing.T) {
	iface := os.Getenv("PACKETIO_IFACE")
	if iface == "" {
		t.Skip("set PACKETIO_IFACE")
	}
	// No WithFrames: the backend sizes the region for the rings it opens.
	// A fixed 8192 predates the refuse-don't-raise rule and the 4-rings-per-
	// queue default, whose 32 rings need four times that; pinning the count
	// here made this diagnostic fail at Open on both counts.
	d, err := Open(iface, WithTxQueues(8), WithRxQueues(0))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	seen := map[uintptr]int{}
	for i := 0; i < d.NumTxQueues(); i++ {
		q := d.TxQueue(i).(*TxQueue)
		for _, dvq := range q.dvqs {
			u := uintptr(dvq.UAR)
			seen[u]++
			t.Logf("queue %d: UAR %#x  bf_size %d  qpn %d", i, u, dvq.BFSize, dvq.QPN)
		}
	}
	t.Logf("%d distinct doorbell registers for %d queues", len(seen), d.NumTxQueues())
	for u, n := range seen {
		if n > 1 {
			t.Logf("  %#x shared by %d queues", u, n)
		}
	}
}
