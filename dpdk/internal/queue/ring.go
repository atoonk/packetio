// Package queue holds the frame-ownership logic of the DPDK backend, with no
// cgo and no DPDK in sight.
//
// Everything that decides who owns a frame lives here: what Alloc may hand out,
// what Transmit accepts, when a frame goes back on the free list, and which
// free list it goes back to. That is the part of a backend where the bugs are --
// a frame in two places at once is invisible in every counter -- so it is the
// part written against a fake driver and tested without hardware.
//
// The driver appears as the PMD interface. In production it is one cgo call per
// batch; in the tests it is a Go type that behaves like a real poll-mode driver,
// including the awkward parts: taking a prefix of a burst rather than all of it,
// freeing transmitted buffers only when it feels like it, and pulling receive
// buffers out of a pool in all-or-nothing bulks.
package queue

import "fmt"

// Ring is one of the two arrays that stand between the PMD and this backend's
// free list: the supply, which Go fills and the driver drains, and the
// returned, which the driver fills and Go drains. Each holds mbuf addresses.
//
// It is deliberately not synchronised. In production each ring has exactly one
// producer and one consumer, and both run on the queue's own goroutine: the
// driver only ever touches a ring from inside a burst call that this goroutine
// made, so the two never run at the same time. Atomics here would cost a locked
// instruction per frame to order accesses that cannot race.
//
// In the real backend the backing array is C memory the mempool ops write to,
// viewed from Go as a slice, and so are the indices, which is why they are
// pointers. Nothing in this type cares which it is.
//
// The indices are one word each because in production they live in C memory,
// beside the array, where the mempool ops read and write them. Each index has
// exactly one writer -- Go moves the supply's tail and the returned ring's
// head, the driver moves the other two -- and both sides run on the queue's own
// thread, so plain loads and stores are enough and no atomic is needed.
type Ring struct {
	buf  []uint64
	mask uint32
	head *uint32 // consumer: the next slot to read
	tail *uint32 // producer: the next slot to write
}

// NewRing builds a ring holding capacity addresses, which must be a power of
// two so that wrapping is a mask. Sizing is the caller's problem and is not a
// tuning knob: a ring too small to hold everything the driver may hand back
// loses frames, so the backend sizes both from the queue depth at Open and
// never grows them on the packet path.
func NewRing(capacity int) (*Ring, error) {
	if capacity <= 0 || capacity&(capacity-1) != 0 {
		return nil, fmt.Errorf("dpdk: a ring of %d entries, which must be a power of two", capacity)
	}
	var head, tail uint32
	return &Ring{buf: make([]uint64, capacity), mask: uint32(capacity - 1),
		head: &head, tail: &tail}, nil
}

// NewRingOver builds a ring over memory and indices the caller already has,
// which is how the real backend shares one with the mempool ops in C.
func NewRingOver(buf []uint64, head, tail *uint32) (*Ring, error) {
	if len(buf) == 0 || len(buf)&(len(buf)-1) != 0 {
		return nil, fmt.Errorf("dpdk: a ring of %d entries, which must be a power of two", len(buf))
	}
	if head == nil || tail == nil {
		return nil, fmt.Errorf("dpdk: a ring without its indices")
	}
	return &Ring{buf: buf, mask: uint32(len(buf) - 1), head: head, tail: tail}, nil
}

// Len is how many addresses are waiting, and Cap how many the ring holds.
func (r *Ring) Len() int { return int(*r.tail - *r.head) }

// Cap is how many addresses the ring can hold at once.
func (r *Ring) Cap() int { return len(r.buf) }

// Room is how many more addresses Push will accept.
func (r *Ring) Room() int { return len(r.buf) - r.Len() }

// Push adds addresses and returns how many it took, always a prefix. A short
// return means the ring is full, which for the returned ring would mean losing
// a frame -- so the backend sizes it so that cannot happen and counts it if it
// does.
func (r *Ring) Push(addrs []uint64) int {
	n := len(addrs)
	if room := r.Room(); n > room {
		n = room
	}
	for i := 0; i < n; i++ {
		r.buf[*r.tail&r.mask] = addrs[i]
		*r.tail++
	}
	return n
}

// PushOne adds one address and reports whether there was room.
func (r *Ring) PushOne(addr uint64) bool {
	if r.Room() == 0 {
		return false
	}
	r.buf[*r.tail&r.mask] = addr
	*r.tail++
	return true
}

// Pop removes up to n addresses, appending them to dst, and returns the grown
// slice. It allocates nothing when dst has the capacity.
func (r *Ring) Pop(n int, dst []uint64) []uint64 {
	if have := r.Len(); n > have {
		n = have
	}
	for i := 0; i < n; i++ {
		dst = append(dst, r.buf[*r.head&r.mask])
		*r.head++
	}
	return dst
}

// PopBulk removes exactly n addresses into dst, or none at all, and reports
// whether it did.
//
// All or nothing, because that is what rte_mempool_get_bulk promises the driver
// and what the driver relies on: mlx5's vectorised receive path asks for a
// whole replenishment batch and treats a short answer as no answer, dropping
// packets rather than taking what there is. A supply that hands out a partial
// bulk would be a subtly different pool from the one DPDK expects.
func (r *Ring) PopBulk(dst []uint64, n int) bool {
	if r.Len() < n {
		return false
	}
	for i := 0; i < n; i++ {
		dst[i] = r.buf[*r.head&r.mask]
		*r.head++
	}
	return true
}
