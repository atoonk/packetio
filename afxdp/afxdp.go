//go:build linux

// Package afxdp is the packetio backend for Linux AF_XDP.
//
// It is a thin adapter over github.com/atoonk/go-afxdp: the two libraries were
// designed around the same model -- a region of fixed-size frames, descriptors
// that name one frame each, and the four verbs that move a frame between the
// pool, the application and the NIC -- so almost every method here forwards.
//
// Where it differs from the mlx5 backend, and why it matters:
//
//   - Each queue has its own frame region, because each AF_XDP socket maps its
//     own. Capabilities reports SharedRegion false, and a frame received on one
//     queue cannot be transmitted on another without a copy.
//
//   - The kernel is in the path. It owns the descriptors after Transmit and
//     does the work in a soft interrupt, which is real CPU this library cannot
//     see. Measure it with the whole machine, not with this process.
//
//   - Opening is backend-specific: the filter that decides what reaches the
//     sockets is an AF_XDP concept with no equivalent elsewhere, so Open takes
//     options of its own. [WithXDP] carries a go-afxdp option through for
//     anything this package does not name.
//
//   - **A socket is bound per queue, and an unbound queue goes to the kernel.**
//     The XDP program delivers a packet to the socket on the queue the card
//     hashed it to; a queue with no socket is passed up the stack, silently and
//     by design. Opening fewer queues than the card has therefore means the
//     card's hash decides whether anything arrives: one flow lands on exactly
//     one queue, and if that queue is outside the range opened, the receive
//     loop returns nothing and nothing is wrong. Measured on a 48-queue
//     ConnectX: four queues received none of 200 kpps, forty-eight received all
//     of it. Open every queue, or narrow what the card spreads with
//     `ethtool -X`. mlx5 has no equivalent problem -- a steering rule delivers to
//     the queue group whatever the hash decided.
//
//   - **On a tagged interface, the kernel's own VLAN filter comes first.** A
//     driver with `rx-vlan-filter on` -- the default on mlx5e -- drops a tag no
//     VLAN sub-interface has registered, in the card, before the XDP hook runs.
//     The program never sees the packet, the filter matches nothing, and a
//     receive loop reports zero with no error anywhere. Register the id
//     (`ip link add link eth0 name eth0.100 type vlan id 100`) or turn the
//     filter off (`ethtool -K eth0 rx-vlan-filter off`). Direct Verbs does not
//     have this problem: its steering rule carries the id and lives in this
//     process's own flow table, below the netdev filter.
//
//   - Opening attaches an XDP program to the interface, and Close detaches it.
//     A process killed between the two leaves it attached, and the next Open
//     fails with "already attached ... likely owned by another running
//     process". Nothing in userspace can prevent that -- the kernel keeps the
//     program because the link holds a reference, not the process -- so a
//     program that may be killed should say how to clear it:
//
//     sudo ip link set dev eth0 xdp off
//
//     This is worth knowing before it happens: the interface keeps working for
//     the kernel's own traffic, so the only symptom is that this library will
//     not open.
package afxdp

import (
	xdp "github.com/atoonk/go-afxdp"
	"github.com/atoonk/packetio"
)

// Device is a NIC opened for AF_XDP, one socket per queue.
type Device struct {
	fleet    *xdp.Fleet
	detached bool
	tx       []*TxQueue
	rx       []*RxQueue
	closed   bool
}

// An Option configures a device at Open.
//
// It is this package's own type rather than go-afxdp's so that go-afxdp's API
// is not inside packetio's compatibility promise: an option changing shape
// there would otherwise be a breaking change here, for callers who never named
// that package. [WithXDP] is the way through for anything not named here.
type Option func(*config)

type config struct {
	xdp      []xdp.Option
	steerErr error
}

// The options below are the ones every backend in this library spells the same
// way, so that moving a program between backends is the import line and
// nothing else. Each one translates to its go-afxdp equivalent; anything this
// package does not name goes through [WithXDP].
//
// None of them changes a default. go-afxdp picks the frame geometry, binds
// every queue, tunes the driver's interrupt behaviour and places each worker
// beside its queue's interrupt on its own, and a device opened with no options
// at all still gets all of that. These exist for the caller who has a reason
// to override one of those decisions -- not as setup a caller has to perform.

// WithQueues limits how many queues to bind, starting from queue 0. The
// default binds every queue on the interface, which is what keeps
// RSS-distributed traffic from landing on a queue nobody is reading.
//
// AF_XDP binds one socket per queue and that socket both sends and receives,
// so unlike the other backends there is no separate transmit and receive
// count: this sets both.
func WithQueues(n int) Option {
	return func(c *config) { c.xdp = append(c.xdp, xdp.WithQueues(n)) }
}

// WithFrames sets how many frames the region holds, across both directions.
// The default is go-afxdp's, currently 8192.
func WithFrames(n int) Option {
	return func(c *config) { c.xdp = append(c.xdp, xdp.WithNumFrames(n)) }
}

// WithFrameSize sets the size of one frame in bytes. The default is 2048; some
// drivers need 4096 to run zero-copy, and go-afxdp already picks 4096 where it
// knows that to be true.
func WithFrameSize(n int) Option {
	return func(c *config) { c.xdp = append(c.xdp, xdp.WithFrameSize(n)) }
}

// WithAffinity places the workers on the given processors, one per queue in
// order, instead of letting the backend choose.
//
// Placement is automatic without this: go-afxdp puts each worker beside the
// processor its queue's interrupt lands on, which is most of the difference
// between a good AF_XDP number and a poor one. Reach for this only when the
// machine is partitioned and the automatic choice would take cores that belong
// to something else. Passing no processors is [WithoutAffinity].
func WithAffinity(cpus ...int) Option {
	return func(c *config) { c.xdp = append(c.xdp, xdp.WithAffinity(cpus...)) }
}

// WithoutAffinity leaves the workers wherever the scheduler puts them.
//
// For a goroutine that drives a queue and nothing else this costs throughput.
// It is the right choice when something else on the machine owns processor
// placement, or when the worker does enough other work that tying it to one
// core is wrong.
func WithoutAffinity() Option {
	return func(c *config) { c.xdp = append(c.xdp, xdp.WithoutAffinity()) }
}

// WithMultiBuffer binds the sockets for packets that span several frames -- a
// jumbo frame over a 2048-byte frame size -- marked with OptContinued on every
// frame but the last, as [packetio.Capabilities.MultiBuffer] describes. It is
// go-afxdp's WithMultiBuffer under the name every backend here uses.
//
// Receive counts its batch in frames and may end it part-way through a chain,
// the rest arriving next call; [RxQueue.ReceivePackets] returns whole packets.
func WithMultiBuffer() Option {
	return func(c *config) { c.xdp = append(c.xdp, xdp.WithMultiBuffer()) }
}

// WithXDP passes go-afxdp options straight through, for what this package does
// not name: the XDP program, the ring geometry, the wakeup flags.
//
//	afxdp.Open("eth0", afxdp.WithXDP(xdp.WithBusyPoll(50, 64)))
//
// Options are applied in the order given, here and above, so a WithXDP that
// repeats one of the named options above wins if it comes after it.
func WithXDP(opts ...xdp.Option) Option {
	return func(c *config) { c.xdp = append(c.xdp, opts...) }
}

// WithSteering asks for only the packets a filter matches, leaving the rest to
// the kernel. It is the backend-neutral spelling of a receive filter; see
// [SteeringOption] for what AF_XDP can and cannot express.
func WithSteering(f packetio.SteeringFilter) Option {
	return func(c *config) {
		o, err := SteeringOption(f)
		if err != nil {
			c.steerErr = err
			return
		}
		c.xdp = append(c.xdp, o)
	}
}

// Open attaches to an interface and binds one socket per queue.
func Open(iface string, opts ...Option) (*Device, error) {
	// Bind with need-wakeup, before anything the caller asked for so that a
	// WithXDP can still override it.
	//
	// It lets the driver park when there is nothing to do and say so, instead
	// of spinning its NAPI loop forever: go-afxdp measured 25 million polls a
	// second and 65% of a twelve-core box in soft interrupt while forwarding
	// nothing. It also spares transmit a sendto per batch, since the kick is
	// then paid only when the driver is actually asleep.
	//
	// go-afxdp does not default it because a caller that drives the rings
	// directly -- Fill and Receive with no Poll -- must then wake the driver
	// itself. This backend is that caller, and Fill does exactly that; see
	// RxQueue.Fill.
	cfg := config{xdp: []xdp.Option{xdp.WithNeedWakeup()}}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.steerErr != nil {
		return nil, cfg.steerErr
	}
	fleet, err := xdp.Open(iface, cfg.xdp...)
	if err != nil {
		return nil, err
	}
	return NewDevice(fleet), nil
}

// NewDevice wraps a fleet the caller already has, for an application that
// manages the fleet's lifetime itself -- keeping one attached across several
// runs, say. Close then closes that fleet, so do not also close it directly.
func NewDevice(fleet *xdp.Fleet) *Device {
	d := &Device{fleet: fleet}
	for _, s := range fleet.Sockets() {
		r := &region{s: s}
		d.tx = append(d.tx, &TxQueue{
			s: s, region: r,
			// The bounds are captured once rather than read through the socket
			// on every Free, which is on a forwarder's hot path.
			regionLen: uint64(len(s.UMEM())),
			frameSize: uint64(s.FrameSize()),
		})
		d.rx = append(d.rx, &RxQueue{s: s, region: r})
	}
	return d
}

// Detach drops the reference to the fleet without closing it, for an
// application that owns the fleet and is only done with this view of it.
func (d *Device) Detach() { d.detached = true }

// Fleet is the underlying go-afxdp fleet, for the things this API does not
// describe: link state, the attached program, kernel ring counters.
func (d *Device) Fleet() *xdp.Fleet { return d.fleet }

// Capabilities reports what this backend and this NIC can do.
func (d *Device) Capabilities() packetio.Capabilities {
	caps := packetio.Capabilities{
		Backend: "afxdp",
		// The XDP program redirects only what the filter matches; the rest
		// carries on up the kernel's own stack.
		KernelCoexistence: true,
		RSS:               len(d.rx) > 1,
		BlockingPoll:      true,
		SharedRegion:      false, // one region per socket
		// Complete drains the completion ring into a pool without saying
		// which frames came back, so Reclaim cannot name them.
		HandsBackFrames: false,
		MaxQueues:       maxQueues,
	}
	if len(d.tx) > 0 {
		zc, err := d.tx[0].s.ZeroCopy()
		caps.ZeroCopy = err == nil && zc
		// MultiBuffer and MaxFrameSize are both read off the socket, so they
		// describe how it was bound however that was asked for --
		// WithMultiBuffer, WithXDP, or a fleet the caller built. Sockets are
		// single-buffer unless asked: one packet is
		// one frame, never a chain. Bound for multi-buffer, the kernel's
		// continuation bit reaches Receive's descriptors unchanged as
		// OptContinued, which is what MultiBuffer promises. MaxPacket is the
		// frame less the headroom the kernel keeps in front of every packet
		// it writes, not the frame.
		caps.MultiBuffer = d.tx[0].s.MultiBuffer()
		caps.MaxFrameSize = d.tx[0].s.MaxPacket()
	}
	return caps
}

// NumTxQueues and NumRxQueues are how many queues were opened. AF_XDP binds one
// socket per queue and it both sends and receives, so these are equal.
func (d *Device) NumTxQueues() int { return len(d.tx) }

// NumRxQueues is how many receive queues were opened.
func (d *Device) NumRxQueues() int { return len(d.rx) }

// TxQueue returns transmit queue i, or nil if there is no such queue.
func (d *Device) TxQueue(i int) packetio.TxQueue {
	if i < 0 || i >= len(d.tx) {
		return nil
	}
	return d.tx[i]
}

// RxQueue returns receive queue i, or nil if there is no such queue.
func (d *Device) RxQueue(i int) packetio.RxQueue {
	if i < 0 || i >= len(d.rx) {
		return nil
	}
	return d.rx[i]
}

// Tx and Rx return a queue as its concrete type, for what is specific here.
func (d *Device) Tx(i int) *TxQueue {
	if i < 0 || i >= len(d.tx) {
		return nil
	}
	return d.tx[i]
}

// Rx returns the concrete receive queue i, or nil when i is out of range.
func (d *Device) Rx(i int) *RxQueue {
	if i < 0 || i >= len(d.rx) {
		return nil
	}
	return d.rx[i]
}

// Close detaches the program and closes every socket, unless Detach was called
// first, in which case the fleet belongs to somebody else and is left alone.
//
// It is safe to call twice, but not from two goroutines at once.
func (d *Device) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	// Tell the queues, so that a method called after this reports ErrClosed
	// rather than a zero indistinguishable from backpressure.
	for _, q := range d.tx {
		q.dead.Store(true)
	}
	for _, q := range d.rx {
		q.dead.Store(true)
	}
	if d.detached {
		return nil
	}
	return d.fleet.Close()
}

// maxQueues is how many queues of either direction this backend can open: one
// socket per receive queue of the NIC, which is what the fleet already bounds.
// The NIC's own channel count is usually lower, and this backend cannot read
// it, so Capabilities reports this ceiling; binding a queue the device does
// not have fails at Open rather than later.
const maxQueues = 256

// region is one socket's frame memory.
type region struct{ s *xdp.Socket }

func (r *region) Bytes() []byte  { return r.s.UMEM() }
func (r *region) FrameSize() int { return r.s.FrameSize() }
func (r *region) NumFrames() int { return len(r.s.UMEM()) / r.s.FrameSize() }

// Frame is the bytes a descriptor names, or nil when it names anything else.
func (r *region) Frame(d packetio.Desc) []byte {
	b := r.s.UMEM()
	if d.Addr >= uint64(len(b)) || d.Addr+uint64(d.Len) > uint64(len(b)) {
		return nil
	}
	return r.s.GetFrame(xdp.Desc{Addr: d.Addr, Len: d.Len, Options: d.Options})
}

// Writable is everything from the descriptor's address to the end of its frame,
// for building a packet whose length is not yet known. It is nil for a
// descriptor that names nothing in this region.
func (r *region) Writable(d packetio.Desc) []byte {
	b := r.s.UMEM()
	size := uint64(r.s.FrameSize())
	if size == 0 || d.Addr >= uint64(len(b)) {
		return nil
	}
	end := (d.Addr/size + 1) * size
	if end > uint64(len(b)) {
		end = uint64(len(b))
	}
	return b[d.Addr:end:end]
}

func toDescs(in []xdp.Desc, out []packetio.Desc) []packetio.Desc {
	for _, d := range in {
		out = append(out, packetio.Desc{Addr: d.Addr, Len: d.Len, Options: d.Options})
	}
	return out
}

func fromDescs(in []packetio.Desc, out []xdp.Desc) []xdp.Desc {
	for _, d := range in {
		// Only the continuation bit may reach the kernel's tx ring: it
		// rejects descriptors carrying any other option bit, silently, into
		// tx_invalid_descs. Desc promises that Transmit ignores the rest, so
		// a forwarder may hand back received descriptors unmasked.
		out = append(out, xdp.Desc{Addr: d.Addr, Len: d.Len, Options: d.Options & packetio.OptContinued})
	}
	return out
}
