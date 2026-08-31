package forward

import "encoding/binary"

// The forwarding table and the rewrite it resolves to. Both are deliberately
// plain, so they can be lifted into another program unchanged.
//
// The table is a 16-8-8 multibit trie: a 65536-slot root indexed by the top 16
// bits of the destination, then up to two 256-slot plies for the next two
// bytes. A route of /16 or shorter costs one array load per packet, /17 to /24
// two, and /25 to /32 three. Inserting expands a route over every slot it
// covers, and never overwrites a slot that a longer prefix already owns, so
// insertion order does not matter. There is no delete: the table here is built
// once from the command line.

// A leaf is 0 for no route, an even number for an adjacency index shifted left
// by one, and an odd number for a child ply index shifted left by one.
const leafEmpty = uint32(0)

func leafAdj(adj uint32) uint32  { return adj << 1 }
func leafPly(ply uint32) uint32  { return ply<<1 | 1 }
func leafIsPly(l uint32) bool    { return l&1 == 1 }
func leafGetPly(l uint32) uint32 { return l >> 1 }

type ply struct {
	leaves [256]uint32
	plens  [256]uint8 // the prefix length that wrote each leaf
}

// V4Fib is one IPv4 forwarding table.
type V4Fib struct {
	rootLeaves []uint32 // 65536 slots, indexed by the top 16 bits
	rootPlens  []uint8
	plies      []*ply
	routes     int
}

func NewV4Fib() *V4Fib {
	return &V4Fib{
		rootLeaves: make([]uint32, 1<<16),
		rootPlens:  make([]uint8, 1<<16),
	}
}

// Lookup is the longest-prefix match: one to three dependent loads. It returns
// an adjacency index, or 0 when there is no route.
func (f *V4Fib) Lookup(dst uint32) uint32 {
	l := f.rootLeaves[dst>>16]
	if l&1 == 0 {
		return l >> 1
	}
	return f.lookupPlies(dst, l)
}

func (f *V4Fib) lookupPlies(dst uint32, l uint32) uint32 {
	p := f.plies[l>>1]
	l = p.leaves[(dst>>8)&0xff]
	if l&1 == 0 {
		return l >> 1
	}
	p = f.plies[l>>1]
	return p.leaves[dst&0xff] >> 1
}

// Insert installs prefix/plen -> adj. adj must not be 0, which means no route.
func (f *V4Fib) Insert(prefix uint32, plen uint8, adj uint32) {
	prefix &= plenMask(plen)
	f.routes++
	f.walk(prefix, plen, func(leaves []uint32, plens []uint8, i int) {
		if plens[i] <= plen || leaves[i] == leafEmpty {
			if leafIsPly(leaves[i]) {
				// Keep the ply and cover what it holds that is less specific.
				f.coverPly(leafGetPly(leaves[i]), adj, plen)
			} else {
				leaves[i] = leafAdj(adj)
				plens[i] = plen
			}
		}
	})
}

// Routes is how many routes were inserted.
func (f *V4Fib) Routes() int { return f.routes }

func plenMask(plen uint8) uint32 {
	if plen == 0 {
		return 0
	}
	return ^uint32(0) << (32 - plen)
}

// coverPly rewrites the slots of a child ply owned by a prefix no longer than
// plen, recursively.
func (f *V4Fib) coverPly(plyIdx uint32, adj uint32, plen uint8) {
	p := f.plies[plyIdx]
	for i := 0; i < 256; i++ {
		if p.plens[i] <= plen || p.leaves[i] == leafEmpty {
			if leafIsPly(p.leaves[i]) {
				f.coverPly(leafGetPly(p.leaves[i]), adj, plen)
			} else {
				p.leaves[i] = leafAdj(adj)
				p.plens[i] = plen
			}
		}
	}
}

// walk visits every slot the prefix covers at its natural level of the trie,
// creating child plies as needed.
func (f *V4Fib) walk(prefix uint32, plen uint8, fn func(leaves []uint32, plens []uint8, i int)) {
	switch {
	case plen <= 16:
		base := prefix >> 16
		n := 1 << (16 - plen)
		for i := 0; i < n; i++ {
			fn(f.rootLeaves, f.rootPlens, int(base)+i)
		}
	case plen <= 24:
		p := f.childPly(f.rootLeaves, f.rootPlens, int(prefix>>16))
		base := (prefix >> 8) & 0xff
		n := 1 << (24 - plen)
		for i := 0; i < n; i++ {
			fn(p.leaves[:], p.plens[:], int(base)+i)
		}
	default:
		p1 := f.childPly(f.rootLeaves, f.rootPlens, int(prefix>>16))
		p2 := f.childPly(p1.leaves[:], p1.plens[:], int((prefix>>8)&0xff))
		base := prefix & 0xff
		n := 1 << (32 - plen)
		for i := 0; i < n; i++ {
			fn(p2.leaves[:], p2.plens[:], int(base)+i)
		}
	}
}

// childPly returns the ply behind slot i, creating it if the slot holds a
// leaf: the leaf is pushed down into all 256 slots of the new ply.
func (f *V4Fib) childPly(leaves []uint32, plens []uint8, i int) *ply {
	if leafIsPly(leaves[i]) {
		return f.plies[leafGetPly(leaves[i])]
	}
	p := &ply{}
	idx := uint32(len(f.plies))
	f.plies = append(f.plies, p)
	if leaves[i] != leafEmpty {
		for j := 0; j < 256; j++ {
			p.leaves[j] = leaves[i]
			p.plens[j] = plens[i]
		}
	}
	leaves[i] = leafPly(idx)
	plens[i] = 0
	return p
}

// Adjacency is what a route resolves to: the Ethernet header, with an optional
// 802.1Q tag, that goes on every packet sent through it. Adjacency 0 is
// reserved to mean no route.
type Adjacency struct {
	L2    [ethHeaderLen + vlanTagLen]byte
	L2Len uint8
}

// NewAdjacency pre-forms the egress header: destination, source, an 802.1Q
// tag if vlan is not 0, and the IPv4 ethertype.
func NewAdjacency(dst, src [6]byte, vlan uint16) Adjacency {
	var a Adjacency
	copy(a.L2[0:6], dst[:])
	copy(a.L2[6:12], src[:])
	n := 12
	if vlan != 0 {
		binary.BigEndian.PutUint16(a.L2[n:], etherTypeVLAN)
		binary.BigEndian.PutUint16(a.L2[n+2:], vlan)
		n += vlanTagLen
	}
	binary.BigEndian.PutUint16(a.L2[n:], etherTypeIPv4)
	a.L2Len = uint8(n + 2)
	return a
}
