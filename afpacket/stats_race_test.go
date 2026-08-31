//go:build linux

package afpacket

import "testing"

// A worker drives the queue while this goroutine reads Stats and Err, which
// is what every example's report loop does. The test exists to fail under
// -race the moment any counter goes back to being a plain integer.
func TestStatsAreSafeWhileTheQueueRuns(t *testing.T) {
	r := testRegion(t, 16, 2048)
	q := testTx(t, r, 0, 16)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			descs := q.Alloc(4)
			for j := range descs {
				descs[j].Len = 64
			}
			// fd is -1, so sendmmsg fails; a failed batch still moves every
			// counter Stats reads, which is the point.
			q.Transmit(descs)
			q.Free(q.Alloc(2))
			q.Complete(16)
		}
	}()
	for reading := true; reading; {
		select {
		case <-done:
			reading = false
		default:
		}
		if _, err := q.Stats(); err != nil {
			t.Errorf("Stats: %v", err)
			return
		}
		_ = q.Err()
		_ = q.LastErrno()
	}
}
