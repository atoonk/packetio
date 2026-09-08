//go:build linux

package netstack

import (
	"fmt"
	"log"
	"net/netip"
	"sync"
	"sync/atomic"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/header"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/stack"
)

// Stats is a snapshot of the endpoint's counters. Rx and Tx count frames
// delivered to the stack and put on the wire; the rest are drops, each named
// for its reason.
type Stats struct {
	Rx, Tx     uint64
	TxDropped  uint64 // packets the transmit ring had no room for (TCP retransmits them)
	TxWaited   uint64 // WritePackets calls that had to wait for ring room
	GSOPackets uint64 // packets larger than one MSS that the endpoint split
	GSOFrames  uint64 // frames those packets became
	GROFrames  uint64 // frames handed to GRO
	GROPackets uint64 // packets GRO delivered to the stack
	RxBadCsum  uint64 // received IPv4 frames dropped for a bad IPv4 or TCP checksum
	Fragments  uint64 // IPv4 fragments, dropped: TCP never sends them and the stack does not take them
	WrongVLAN  uint64 // frame tagged for another VLAN, or tagged/untagged mismatch
	NotIP      uint64 // EtherType the stack is not given: neither IPv4 nor ARP
	Runt       uint64 // frame too short for an IPv4 header
	OwnEcho    uint64 // frames carrying this endpoint's own source MAC (a tap echoing its transmissions)
	Chained    uint64 // frames that were one piece of a packet spanning several; not accepted
	UnknownMAC uint64 // transmit to a peer never heard from and never resolved: sent to broadcast
	Neighbors  int    // size of the MAC table, seeded and learned

	// DeviceRxDropped and DeviceTxDropped are the device's own counters:
	// what the NIC or the kernel discarded before this stack saw it, and
	// what the device refused to send. They are not this package's drops
	// and are usually the first sign that a link is offering more than the
	// stack is taking.
	DeviceRxDropped, DeviceTxDropped uint64

	// QDiscDropped is packets the stack's transmit queueing discipline
	// dropped for want of room, before the endpoint saw them. It is what
	// fills first when the wire cannot keep up; Config.TxQueueLen is its
	// depth. Zero with Config.DirectTx, which has no such queue.
	QDiscDropped uint64
}

// endpoint is the stack.LinkEndpoint over a packetio.Device: one receive
// goroutine per receive queue moves frames into the stack, and the stack's
// transmit calls build frames straight into the device's own frame memory.
// The stack does everything above Ethernet (ARP, IPv4, TCP); this does
// Ethernet, 802.1Q, and the bookkeeping between the two. New builds one and
// wires it into a Stack, which is the only thing that drives it: its
// exported methods are gVisor's interface, not this package's, and calling
// them from outside would take the stack apart underneath itself.
type endpoint struct {
	dev     packetio.Device
	caps    packetio.Capabilities
	cfg     Config
	mac     [6]byte
	addr    [4]byte      // the stack's address, for what the neighbor table learns from
	prefix  netip.Prefix // the on-link prefix, masked
	linkLen int          // Ethernet header length, 14 or 18 with the VLAN tag
	maxMTU  int          // what the device's frames can carry after the link header
	gsoMax  uint32       // largest packet TCP may hand over, bounded by the transmit pool
	mtu     atomic.Uint32
	neigh   *neighbors
	rxq     []packetio.RxQueue
	txq     []txQueue
	logf    func(string, ...any)

	// learned is told about every (IP, MAC) pair heard from, and about the
	// pairs evicted to make room. The Stack points it at its ARP cache.
	learned func(ip [4]byte, mac [6]byte, evicted [4]byte, didEvict bool)
	// afterBatch runs on each receive goroutine after every batch it
	// delivered, and now and then while idle, with no lock held: it is
	// where the stack does the TCP work it set aside (handshakes, closes)
	// while segments were being processed inline on that goroutine. The
	// endpoint holds no lock when it calls; the stack's own locks are what
	// that work takes, and a delivery in progress would hold them.
	afterBatch func()

	mu         sync.Mutex
	dispatcher stack.NetworkDispatcher
	rxStop     atomic.Bool // the receive goroutines are to return
	txClosed   atomic.Bool // WritePackets refuses; set after the stack is destroyed
	wg         sync.WaitGroup
	onClose    func()

	failOnce sync.Once
	failed   error
	done     chan struct{} // closed on the first queue failure

	rx, tx, txDropped, txWaited, wrongVLAN, notIP, runt, ownEcho, chained, unknownMAC atomic.Uint64
	gsoPackets, gsoFrames, groFrames, groPackets, rxBadCsum, fragments                atomic.Uint64
}

var (
	_ stack.LinkEndpoint = (*endpoint)(nil)
	_ stack.GSOEndpoint  = (*endpoint)(nil)
)

// newEndpoint builds an endpoint over every queue of dev. cfg has had its
// defaults applied. The device stays the caller's: Close stops the
// endpoint's goroutines and nothing else.
func newEndpoint(dev packetio.Device, cfg Config) (*endpoint, error) {
	if dev.NumRxQueues() == 0 || dev.NumTxQueues() == 0 {
		return nil, fmt.Errorf("netstack: the device has %d receive and %d transmit queues; a stack needs both",
			dev.NumRxQueues(), dev.NumTxQueues())
	}
	caps := dev.Capabilities()
	linkLen := linkHeaderLen(cfg.VLAN)
	maxMTU := caps.MaxFrameSize - linkLen
	if cfg.MTU == 0 {
		cfg.MTU = min(defaultMTU, maxMTU)
	}
	if cfg.MTU > maxMTU {
		return nil, fmt.Errorf("netstack: MTU %d plus a %d-byte link header exceeds the %d bytes a %s frame carries",
			cfg.MTU, linkLen, caps.MaxFrameSize, caps.Backend)
	}
	if cfg.MTU < header.IPv4MinimumMTU {
		return nil, fmt.Errorf("netstack: MTU %d is below the IPv4 minimum", cfg.MTU)
	}
	e := &endpoint{
		dev:     dev,
		caps:    caps,
		cfg:     cfg,
		addr:    cfg.Addr.Addr().As4(),
		prefix:  cfg.Addr.Masked(),
		linkLen: linkLen,
		maxMTU:  maxMTU,
		neigh:   newNeighbors(),
		txq:     make([]txQueue, dev.NumTxQueues()),
		logf:    cfg.Logf,
		done:    make(chan struct{}),
	}
	if e.logf == nil {
		e.logf = log.Printf
	}
	copy(e.mac[:], cfg.MAC)
	e.mtu.Store(uint32(cfg.MTU))
	room := 0
	for i := range e.txq {
		e.txq[i].q = dev.TxQueue(i)
		q := e.txq[i].q
		if r := min(q.NumFreeFrames(), q.NumFreeSlots()); i == 0 || r < room {
			room = r
		}
	}
	// A packet is transmitted whole or not at all, so one needing more
	// frames than a queue can hold would never go out: the connection would
	// stall rather than drop. The bound is half the smallest queue, so a
	// packet never needs it all to itself, counted in frames of the
	// smallest segment a peer may ask for -- the MTU is what a frame holds,
	// not what a segment carries, and a peer behind a smaller link cuts the
	// same bytes into far more frames. Never less than one MTU, which is
	// GSO doing nothing.
	e.gsoMax = uint32(max(cfg.MTU, min(gsoMaxSize, room/2*minMSS)))
	for i := 0; i < dev.NumRxQueues(); i++ {
		e.rxq = append(e.rxq, dev.RxQueue(i))
	}
	return e, nil
}

// NumTxQueues is how many transmit queues the endpoint drives: the count to
// give gVisor's fifo queueing discipline, so each is written by one goroutine.
func (e *endpoint) NumTxQueues() int { return len(e.txq) }

// Stats returns a snapshot of the counters, the device's own among them.
func (e *endpoint) Stats() Stats {
	var rxDropped, txDropped uint64
	for _, q := range e.rxq {
		if s, err := q.Stats(); err == nil {
			rxDropped += s.Dropped
		}
	}
	for i := range e.txq {
		if s, err := e.txq[i].q.Stats(); err == nil {
			txDropped += s.Errors
		}
	}
	return Stats{
		DeviceRxDropped: rxDropped, DeviceTxDropped: txDropped,
		Rx: e.rx.Load(), Tx: e.tx.Load(), TxDropped: e.txDropped.Load(), TxWaited: e.txWaited.Load(),
		GSOPackets: e.gsoPackets.Load(), GSOFrames: e.gsoFrames.Load(),
		GROFrames: e.groFrames.Load(), GROPackets: e.groPackets.Load(),
		RxBadCsum: e.rxBadCsum.Load(), Fragments: e.fragments.Load(),
		WrongVLAN: e.wrongVLAN.Load(), NotIP: e.notIP.Load(), Runt: e.runt.Load(),
		OwnEcho: e.ownEcho.Load(), Chained: e.chained.Load(),
		UnknownMAC: e.unknownMAC.Load(), Neighbors: e.neigh.size(),
	}
}

// fail records the first queue failure: it is what Err returns and what
// closes Done. A queue that failed is not driven again; the stack goes on
// running, and the caller decides what to do.
func (e *endpoint) fail(err error) {
	e.failOnce.Do(func() {
		e.failed = err
		e.logf("netstack: %v", err)
		close(e.done)
	})
}

// Err returns the first device failure the endpoint hit, or nil.
func (e *endpoint) Err() error {
	select {
	case <-e.done:
		return e.failed
	default:
		return nil
	}
}

// Done is closed when a queue of the device fails.
func (e *endpoint) Done() <-chan struct{} { return e.done }

// ---- stack.NetworkLinkEndpoint ----

// MTU implements stack.LinkEndpoint.
func (e *endpoint) MTU() uint32 { return e.mtu.Load() }

// SetMTU implements stack.LinkEndpoint. The device's frame size is the
// ceiling: a larger request gets that, not a segment that overruns a frame.
func (e *endpoint) SetMTU(mtu uint32) {
	e.mtu.Store(uint32(min(int(mtu), e.maxMTU)))
}

// MaxHeaderLength implements stack.LinkEndpoint: the Ethernet header, tagged
// or not, that AddHeader will push.
func (e *endpoint) MaxHeaderLength() uint16 { return uint16(e.linkLen) }

// LinkAddress implements stack.LinkEndpoint.
func (e *endpoint) LinkAddress() tcpip.LinkAddress { return tcpip.LinkAddress(e.mac[:]) }

// SetLinkAddress implements stack.LinkEndpoint, and does nothing: the MAC is
// Config.MAC for the life of the stack. Every received frame is classified
// against it on every queue, and changing it under them is not worth a
// synchronised read per frame for a call nothing here makes.
func (e *endpoint) SetLinkAddress(tcpip.LinkAddress) {}

// Capabilities implements stack.LinkEndpoint. Resolution is required: the
// stack runs ARP and asks for a peer's MAC before sending to it. Receive
// checksums are claimed as offloaded because the endpoint verifies every
// IPv4 packet it delivers, on the frame as it arrived, and drops what fails
// (see deliver). With GRO that is not optional -- a merged packet cannot be
// verified afterwards -- and without it the claim saves the stack a second
// pass over every received byte. Transmit checksums are the endpoint's own
// work, in software, and are not claimed.
func (e *endpoint) Capabilities() stack.LinkEndpointCapabilities {
	return stack.CapabilityResolutionRequired | stack.CapabilityRXChecksumOffload
}

// GSOMaxSize implements stack.GSOEndpoint: the largest packet TCP may hand
// WritePackets when GSO is on. A packet is transmitted whole or not at all,
// so one needing more frames than a transmit queue holds could never be
// sent; the smallest queue's pool is the ceiling.
func (e *endpoint) GSOMaxSize() uint32 { return e.gsoMax }

// SupportedGSO implements stack.GSOEndpoint. HostGSOSupported is the flavour
// where the stack sends one big packet and the link splits it.
func (e *endpoint) SupportedGSO() stack.SupportedGSO {
	if e.cfg.NoGSO {
		return stack.GSONotSupported
	}
	return stack.HostGSOSupported
}

// ARPHardwareType implements stack.LinkEndpoint.
func (e *endpoint) ARPHardwareType() header.ARPHardwareType { return header.ARPHardwareEther }

// Attach implements stack.LinkEndpoint. A non-nil dispatcher starts one
// receive goroutine per receive queue; a nil one stops them and waits.
func (e *endpoint) Attach(d stack.NetworkDispatcher) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if d == nil {
		e.stopReceiveLocked()
		return
	}
	if e.dispatcher != nil {
		return
	}
	e.rxStop.Store(false)
	e.dispatcher = d
	for qi, q := range e.rxq {
		e.wg.Add(1)
		go e.rxLoop(qi, q, d)
	}
}

// stopReceive stops the receive goroutines and waits for them. Transmit
// goes on: the stack still has connections to reset and close.
func (e *endpoint) stopReceive() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopReceiveLocked()
}

func (e *endpoint) stopReceiveLocked() {
	if e.dispatcher == nil {
		return
	}
	e.rxStop.Store(true)
	e.wg.Wait()
	e.dispatcher = nil
}

// IsAttached implements stack.LinkEndpoint.
func (e *endpoint) IsAttached() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dispatcher != nil
}

// Wait implements stack.LinkEndpoint: it blocks until the receive goroutines
// have exited.
func (e *endpoint) Wait() { e.wg.Wait() }

// Close implements stack.LinkEndpoint: it stops the receive goroutines and
// then transmit. When Close returns no goroutine is inside a queue of the
// device, which is what the caller needs before closing the device. The
// device itself is not closed; it belongs to whoever opened it.
func (e *endpoint) Close() {
	e.mu.Lock()
	e.stopReceiveLocked()
	f := e.onClose
	e.mu.Unlock()
	e.txClosed.Store(true)
	// A writer that checked txClosed before it was set may still be in
	// SendFunc; taking each queue's lock in turn waits for it.
	for i := range e.txq {
		e.txq[i].mu.Lock()
		e.txq[i].mu.Unlock() //nolint:staticcheck // the empty critical section is the fence
	}
	if f != nil {
		f()
	}
}

// SetOnCloseAction implements stack.LinkEndpoint.
func (e *endpoint) SetOnCloseAction(f func()) {
	e.mu.Lock()
	e.onClose = f
	e.mu.Unlock()
}

// AddHeader implements stack.LinkEndpoint: it prepends the Ethernet (and
// 802.1Q) header. The destination MAC is the one the stack resolved for the
// route; failing that, the neighbor table by IPv4 destination -- the stack
// has none when the kernel answered the ARP on its behalf; failing that,
// broadcast, counted.
func (e *endpoint) AddHeader(pkt *stack.PacketBuffer) {
	dst := broadcastMAC
	if r := pkt.EgressRoute.RemoteLinkAddress; len(r) == 6 {
		copy(dst[:], r)
	} else if nh := pkt.NetworkHeader().Slice(); pkt.NetworkProtocolNumber == header.IPv4ProtocolNumber && len(nh) >= ipv4MinHdrLen {
		if mac, ok := e.neigh.lookup([4]byte(nh[16:20])); ok {
			dst = mac
		} else {
			e.unknownMAC.Add(1)
		}
	} else {
		e.unknownMAC.Add(1)
	}
	hdr := pkt.LinkHeader().Push(e.linkLen)
	putHeader(hdr, dst, e.mac, e.cfg.VLAN, uint16(pkt.NetworkProtocolNumber))
}

// ParseHeader implements stack.LinkEndpoint.
func (e *endpoint) ParseHeader(pkt *stack.PacketBuffer) bool {
	_, ok := pkt.LinkHeader().Consume(e.linkLen)
	return ok
}

// AddNeighbor pins the MAC for ip: learning will not change it and a full
// table will not evict it.
func (e *endpoint) AddNeighbor(ip [4]byte, mac [6]byte) {
	if changed, ev, didEvict := e.neigh.pin(ip, mac); changed && e.learned != nil {
		e.learned(ip, mac, ev, didEvict)
	}
}
