//go:build linux

package netstack

import (
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/stack"
)

// txQueue is one transmit queue's state: the queue, the lock that keeps it
// to one goroutine at a time, and the builder reused across batches. Under
// gVisor's fifo queueing discipline exactly one goroutine writes each queue
// and the lock is never contended; with Config.DirectTx every writer takes
// it, one packet at a time. Either way a queue is driven by one goroutine
// at a time, which is what packetio requires.
type txQueue struct {
	mu      sync.Mutex
	q       packetio.TxQueue
	segs    []int // scratch: frames per packet in the batch
	builder copier
}

// ringWait bounds how long WritePackets waits for room on a full transmit
// ring before giving the rest of the batch up. Completions arrive on the
// device's own schedule; a couple of milliseconds covers a full ring draining
// at any rate a queue this size is worth having.
const ringWait = 2 * time.Millisecond

// WritePackets implements stack.LinkWriter. Each packet is built directly
// into the device's frames by SendFunc, whole packets only: a GSO packet
// becomes several frames, and half a packet on the wire is worse than none.
// The queue is chosen by pkt.Hash modulo the queue count, which under fifo
// is the same for every packet of a batch. What the ring cannot take within
// ringWait is dropped and counted, once per packet; TCP retransmits. With
// Config.DirectTx there is no wait at all (see below).
func (e *endpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	list := pkts.AsSlice()
	if len(list) == 0 {
		return 0, nil
	}
	// The queue index, from the hash the receive path put on the packet.
	// gVisor's fifo discipline picks its goroutine by the same hash and the
	// same modulus (see stack.go, where it is given len(e.txq) queues), so
	// the goroutine writing a queue is always the one for that queue, which
	// is the rule packetio makes. Under DirectTx there is no discipline and
	// gVisor hands over one packet at a time, so the mutex below is what
	// keeps the rule instead.
	q := &e.txq[int(list[0].Hash)%len(e.txq)]
	q.mu.Lock()
	defer q.mu.Unlock()
	if e.txClosed.Load() {
		return 0, &tcpip.ErrClosedForSend{}
	}

	sent := 0
	deadline := time.Time{}
	for sent < len(list) {
		n, err := e.transmit(q, list[sent:])
		sent += n
		if err != nil {
			e.fail(err)
			e.txDropped.Add(uint64(len(list) - sent))
			return sent, &tcpip.ErrClosedForSend{}
		}
		if n > 0 {
			continue
		}
		// No room for even the next packet. Completions are what make
		// room, and they come with time, so wait a little for them --
		// but not with Config.DirectTx, where the caller is whatever
		// goroutine produced the packet, often the receive goroutine
		// itself under inline processing: stalling that to wait on the
		// transmit ring stops the intake that drives the completions.
		if e.cfg.DirectTx {
			break
		}
		if deadline.IsZero() {
			deadline = time.Now().Add(ringWait)
			e.txWaited.Add(1)
		} else if time.Now().After(deadline) {
			break
		}
		runtime.Gosched()
	}
	e.txDropped.Add(uint64(len(list) - sent))
	return sent, nil
}

// transmit builds as many leading whole packets of pkts into frames as the
// ring and the pool allow, and queues them. It returns the packets sent, and
// an error when the queue has failed. Called with q.mu held.
func (e *endpoint) transmit(q *txQueue, pkts []*stack.PacketBuffer) (int, error) {
	if err := q.q.Err(); err != nil {
		return 0, err
	}
	// Take completions back first, so the budget is exact.
	q.q.Complete(q.q.NumCompleted())
	budget := min(q.q.NumFreeSlots(), q.q.NumFreeFrames())
	if budget <= 0 {
		return 0, nil
	}
	segs := q.segs[:0]
	for _, pkt := range pkts {
		segs = append(segs, segments(pkt))
		if len(segs) >= budget { // no later packet can fit anyway
			break
		}
	}
	q.segs = segs
	admitted, frames := admit(segs, budget)
	if admitted == 0 {
		return 0, nil
	}

	// SendFunc calls the builder once per frame, i counting up from zero,
	// so a running cursor over (packet, segment) is all the index that is
	// needed -- and build checks i against it rather than trusting it. The
	// builder is a method on a struct the queue keeps: a closure here would
	// carry the segmenter to the heap on every call.
	c := &q.builder
	c.e, c.pkts, c.segs = e, pkts[:admitted], segs[:admitted] // segs is a window onto q.segs
	c.p, c.gso, c.frame = 0, false, 0
	if c.fn == nil {
		c.fn = c.build
	}
	n, err := q.q.SendFunc(frames, c.fn)
	c.pkts, c.segs = nil, nil // no reference kept past the call
	e.tx.Add(uint64(n))
	if err != nil {
		return 0, fmt.Errorf("transmit queue: %w", err)
	}
	// n may be zero although the budget said there was room: a full device
	// queue is backpressure, not a failure (AF_PACKET reports EAGAIN and
	// ENOBUFS that way). Either way the caller waits and then drops.
	//
	// n may also stop inside a packet, since a device takes the frames it
	// has room for and the count it promised is not a boundary. Such a
	// packet went out in part, so it counts as sent: its frames are on the
	// wire and cannot be recalled, and rebuilding it would put the same
	// ones there twice while the batch made no progress at all. TCP
	// retransmits what is missing.
	sent, frames := admit(segs[:admitted], n)
	if frames < n {
		sent++
	}
	return sent, nil
}

// copier is transmit's builder state, one per queue, reused.
type copier struct {
	e     *endpoint
	pkts  []*stack.PacketBuffer
	segs  []int
	p     int // packet being emitted
	frame int // frame being built, to check the order they are asked for in
	s     segmenter
	gso   bool // s is initialised for packet p
	fn    func(int, []byte) int
}

// build fills one frame: the next whole packet, or the next segment of the
// packet being split. frame is the packet area of one of the device's
// frames, headroom already skipped, so writing starts at offset 0.
func (c *copier) build(i int, frame []byte) int {
	if i != c.frame {
		// The frames are built in order, which is what the cursor below
		// assumes. Refusing rather than emitting the wrong bytes: a zero
		// length is a frame the device does not send.
		c.e.logf("netstack: transmit queue asked for frame %d, expected %d", i, c.frame)
		return 0
	}
	c.frame++
	e := c.e
	pkt := c.pkts[c.p]
	var length int
	if c.segs[c.p] == 1 {
		length = copyPacket(frame, pkt)
		if needsChecksum(pkt) {
			finishTCPChecksum(frame, e.linkLen)
		}
		c.p++
	} else {
		if !c.gso {
			c.s.init(pkt, e.linkLen)
			c.gso = true
			e.gsoPackets.Add(1)
			e.gsoFrames.Add(uint64(c.segs[c.p]))
		}
		length = c.s.next(frame)
		if c.s.done() {
			c.gso = false
			c.p++
		}
	}
	return length
}

// copyPacket writes pkt, headers and all, into frame and returns the frame
// length. A packet's views begin with reserved header space that was not
// used, which is what the offset from AsViewList skips. Short frames are
// padded to the Ethernet minimum so the driver never sees a runt.
func copyPacket(frame []byte, pkt *stack.PacketBuffer) int {
	views, off := pkt.AsViewList()
	n := 0
	for v := views.Front(); v != nil; v = v.Next() {
		s := v.AsSlice()
		if off >= len(s) {
			off -= len(s)
			continue
		}
		s = s[off:]
		off = 0
		n += copy(frame[n:], s)
	}
	if n < minFrameLen {
		clear(frame[n:minFrameLen])
		n = minFrameLen
	}
	return n
}
