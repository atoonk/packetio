package queue

import (
	"testing"

	"github.com/atoonk/packetio/dpdk/internal/mbuf"
)

// fakePMD is a poll-mode driver that behaves like a real one, including the
// parts that make DPDK awkward to put under this API. Every behaviour here was
// observed on a ConnectX-6 Dx with DPDK 23.11 during the M0 spike:
//
//   - a transmit burst may take a prefix and no more;
//   - finished buffers are not handed back when the hardware finishes with
//     them, but when the driver next looks, and it only looks inside a burst or
//     when poked;
//   - it does not look at all until enough packets have gone since the last
//     time (MLX5_TX_COMP_THRESH is 32), so a small trailing batch stays with
//     the driver however much it is poked;
//   - receive buffers are taken from the supply in all-or-nothing bulks;
//   - a freed buffer goes to the pool the mbuf names, not to the queue that
//     sent it.
//
// A fake that got any of those wrong would let a backend pass here and starve
// on hardware.
type fakePMD struct {
	region   []byte
	regionVA uint64
	l        mbuf.Layout

	// returned is where a freed mbuf goes, chosen by the pool the mbuf names.
	returned map[uint64]*Ring
	supply   *Ring

	// transmit
	accept     int // most descriptors one burst will take; 0 means all
	compThresh int // packets that must accumulate before completions are read
	holding    []uint64
	sent       int

	// receive
	rxOlFlags uint64
	// chained makes every delivered packet claim more than one mbuf.
	chained bool     // what the NIC reports about each received packet
	pending [][]byte // packets waiting to be delivered
	bulk    int      // buffers taken from the supply at a time
	held    []uint64 // taken from the supply, not yet filled
	noBuf   int      // bulks the supply could not satisfy
}

func newFakePMD(region []byte, regionVA uint64, l mbuf.Layout) *fakePMD {
	return &fakePMD{
		region: region, regionVA: regionVA, l: l,
		returned:   map[uint64]*Ring{},
		compThresh: 32,
		bulk:       4,
	}
}

func (f *fakePMD) TxBurst(mbufs []uint64) int {
	f.completions() // a real driver reads completions at the top of a burst
	n := len(mbufs)
	if f.accept > 0 && n > f.accept {
		n = f.accept
	}
	f.holding = append(f.holding, mbufs[:n]...)
	f.sent += n
	return n
}

func (f *fakePMD) Poke() { f.completions() }

// completions hands back everything the driver holds, but only once enough has
// accumulated. Below the threshold it does nothing at all, which is the trap.
func (f *fakePMD) completions() {
	if len(f.holding) < f.compThresh {
		return
	}
	for _, va := range f.holding {
		f.free(va)
	}
	f.holding = f.holding[:0]
}

// free returns one mbuf to the pool it names, exactly as rte_mempool_put does.
func (f *fakePMD) free(va uint64) {
	m := int(va - f.regionVA)
	p := mbuf.Pool(f.region, m)
	r := f.returned[p]
	if r == nil {
		panic("the driver freed an mbuf into a pool nothing registered")
	}
	if !r.PushOne(va) {
		panic("a returned ring overflowed: the backend sized it too small and lost a frame")
	}
}

// take gets one buffer from the supply, in bulks, all or nothing.
func (f *fakePMD) take() (uint64, bool) {
	if len(f.held) == 0 {
		buf := make([]uint64, f.bulk)
		if !f.supply.PopBulk(buf, f.bulk) {
			f.noBuf++
			return 0, false
		}
		f.held = append(f.held, buf...)
	}
	va := f.held[0]
	f.held = f.held[1:]
	return va, true
}

func (f *fakePMD) RxBurst(out []uint64) int {
	n := 0
	for n < len(out) && len(f.pending) > 0 {
		va, ok := f.take()
		if !ok {
			break
		}
		pkt := f.pending[0]
		f.pending = f.pending[1:]

		m := int(va - f.regionVA)
		frame := m - f.l.ObjHeader
		at := frame + f.l.BufferOffset(0) + f.l.Headroom
		copy(f.region[at:], pkt)
		mbuf.PrepareTx(f.region, m, mbuf.Pool(f.region, m), uint16(f.l.Headroom), uint32(len(pkt)))
		mbuf.SetOlFlags(f.region, m, f.rxOlFlags)
		if f.chained {
			// A packet spanning several mbufs, which this backend refuses.
			// Only nb_segs is needed to say so; the rest of the chain is not
			// walked, because the refusal happens before anything reads it.
			mbuf.SetNbSegs(f.region, m, 2)
		}
		out[n] = va
		n++
	}
	return n
}

// deliver queues packets for the next receive bursts.
func (f *fakePMD) deliver(pkts ...[]byte) { f.pending = append(f.pending, pkts...) }

// rig is a region, its layout and the rings, built the way the real backend
// builds them: one region, each queue owning a disjoint run of frames.
type rig struct {
	region   []byte
	regionVA uint64
	l        mbuf.Layout
	pmd      *fakePMD
}

const testRegionVA = 0x7f0000000000

func newRig(t *testing.T, frames int) *rig {
	t.Helper()
	l := mbuf.Layout{FrameSize: 2048, ObjHeader: 64, MbufSize: mbuf.Size, Headroom: 128}
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
	r := &rig{
		region:   make([]byte, frames*l.FrameSize),
		regionVA: testRegionVA,
		l:        l,
	}
	r.pmd = newFakePMD(r.region, r.regionVA, l)
	// The mempool's initialiser writes each mbuf's buffer once at open; the
	// backend relies on it having happened.
	for i := 0; i < frames; i++ {
		frame := i * l.FrameSize
		m := l.MbufOffset(frame)
		mbuf.SetBuffer(r.region, m,
			r.regionVA+uint64(l.BufferOffset(frame)),
			r.regionVA+uint64(l.BufferOffset(frame)),
			uint16(l.BufferLen()))
	}
	return r
}

// tx builds a transmit queue owning frames [first, first+n).
func (r *rig) tx(t *testing.T, poolVA uint64, first, n, depth int) *Tx {
	t.Helper()
	return r.txOffload(t, poolVA, first, n, depth, false, false)
}

// txOffload is tx with the device's offloads declared, for the cases that turn
// on what the device took rather than what was asked for.
func (r *rig) txOffload(t *testing.T, poolVA uint64, first, n, depth int, csum, tso bool) *Tx {
	t.Helper()
	ret, err := NewRing(1024)
	if err != nil {
		t.Fatal(err)
	}
	r.pmd.returned[poolVA] = ret
	q, err := NewTx(Config{
		Region: r.region, RegionVA: r.regionVA, Layout: r.l, PoolVA: poolVA,
		FirstFrame: first, Frames: n, Depth: depth,
		Returned: ret, PMD: r.pmd,
		Checksums: csum, TSO: tso,
	})
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// rx builds a receive queue owning frames [first, first+n).
func (r *rig) rx(t *testing.T, poolVA uint64, first, n, depth int) *Rx {
	return r.rxSupply(t, poolVA, first, n, depth, 1024)
}

// rxSupply is rx with a supply ring of a chosen size, for the cases where the
// supply is the thing that runs short.
func (r *rig) rxSupply(t *testing.T, poolVA uint64, first, n, depth, supply int) *Rx {
	t.Helper()
	sup, err := NewRing(supply)
	if err != nil {
		t.Fatal(err)
	}
	ret, err := NewRing(1024)
	if err != nil {
		t.Fatal(err)
	}
	r.pmd.returned[poolVA] = ret
	r.pmd.supply = sup
	q, err := NewRx(Config{
		Region: r.region, RegionVA: r.regionVA, Layout: r.l, PoolVA: poolVA,
		FirstFrame: first, Frames: n, Depth: depth,
		Supply: sup, Returned: ret, PMD: r.pmd,
	})
	if err != nil {
		t.Fatal(err)
	}
	return q
}
