//go:build linux

package netstack

import (
	"errors"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/atoonk/packetio"
)

// goID is the calling goroutine's id, for the one-goroutine-per-queue check.
// Only a test would do this.
func goID() int64 {
	var buf [64]byte
	s := string(buf[:runtime.Stack(buf[:], false)])
	s = strings.TrimPrefix(s, "goroutine ")
	id, _ := strconv.ParseInt(s[:strings.IndexByte(s, ' ')], 10, 64)
	return id
}

// fakeDevice is a packetio.Device in memory: one receive queue fed from a
// channel, one transmit queue that hands every frame to a sink. Two of them
// linked back to back are a wire, so two stacks can talk TCP to each other
// with no root, no interface and no kernel between them. It is what the
// tests of the stack's own behaviour (closing, drops, failures) run on.
type fakeDevice struct {
	rx     []*fakeRx
	tx     []*fakeTx
	closed atomic.Bool
}

const (
	fakeFrameSize = 2048
	fakeFrames    = 64
)

type fakeRegion struct{ buf []byte }

func (r *fakeRegion) Bytes() []byte                { return r.buf }
func (r *fakeRegion) Frame(d packetio.Desc) []byte { return r.buf[d.Addr : d.Addr+uint64(d.Len)] }
func (r *fakeRegion) Writable(d packetio.Desc) []byte {
	return r.buf[d.Addr : (d.Addr/fakeFrameSize+1)*fakeFrameSize]
}
func (r *fakeRegion) FrameSize() int { return fakeFrameSize }
func (r *fakeRegion) NumFrames() int { return fakeFrames }

// fakeFrame is one frame on its way into a receive queue.
type fakeFrame struct {
	data []byte
	opts uint32
}

type fakeRx struct {
	region fakeRegion
	in     chan fakeFrame
	err    atomic.Pointer[error] // what Poll returns once set
	closed atomic.Bool
}

func newFakeRx() *fakeRx {
	return &fakeRx{region: fakeRegion{buf: make([]byte, fakeFrames*fakeFrameSize)}, in: make(chan fakeFrame, 4096)}
}

func (q *fakeRx) inject(data []byte, opts uint32) bool {
	select {
	case q.in <- fakeFrame{data: append([]byte(nil), data...), opts: opts}:
		return true
	default:
		return false
	}
}

func (q *fakeRx) fail(err error) { q.err.Store(&err) }

func (q *fakeRx) Region() packetio.Region { return &q.region }
func (q *fakeRx) Fill(n int) int          { return n }
func (q *fakeRx) Poll(timeout time.Duration) (int, error) {
	if q.closed.Load() {
		return 0, packetio.ErrClosed
	}
	if e := q.err.Load(); e != nil {
		return 0, *e
	}
	if len(q.in) > 0 {
		return len(q.in), nil
	}
	if timeout == 0 {
		return 0, nil
	}
	select {
	case f := <-q.in:
		q.in <- f // put it back; Receive takes it
		return 1, nil
	case <-time.After(timeout):
		return 0, nil
	}
}
func (q *fakeRx) Receive(max int) []packetio.Desc {
	var descs []packetio.Desc
	for i := 0; i < max && i < fakeFrames; i++ {
		select {
		case f := <-q.in:
			off := uint64(i * fakeFrameSize)
			n := copy(q.region.buf[off:off+fakeFrameSize], f.data)
			descs = append(descs, packetio.Desc{Addr: off, Len: uint32(n), Options: f.opts})
		default:
			return descs
		}
	}
	return descs
}
func (q *fakeRx) Err() error {
	if e := q.err.Load(); e != nil {
		return *e
	}
	return nil
}
func (q *fakeRx) Recycle([]packetio.Desc)          {}
func (q *fakeRx) NumFreeFillSlots() int            { return fakeFrames }
func (q *fakeRx) NumReceived() int                 { return len(q.in) }
func (q *fakeRx) NumFreeFrames() int               { return fakeFrames }
func (q *fakeRx) Stats() (packetio.RxStats, error) { return packetio.RxStats{}, nil }
func (q *fakeRx) Close() error                     { q.closed.Store(true); return nil }

type fakeTx struct {
	region fakeRegion
	mu     sync.Mutex
	sink   func([]byte) // where frames go; nil drops them
	sent   atomic.Int64
	slots  atomic.Int64          // room SendFunc reports
	accept atomic.Int64          // frames SendFunc takes of what it is asked, -1 for all
	dead   atomic.Bool           // SendFunc takes nothing and says nothing: a full device queue
	err    atomic.Pointer[error] // what SendFunc returns instead

	// driver is the goroutine inside SendFunc, to catch two at once: a
	// packetio queue must be driven by one goroutine at a time.
	driver atomic.Int64
	races  atomic.Int64
}

func newFakeTx() *fakeTx {
	q := &fakeTx{region: fakeRegion{buf: make([]byte, fakeFrames*fakeFrameSize)}}
	q.slots.Store(fakeFrames)
	q.accept.Store(-1)
	return q
}

func (q *fakeTx) Region() packetio.Region { return &q.region }
func (q *fakeTx) Alloc(int) []packetio.Desc {
	panic("the stack does not Alloc")
}
func (q *fakeTx) Transmit([]packetio.Desc) int                 { panic("the stack does not Transmit") }
func (q *fakeTx) Err() error                                   { return nil }
func (q *fakeTx) Complete(int) int                             { return 0 }
func (q *fakeTx) Reclaim(int, []packetio.Desc) []packetio.Desc { return nil }
func (q *fakeTx) Free([]packetio.Desc)                         {}
func (q *fakeTx) NumCompleted() int                            { return 0 }
func (q *fakeTx) NumInFlight() int                             { return 0 }
func (q *fakeTx) NumFreeSlots() int                            { return int(q.slots.Load()) }
func (q *fakeTx) NumFreeFrames() int                           { return int(q.slots.Load()) }
func (q *fakeTx) SendFunc(count int, build func(int, []byte) int) (int, error) {
	me := goID()
	if prev := q.driver.Swap(me); prev != 0 && prev != me {
		q.races.Add(1)
	}
	defer q.driver.Store(0)
	if e := q.err.Load(); e != nil {
		return 0, *e
	}
	if q.dead.Load() {
		return 0, nil
	}
	if a := q.accept.Load(); a >= 0 && int(a) < count {
		count = int(a) // the device takes what it has room for, mid-packet or not
	}
	for i := 0; i < count; i++ {
		frame := q.region.buf[i%fakeFrames*fakeFrameSize:][:fakeFrameSize]
		n := build(i, frame)
		q.sent.Add(1)
		if q.sink != nil {
			q.sink(frame[:n])
		}
	}
	return count, nil
}
func (q *fakeTx) Stats() (packetio.TxStats, error) { return packetio.TxStats{}, nil }
func (q *fakeTx) Close() error                     { return nil }

func newFakeDevice() *fakeDevice { return newFakeDeviceQueues(1) }

func newFakeDeviceQueues(n int) *fakeDevice {
	d := &fakeDevice{}
	for i := 0; i < n; i++ {
		d.rx = append(d.rx, newFakeRx())
		d.tx = append(d.tx, newFakeTx())
	}
	return d
}

func (d *fakeDevice) Capabilities() packetio.Capabilities {
	return packetio.Capabilities{Backend: "fake", BlockingPoll: true, MaxFrameSize: fakeFrameSize}
}
func (d *fakeDevice) NumTxQueues() int               { return len(d.tx) }
func (d *fakeDevice) NumRxQueues() int               { return len(d.rx) }
func (d *fakeDevice) TxQueue(i int) packetio.TxQueue { return d.tx[i] }
func (d *fakeDevice) RxQueue(i int) packetio.RxQueue { return d.rx[i] }
func (d *fakeDevice) Close() error {
	if d.closed.Swap(true) {
		return errors.New("closed twice")
	}
	for _, q := range d.rx {
		q.Close()
	}
	return nil
}

// link joins two fake devices back to back: what one transmits the other
// receives, on the matching queue index (as a NIC's steering would).
func link(a, b *fakeDevice) {
	for i := range a.tx {
		j := i % len(b.rx)
		a.tx[i].sink = func(f []byte) { b.rx[j].inject(f, 0) }
	}
	for i := range b.tx {
		j := i % len(a.rx)
		b.tx[i].sink = func(f []byte) { a.rx[j].inject(f, 0) }
	}
}
