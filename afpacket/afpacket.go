//go:build linux

// Package afpacket drives a NIC through an ordinary AF_PACKET socket: a
// TPACKET_V3 memory-mapped ring on receive, and batched sendmmsg on transmit.
//
// It is the backend that works everywhere. There is no hardware requirement, no
// driver requirement, no cgo, and nothing to load: if the interface exists and
// the process has CAP_NET_RAW, this works. That is the whole reason it is here.
//
// It is also, by a wide margin, the slowest of the three. Every packet is
// copied by the kernel into the ring and copied again out of it, and transmit
// costs a system call per batch rather than a doorbell write. Expect a couple
// of million packets per second where [github.com/atoonk/packetio/afxdp] does
// tens of millions and [github.com/atoonk/packetio/mlx5] does more still. Use
// it for correctness, for portability, and as the floor to measure the others
// against, not to fill a 100G link.
//
// There is no receive filter on this backend, deliberately. AF_PACKET is a
// tap: the kernel processes every packet whether or not a socket takes a copy,
// so nothing here could keep traffic away from the kernel or steer it toward
// this program. A filter would only decide what is copied into the ring, and
// offering that under the same name the steering backends use invites the
// wrong expectation. The receive queues take everything the interface sees;
// select in your receive loop, or use a backend that steers.
//
// Like every packetio backend, a queue belongs to one goroutine. Nothing here
// is synchronised, and two goroutines sharing a queue will corrupt its pool.
// Different queues of one Device are independent and may run concurrently.
//
// The receive path is a TPACKET_V3 memory-mapped ring; see tpacket.go for how
// it works and why it is a ring rather than recvmmsg.
package afpacket

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"sync"

	"github.com/atoonk/packetio"
	"golang.org/x/sys/unix"
)

// Device is an open AF_PACKET capture of one interface.
//
// Several receive queues means several sockets joined into a PACKET_FANOUT
// group, so the kernel spreads received packets across them by flow hash.
// Transmit queues are independent: any socket can send anything.
type Device struct {
	iface  string
	index  int
	mtu    int
	cfg    config
	region *region
	tx     []*TxQueue
	rx     []*RxQueue
	socks  []int
	// rings is every mapped receive ring, held here rather than only on the
	// RxQueues so that Close unmaps the ones a failed Open created before any
	// queue existed. Closing a packet socket does not unmap its ring, and a
	// live mapping keeps the socket alive and still taking its fanout share.
	rings   []*ring
	closeMu sync.Mutex
	closed  bool
}

// Open captures iface through AF_PACKET.
func Open(iface string, opts ...Option) (d *Device, err error) {
	cfg := defaults()
	for _, o := range opts {
		o(&cfg)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("afpacket: interface %q: %w", iface, err)
	}
	if ifi.Flags&net.FlagUp == 0 {
		// Transmit would fail with ENETDOWN for every batch and receive would
		// block forever, neither saying why.
		return nil, fmt.Errorf("afpacket: interface %s is down; bring it up with "+
			"`sudo ip link set %s up`", iface, iface)
	}

	d = &Device{iface: iface, index: ifi.Index, mtu: ifi.MTU, cfg: cfg}
	// The cleanup below holds its own reference, because returning "nil, err"
	// assigns the named result before the deferred function runs, and a nil
	// receiver would then be the thing we tried to close.
	half := d
	defer func() {
		if err != nil {
			half.Close()
			d = nil
		}
	}()

	if d.region, err = newRegion(cfg.frames, cfg.frameSize); err != nil {
		return nil, err
	}

	// One socket per queue. Receive sockets are fanned out when there is more
	// than one; a transmit socket is never in the group, because a member with
	// no ring is handed its hash share of the ingress and drops every packet
	// of it.
	nq := max(cfg.rxQueues, cfg.txQueues)
	group := fanoutGroup()
	for i := 0; i < nq; i++ {
		wantRing := i < cfg.rxQueues
		fd, r, err := d.openSocket(ifi.Index, wantRing, wantRing && cfg.rxQueues > 1, group)
		if err != nil {
			return nil, err
		}
		d.socks = append(d.socks, fd)
		if r != nil {
			d.rings = append(d.rings, r)
		}
	}

	// Frames are split evenly: transmit queues draw from the front of the
	// region, receive queues from the back, so a descriptor's address alone
	// says which pool owns it.
	perQueue := cfg.frames / (cfg.txQueues + cfg.rxQueues)
	base := 0
	for i := 0; i < cfg.txQueues; i++ {
		d.tx = append(d.tx, newTxQueue(d.region, d.socks[i%len(d.socks)], base, perQueue, cfg.gso))
		base += perQueue
	}
	for i := 0; i < cfg.rxQueues; i++ {
		q, err := newRxQueue(d.region, d.socks[i], d.rings[i], base, perQueue)
		if err != nil {
			return nil, err
		}
		d.rx = append(d.rx, q)
		base += perQueue
	}
	return d, nil
}

// fanoutGroup returns an id for a new fanout group.
//
// The kernel matches an existing group on the id alone, then insists the type,
// protocol and device match too -- so a guessable id means two unrelated
// devices either silently share one group and split each other's traffic, or
// fail to open with EINVAL. Deriving it from the pid and interface index, as
// this once did, collides for two Devices on one interface in one process,
// which is an ordinary thing to want. Random, so it does not.
func fanoutGroup() uint16 { return uint16(rand.Uint32()) }

// openSocket creates one AF_PACKET socket bound to the interface, gives it a
// receive ring if it needs one, and joins it to the fanout group last.
func (d *Device) openSocket(index int, wantRing, fanout bool, group uint16) (int, *ring, error) {
	// A socket with no ring never receives, so it is bound with protocol 0,
	// which registers no receive hook at all. Bound with ETH_P_ALL instead, as
	// this once was, the kernel copies every frame on the wire into a receive
	// buffer nobody drains, for nothing. Sending is unaffected: the socket is
	// still bound to the interface.
	proto := uint16(0)
	if wantRing {
		proto = htons(unix.ETH_P_ALL)
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(proto))
	if err != nil {
		return -1, nil, fmt.Errorf("afpacket: socket: %w (needs root or CAP_NET_RAW)", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{
		Protocol: proto,
		Ifindex:  index,
	}); err != nil {
		unix.Close(fd)
		return -1, nil, fmt.Errorf("afpacket: bind %s: %w", d.iface, err)
	}
	// Do not loop our own transmissions back into receive. Best effort: it
	// needs 4.20 or newer.
	unix.SetsockoptInt(fd, unix.SOL_PACKET, packetIgnoreOutgoing, 1)
	// Skip the qdisc on transmit, handing frames straight to the driver. The
	// tradeoff is that a momentarily full device queue drops the frame rather
	// than queueing it, which for a packet-I/O library is the right side of the
	// trade: we would rather see the drop than hide latency in a queue.
	unix.SetsockoptInt(fd, unix.SOL_PACKET, packetQdiscBypass, 1)
	unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, d.cfg.socketBuffer)
	unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, d.cfg.socketBuffer)
	// PACKET_AUXDATA is not what carries the VLAN tag here -- TPACKET_V3 puts
	// it in the frame header -- so this failing costs nothing.
	unix.SetsockoptInt(fd, unix.SOL_PACKET, packetAuxdata, 1)
	if d.cfg.promiscuous && wantRing {
		if err := unix.SetsockoptPacketMreq(fd, unix.SOL_PACKET, unix.PACKET_ADD_MEMBERSHIP,
			&unix.PacketMreq{Ifindex: int32(index), Type: unix.PACKET_MR_PROMISC}); err != nil {
			unix.Close(fd)
			return -1, nil, fmt.Errorf("afpacket: promiscuous mode on %s: %w", d.iface, err)
		}
	}
	if d.cfg.gso {
		// Both directions need this, and it has to be set before any ring is
		// configured: a transmit socket without it takes the virtio header for
		// packet data and puts ten junk bytes on the wire.
		if err := unix.SetsockoptInt(fd, unix.SOL_PACKET, packetVnetHdr, 1); err != nil {
			unix.Close(fd)
			return -1, nil, fmt.Errorf("afpacket: PACKET_VNET_HDR on %s: %w (the kernel may not "+
				"support offload passthrough here; open without WithGSO)", d.iface, err)
		}
	}
	var r *ring
	if wantRing {
		// The ring has to exist before the socket joins the fanout group. A
		// socket that joins first is handed its share of the traffic straight
		// away, and with nowhere to put it the kernel drops every packet and
		// never releases a block -- the reader then waits forever on a socket
		// whose drop counter is climbing.
		var err error
		if r, err = setupRing(fd, d.cfg.gso); err != nil {
			unix.Close(fd)
			return -1, nil, fmt.Errorf("afpacket: receive ring on %s: %w", d.iface, err)
		}
	}
	if fanout {
		// PACKET_FANOUT_HASH spreads by flow hash, so a flow stays on one
		// queue. Rollover so a queue whose reader is briefly behind does not
		// drop what another queue could have taken.
		//
		// Deliberately not PACKET_FANOUT_FLAG_DEFRAG: it runs IP
		// defragmentation on every packet in softirq context, measured here at
		// about thirty cores for two million packets a second. Fragments of one
		// datagram then hash apart, which is the trade.
		//
		// The kernel splits this one integer as fanout_add(sk, val & 0xffff,
		// val >> 16): the group id is the LOW half and the type and flags the
		// high half, which is the opposite way round from how it reads.
		arg := int(group) | (fanoutHash|fanoutFlagRollover)<<16
		if err := unix.SetsockoptInt(fd, unix.SOL_PACKET, packetFanout, arg); err != nil {
			if r != nil {
				r.close()
			}
			unix.Close(fd)
			return -1, nil, fmt.Errorf("afpacket: PACKET_FANOUT on %s: %w", d.iface, err)
		}
	}
	return fd, r, nil
}

// Capabilities describes what this backend can do, which is not much: the
// kernel copies every packet in both directions, and the only way to spread
// receive over several queues is the fanout group's flow hash.
func (d *Device) Capabilities() packetio.Capabilities {
	return packetio.Capabilities{
		Backend:  "afpacket",
		ZeroCopy: false,
		// A packet socket only ever takes copies; the kernel's own path is
		// untouched by definition.
		KernelCoexistence: true,
		MultiBuffer:       false,
		RSS:               len(d.rx) > 1,
		BlockingPoll:      true,
		SharedRegion:      true,
		HandsBackFrames:   true,
		Offload:           d.cfg.gso,
		MaxFrameSize:      d.cfg.frameSize,
		MaxQueues:         maxQueues,
	}
}

// Region is the frame memory every queue of this device shares.
func (d *Device) Region() packetio.Region { return d.region }

// NumTxQueues is how many transmit queues were opened.
func (d *Device) NumTxQueues() int { return len(d.tx) }

// NumRxQueues is how many receive queues were opened.
func (d *Device) NumRxQueues() int { return len(d.rx) }

// TxQueue and RxQueue return queue i, or nil when i is out of range.
func (d *Device) TxQueue(i int) packetio.TxQueue {
	if i < 0 || i >= len(d.tx) {
		return nil
	}
	return d.tx[i]
}

// RxQueue returns receive queue i, or nil when i is out of range.
func (d *Device) RxQueue(i int) packetio.RxQueue {
	if i < 0 || i >= len(d.rx) {
		return nil
	}
	return d.rx[i]
}

// Tx and Rx return the concrete queues, for the few things specific to this
// backend: the kernel drop counters and the last transmit errno.
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

// Interface returns the captured interface's name, and MTU its MTU as it was
// when the device was opened.
func (d *Device) Interface() string { return d.iface }

// MTU is the interface's MTU as it was when the device was opened.
func (d *Device) MTU() int { return d.mtu }

// Close releases the queues, the rings, the sockets and the frame memory.
//
// It is safe to call on a half-built device, safe to call twice, and safe to
// call from another goroutine than the one using the queues: closing a receive
// queue wakes a goroutine blocked in its Poll, which then returns
// [packetio.ErrClosed]. It is still not safe to call while a goroutine is
// part-way through Receive or Transmit, which read memory this unmaps.
func (d *Device) Close() error {
	if d == nil {
		return nil
	}
	d.closeMu.Lock()
	defer d.closeMu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true

	var errs []error
	// Queues first: an RxQueue's Close wakes anything blocked in its Poll and
	// unmaps its ring, and both have to happen before the socket goes.
	for _, q := range d.tx {
		if err := q.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	for _, q := range d.rx {
		if err := q.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	// Any ring no queue adopted, because Open failed part-way. Closing a
	// packet socket does not unmap its ring.
	for _, r := range d.rings {
		if err := r.close(); err != nil {
			errs = append(errs, err)
		}
	}
	for _, fd := range d.socks {
		if err := unix.Close(fd); err != nil {
			errs = append(errs, fmt.Errorf("afpacket: closing socket: %w", err))
		}
	}
	if d.region != nil {
		if err := d.region.close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// htons converts to network byte order. binary.NativeEndian says once what a
// GOARCH switch would have to keep saying for every new port.
func htons(v uint16) uint16 {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return binary.NativeEndian.Uint16(b[:])
}

var _ packetio.Device = (*Device)(nil)
