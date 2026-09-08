//go:build linux

package netstack

import (
	"errors"
	"fmt"
	"net/netip"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/buffer"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/header"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/stack"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/stack/gro"
)

const (
	// rxBatch is how many frames one Receive takes. A batch is delivered and
	// recycled before the next, so it bounds how long a frame is held.
	rxBatch = 64
	// pollTimeout bounds a blocking Poll, so a stop is noticed without a
	// packet arriving to end the wait.
	pollTimeout = 100 * time.Millisecond
	// idleBeforeSleep is how long a spinning receive loop stays quiet before
	// it starts sleeping between polls, and idleSleep how long it then
	// sleeps. A queue that is busy never gets there; one that is quiet costs
	// a fraction of a core instead of all of it.
	idleBeforeSleep = 5 * time.Millisecond
	idleSleep       = 50 * time.Microsecond
	// idleCheckEvery is how many empty polls of a spinning backend go by
	// between the loop's housekeeping: draining deferred TCP work, deciding
	// whether to sleep. Such a poll is well under a microsecond, so this is
	// a few microseconds of latency for work that has none of its own. A
	// backend whose Poll blocks does the housekeeping every time round,
	// having already waited.
	idleCheckEvery = 64
	// errCheckEvery is how often a quiet loop asks the queue whether it has
	// failed: a queue that failed looks exactly like a quiet link.
	errCheckEvery = time.Second
)

// rxLoop is one receive queue's goroutine: Fill, Poll, Receive, deliver,
// Recycle. It holds no frame past Recycle, which is what keeps the fill ring
// fed. The queue is driven by this goroutine alone, as packetio requires.
func (e *endpoint) rxLoop(qi int, q packetio.RxQueue, d stack.NetworkDispatcher) {
	defer e.wg.Done()

	var g *gro.GRO
	if !e.cfg.NoGRO {
		g = &gro.GRO{Dispatcher: countingDispatcher{d: d, n: &e.groPackets}}
		g.Init(true)
	}
	blocking := e.caps.BlockingPoll
	region := q.Region()
	quiet := 0                // empty polls in a row
	quietSince := time.Time{} // when the current quiet spell began
	lastErrCheck := time.Now()
	inChain := false // the last frame was one piece of a packet spanning several
	for !e.rxStop.Load() {
		// Every iteration, before waiting: on AF_XDP this is also what wakes
		// a driver that parked while the ring was quiet.
		q.Fill(q.NumFreeFillSlots())

		var n int
		var err error
		if blocking {
			n, err = q.Poll(pollTimeout)
		} else {
			n, err = q.Poll(0)
		}
		if err != nil {
			if !errors.Is(err, packetio.ErrClosed) {
				e.fail(fmt.Errorf("receive queue %d: %w", qi, err))
			}
			return
		}
		if n == 0 {
			if g != nil {
				g.Flush() // idle: nothing more is coming to merge with
			}
			quiet++
			// The housekeeping below is gated on a spinning backend,
			// where an empty poll costs almost nothing and doing it every
			// time would be most of the loop. A blocking Poll already
			// waited pollTimeout, so there the gate would delay the work
			// by that much again, times the gate.
			if !blocking && quiet%idleCheckEvery != 0 {
				if !e.cfg.BusyPoll {
					runtime.Gosched()
				}
				continue
			}
			if e.afterBatch != nil {
				e.afterBatch() // work set aside earlier must not wait for the next packet
			}
			now := time.Now()
			if !blocking && !e.cfg.BusyPoll {
				// Spin through a short lull, so a reply is not delayed
				// by a sleep; sleep only once the queue has been quiet
				// for a while. A sleep this short is tens of microseconds
				// on bare metal and about a millisecond on a virtual
				// machine, which is why it is not the first resort.
				if quietSince.IsZero() {
					quietSince = now
				} else if now.Sub(quietSince) > idleBeforeSleep {
					time.Sleep(idleSleep)
				}
				runtime.Gosched()
			}
			if now.Sub(lastErrCheck) > errCheckEvery {
				lastErrCheck = now
				if err := q.Err(); err != nil {
					e.fail(fmt.Errorf("receive queue %d: %w", qi, err))
					return
				}
			}
			continue
		}
		quiet, quietSince = 0, time.Time{}

		descs := q.Receive(rxBatch)
		for _, dsc := range descs {
			// A packet laid across several frames: every piece but the
			// last is flagged, and the last is whatever bytes were left,
			// not a frame. The stack is never told the device may do that,
			// so this only happens on a device that does it regardless.
			if dsc.Options&packetio.OptContinued != 0 || inChain {
				inChain = dsc.Options&packetio.OptContinued != 0
				e.chained.Add(1)
				continue
			}
			e.deliver(qi, region.Frame(dsc), d, g)
		}
		if g != nil {
			g.Flush()
		}
		q.Recycle(descs)
		if e.afterBatch != nil {
			e.afterBatch()
		}
	}
}

// deliver hands one received frame to the stack. This is where the receive
// copy happens: the frame is copied into a pooled view and is free to be
// recycled the moment deliver returns.
func (e *endpoint) deliver(qi int, f []byte, d stack.NetworkDispatcher, g *gro.GRO) {
	linkLen, et, ok := parseFrame(f, e.cfg.VLAN)
	if !ok {
		e.wrongVLAN.Add(1)
		return
	}
	if [6]byte(f[6:12]) == e.mac {
		// Our own transmission, shown back to us: an AF_PACKET tap does
		// that unless told otherwise, and DPDK's af_packet vdev is not.
		e.ownEcho.Add(1)
		return
	}
	var proto tcpip.NetworkProtocolNumber
	pktLen := len(f) // what the stack gets: the frame, or the packet without its padding
	switch et {
	case etherTypeIPv4:
		if len(f) < linkLen+ipv4MinHdrLen {
			e.runt.Add(1)
			return
		}
		proto = header.IPv4ProtocolNumber
		// Verified here for every frame: with GRO the merged packet is
		// handed over as already checked, and without it the checksums
		// still gate what the neighbor table learns from. A fragment is
		// not accepted at all; TCP never sends one, and a reassembled
		// packet would inherit "verified" from its pieces unverified.
		ip := header.IPv4(f[linkLen:])
		if !checksumsValid(f[linkLen:]) {
			e.rxBadCsum.Add(1)
			return
		}
		if ip.FragmentOffset() != 0 || ip.Flags()&header.IPv4FlagMoreFragments != 0 {
			e.fragments.Add(1)
			return
		}
		// A short packet is padded to the Ethernet minimum on the wire.
		// The stack trims to the IP length itself, but GRO sizes a
		// segment by the bytes it was given and would merge the padding
		// into the stream, so what it gets is the packet alone.
		pktLen = linkLen + int(ip.TotalLength())
		if e.addressedToUs(f, linkLen) {
			e.learn(f, linkLen)
		}
	case etherTypeARP:
		if len(f) < linkLen+header.ARPSize {
			e.runt.Add(1)
			return
		}
		proto = header.ARPProtocolNumber
	default:
		e.notIP.Add(1)
		return
	}
	e.rx.Add(1)

	v := buffer.NewViewSize(pktLen)
	copy(v.AsSlice(), f[:pktLen])
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithView(v)})
	pkt.NetworkProtocolNumber = proto
	// The queue this arrived on. Under fifo it picks the transmit queue for
	// the packets sent in reply, and the fork's TxHashFromRx (fork.go) copies
	// it into the connection, so a connection's replies leave on the queue
	// its segments arrive on.
	pkt.Hash = uint32(qi)
	pkt.LinkHeader().Consume(linkLen)
	switch {
	case [6]byte(f[0:6]) == e.mac:
		pkt.PktType = tcpip.PacketHost
	case [6]byte(f[0:6]) == broadcastMAC:
		pkt.PktType = tcpip.PacketBroadcast
	case isMulticast(f[0:6]):
		pkt.PktType = tcpip.PacketMulticast
	default:
		pkt.PktType = tcpip.PacketOtherHost
	}
	if g != nil && proto == header.IPv4ProtocolNumber {
		// GRO validates a packet's checksums before merging and silently
		// delivers it unmerged on failure -- and upstream's check reads the
		// wrong byte range once a link header has been consumed. The
		// checksums were verified above, on the raw frame; tell it so.
		pkt.RXChecksumValidated = true
		e.groFrames.Add(1)
		g.Enqueue(pkt)
	} else {
		d.DeliverNetworkPacket(proto, pkt)
	}
	pkt.DecRef() // the stack holds its own reference to what it keeps
}

// addressedToUs reports whether an IPv4 frame was sent to this stack, MAC
// and address both, by one host on the stack's own prefix: the only frames
// the neighbor table learns from. Anything else -- another host's traffic
// seen on a tap, a broadcast, a packet from beyond the prefix that the
// gateway forwarded -- says nothing trustworthy about how to reach its
// source.
func (e *endpoint) addressedToUs(f []byte, linkLen int) bool {
	if [6]byte(f[0:6]) != e.mac {
		return false
	}
	// The source must be one host's own address. A frame claiming to come
	// from a group address -- broadcast among them -- would otherwise put
	// that peer's traffic on every port of the segment, which is a spoof
	// that leaves the peer working and so shows no symptom at all.
	if src := f[6:12]; isMulticast(src) || [6]byte(src) == ([6]byte{}) {
		return false
	}
	ip := header.IPv4(f[linkLen:])
	if ip.DestinationAddress().As4() != e.addr {
		return false
	}
	return e.prefix.Contains(netip.AddrFrom4(ip.SourceAddress().As4()))
}

// learn records the source of an IPv4 frame in the neighbor table, and
// tells the stack when that is news.
func (e *endpoint) learn(f []byte, linkLen int) {
	ip, mac := [4]byte(f[linkLen+12:linkLen+16]), [6]byte(f[6:12])
	if changed, ev, didEvict := e.neigh.learn(ip, mac); changed && e.learned != nil {
		e.learned(ip, mac, ev, didEvict)
	}
}

// countingDispatcher sits between GRO and the stack so the merge ratio
// (frames in versus packets out) is observable.
type countingDispatcher struct {
	d stack.NetworkDispatcher
	n *atomic.Uint64
}

func (c countingDispatcher) DeliverNetworkPacket(p tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
	c.n.Add(1)
	c.d.DeliverNetworkPacket(p, pkt)
}

func (c countingDispatcher) DeliverLinkPacket(p tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
	c.d.DeliverLinkPacket(p, pkt)
}
