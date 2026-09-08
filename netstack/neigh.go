//go:build linux

package netstack

import (
	"sync"
	"sync/atomic"
	"time"
)

// neighbors is the IPv4-to-MAC table: entries the caller seeded through
// Config.Neighbors or AddNeighbor, which are pinned, and entries learned
// from frames addressed to the stack. Reads happen on every received frame
// (learn) and every transmitted one (AddHeader), from every queue; writes
// only when a peer is first heard from or changes MAC. So the table is
// copy-on-write: readers load a pointer to an immutable map, no lock and no
// shared cache line written on the packet path; a writer copies the map
// under its mutex and publishes the new one.
//
// Learning is unauthenticated, as ARP is: a host on the link can claim
// another's address by sending a frame from it. What the table does about
// that is bound the damage: it learns only from frames addressed to the
// stack, from one host on its own prefix; a pinned entry is never
// overwritten or evicted, so the gateway given in Config stays the gateway;
// and a learned write that replaces an entry is throttled to one per
// writeInterval, tested before the lock, so a flood of forged sources costs
// a bounded amount of work rather than a table copy, and a lock, per frame.
type neighbors struct {
	lastWrite atomic.Int64 // when the table last took a write, for writeInterval
	m         atomic.Pointer[map[[4]byte]neighbor]
	mu        sync.Mutex // writers only
}

type neighbor struct {
	mac    [6]byte
	pinned bool // seeded by the caller: never learned over, never evicted
}

func newNeighbors() *neighbors {
	n := &neighbors{}
	m := make(map[[4]byte]neighbor)
	n.m.Store(&m)
	return n
}

// maxNeighbors bounds the table. A server on one subnet has a handful of
// peers (or one router); on a routed network the table holds one entry per
// client address. Past the bound a learned entry is evicted for each new
// one, at most one per writeInterval. An evicted peer is relearned from its
// next frame.
const (
	maxNeighbors  = 4096
	writeInterval = 10 * time.Millisecond
)

// learn records mac as the way to reach ip, unless ip is pinned. It reports
// whether the table changed, and which entry, if any, was evicted to make
// room, so the stack's own neighbor cache can be kept in step.
//
// The packet path is one pointer load and a map lookup. A write that
// replaces something -- a changed MAC, or an eviction from a full table --
// is throttled, and the throttle is tested here, before the mutex: a flood
// of frames with forged sources must not queue every receive goroutine
// behind one lock, which costs far more than the copying the throttle was
// written to bound. A change refused here is not lost for good, since the
// peer's next frame after the interval takes it.
//
// A first sighting of a peer is not throttled: its reply would otherwise go
// to broadcast for want of an entry. That path is bounded by maxNeighbors,
// after which every new address is an eviction and throttled with the rest.
func (n *neighbors) learn(ip [4]byte, mac [6]byte) (changed bool, evicted [4]byte, didEvict bool) {
	m := *n.m.Load()
	have, known := m[ip]
	if known && (have.mac == mac || have.pinned) {
		return false, evicted, false
	}
	if known || len(m) >= maxNeighbors {
		if time.Now().UnixNano()-n.lastWrite.Load() < int64(writeInterval) {
			return false, evicted, false
		}
	}
	return n.write(ip, neighbor{mac: mac})
}

// pin records mac as the way to reach ip and keeps it there: learning will
// not change it and a full table will not evict it. It is the caller's own
// act, so it is not throttled.
func (n *neighbors) pin(ip [4]byte, mac [6]byte) (changed bool, evicted [4]byte, didEvict bool) {
	return n.write(ip, neighbor{mac: mac, pinned: true})
}

func (n *neighbors) write(ip [4]byte, nb neighbor) (changed bool, evicted [4]byte, didEvict bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	old := *n.m.Load()
	have, known := old[ip]
	if known && have == nb {
		return false, evicted, false
	}
	if known && have.pinned && !nb.pinned {
		return false, evicted, false
	}
	now := time.Now().UnixNano()
	evict := !known && len(old) >= maxNeighbors
	if !nb.pinned && (known || evict) {
		// The authoritative test, since learn's is outside the lock.
		if now-n.lastWrite.Load() < int64(writeInterval) {
			return false, evicted, false
		}
	}
	m := make(map[[4]byte]neighbor, len(old)+1)
	for k, v := range old {
		if evict && !v.pinned {
			evict = false // drop the first unpinned entry the map hands out
			evicted, didEvict = k, true
			continue
		}
		m[k] = v
	}
	if evict {
		return false, evicted, false // every entry is pinned; nothing to make room with
	}
	m[ip] = nb
	n.m.Store(&m)
	n.lastWrite.Store(now)
	return true, evicted, didEvict
}

func (n *neighbors) lookup(ip [4]byte) ([6]byte, bool) {
	nb, ok := (*n.m.Load())[ip]
	return nb.mac, ok
}

func (n *neighbors) size() int { return len(*n.m.Load()) }
