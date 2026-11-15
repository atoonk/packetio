//go:build linux && cgo && dpdk && amd64

package dpdk_test

import (
	"testing"
	"time"

	"github.com/atoonk/packetio"
)

// A receiver that waits with Poll gets its packets through the stash: Poll
// has to take them to learn that they are there. Every one of them must come
// out of Receive exactly once and in the order the driver gave them, also
// when the caller asks for fewer than Poll found.
func TestPollThenReceiveLosesNothing(t *testing.T) {
	needRoot(t)
	d, err := open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rx := d.RxQueue(0)
	if rx == nil {
		t.Skip("no receive queue")
	}
	n, err := rx.Poll(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if n < 4 {
		t.Skipf("Poll found %d packets; this device delivers too few to split", n)
	}
	seen := map[uint64]bool{}
	take := func(max, want int) []packetio.Desc {
		t.Helper()
		got := rx.Receive(max)
		if len(got) != want {
			t.Fatalf("Receive(%d) after a Poll that found %d gave %d packets, want %d", max, n, len(got), want)
		}
		for _, x := range got {
			if seen[x.Addr] {
				t.Fatalf("frame %#x was handed out twice", x.Addr)
			}
			seen[x.Addr] = true
		}
		return got
	}
	first := append([]packetio.Desc(nil), take(n/2, n/2)...)
	held := rx.Receive(0) // a no-op, and must not disturb what is waiting
	if len(held) != 0 {
		t.Fatalf("Receive(0) returned %d packets", len(held))
	}
	rx.Recycle(first)
	// The rest of what Poll found, and no more than that: Receive does not
	// mix stashed packets with a fresh burst.
	rest := append([]packetio.Desc(nil), take(n, n-n/2)...)
	rx.Recycle(rest)
	if len(seen) != n {
		t.Fatalf("Poll found %d packets, Receive handed out %d", n, len(seen))
	}
}

// What Receive returned stays the caller's until the next Receive, so a Poll
// in between (a worker checking for more before it parks) must not change it.
func TestPollDoesNotDisturbWhatReceiveReturned(t *testing.T) {
	needRoot(t)
	d, err := open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rx := d.RxQueue(0)
	if rx == nil {
		t.Skip("no receive queue")
	}
	n, err := rx.Poll(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if n < 4 {
		t.Skipf("Poll found %d packets", n)
	}
	got := rx.Receive(n / 2)
	want := append([]packetio.Desc(nil), got...)
	if _, err := rx.Poll(0); err != nil {
		t.Fatal(err)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("packet %d changed under the caller after a Poll: %+v, was %+v", i, got[i], want[i])
		}
	}
	rx.Recycle(got)
	for {
		more := rx.Receive(64)
		if len(more) == 0 {
			break
		}
		rx.Recycle(more)
		if n -= len(more); n < -4096 {
			break // a device that never runs dry (net_null): enough
		}
	}
}

// Poll, Receive, Recycle is the loop of a worker that waits for traffic. It
// runs once per burst, so it must not allocate: at 15 Mpps a slice per round
// was 240 MB/s of garbage and a collection every few seconds, each one a
// burst of drops.
func TestPollThenReceiveDoesNotAllocate(t *testing.T) {
	needRoot(t)
	d, err := open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rx := d.RxQueue(0)
	if rx == nil {
		t.Skip("no receive queue")
	}
	if n, err := rx.Poll(2 * time.Second); err != nil || n == 0 {
		t.Skipf("no traffic on this device (n=%d err=%v)", n, err)
	}
	packets, rounds := 0, 0
	round := func() {
		rx.Fill(rx.NumFreeFillSlots())
		n, err := rx.Poll(0)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
		// One Receive takes everything a Poll found (it looks at one batch).
		got := rx.Receive(256)
		packets += len(got)
		rounds++
		rx.Recycle(got)
	}
	for i := 0; i < 64; i++ { // let both arrays reach their size
		round()
	}
	packets, rounds = 0, 0
	a := testing.AllocsPerRun(200, round)
	// The measurement means something only if the rounds carried packets:
	// with the supply empty every round is a Poll that finds nothing.
	if rounds < 100 {
		t.Skipf("only %d of 201 rounds found packets (%d in all); nothing was measured", rounds, packets)
	}
	if a != 0 {
		t.Fatalf("a Poll, Receive, Recycle round allocates %.1f times, want 0 (%d rounds, %d packets)", a, rounds, packets)
	}
}
