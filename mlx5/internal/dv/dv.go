//go:build linux && cgo && mlx5 && (amd64 || arm64)

// Package dv opens an mlx5 device and creates packet queues on it.
//
// This is the only part of packetio that uses cgo, and it runs only when a
// device is opened or closed. It asks libibverbs and libmlx5 to build a raw
// Ethernet queue pair, then asks mlx5dv_init_obj where the memory they
// allocated for it lives, and hands those addresses back as ordinary Go slices
// and pointers. Everything after that is Go, and the C library is not called
// again until the device is closed.
package dv

/*
#cgo LDFLAGS: -libverbs -lmlx5
#cgo CFLAGS: -Wall

#include <stdlib.h>
#include "shim.h"
*/
import "C"

import (
	"fmt"
	"math/bits"
	"unsafe"
)

// errBuf is where the C side writes a message. It is a fixed size because the
// alternative is allocating in a failure path that has just run out of
// something.
const errBufSize = 256

func cErr(buf *C.char, format string, args ...any) error {
	msg := C.GoString(buf)
	if msg == "" {
		msg = "unknown error"
	}
	return fmt.Errorf("%s: %s", fmt.Sprintf(format, args...), msg)
}

// Caps describes what a device can do.
type Caps struct {
	// IBDev is the verbs device name, such as mlx5_0, and FW its firmware
	// version.
	IBDev string
	FW    string

	// Port is the port the interface belongs to, and PortActive whether it is
	// up.
	Port       uint32
	PortActive bool

	// MaxQPWR is the longest queue the device will create, MaxSGE the number
	// of data segments one work request may have, and MaxCQE the longest
	// completion queue.
	MaxQPWR uint32
	MaxSGE  uint32
	MaxCQE  uint32

	// DVFlags is what mlx5dv_query_device reported. The interesting bits are
	// EnhancedMPW, which says the device can carry several packets in one work
	// queue entry, and CQE128BComp.
	DVFlags uint64

	VendorID     uint32
	VendorPartID uint32
}

// Bits of Caps.DVFlags worth naming.
const (
	CapCQEV1       = 1 << 0 // version 1 completion entries, which this code assumes
	CapMPWAllowed  = 1 << 2 // the older multi-packet write
	CapEnhancedMPW = 1 << 3 // several packets per work queue entry
	CapCQE128BComp = 1 << 4
	CapCQE128BPad  = 1 << 5
)

// EnhancedMPW reports whether the device can carry several packets in one work
// queue entry. Nothing here uses it yet; it is the next lever on transmit rate
// after batching, and knowing early whether a card has it is worth the line.
func (c Caps) EnhancedMPW() bool { return c.DVFlags&CapEnhancedMPW != 0 }

// Device is an open mlx5 device with a protection domain and, once a region is
// registered, memory the NIC may use.
type Device struct {
	ctx  *C.pio_ctx
	caps Caps
	lkey uint32
	reg  []byte
}

// Open opens the verbs device named ibdev and prepares it for packet queues.
func Open(ibdev string, port uint32) (*Device, error) {
	cname := C.CString(ibdev)
	defer C.free(unsafe.Pointer(cname))

	var (
		ctx    *C.pio_ctx
		caps   C.struct_pio_caps
		errbuf [errBufSize]C.char
	)
	if C.pio_open(cname, C.uint32_t(port), &ctx, &caps, &errbuf[0], errBufSize) != 0 {
		return nil, cErr(&errbuf[0], "opening %s port %d", ibdev, port)
	}

	d := &Device{
		ctx: ctx,
		caps: Caps{
			IBDev:        C.GoString(&caps.ibdev[0]),
			FW:           C.GoString(&caps.fw[0]),
			Port:         uint32(caps.port),
			PortActive:   caps.port_state == 4, // IBV_PORT_ACTIVE
			MaxQPWR:      uint32(caps.max_qp_wr),
			MaxSGE:       uint32(caps.max_sge),
			MaxCQE:       uint32(caps.max_cqe),
			DVFlags:      uint64(caps.dv_flags),
			VendorID:     uint32(caps.vendor_id),
			VendorPartID: uint32(caps.vendor_part_id),
		},
	}
	return d, nil
}

// Caps describes the device.
func (d *Device) Caps() Caps { return d.caps }

// Close releases the device, its protection domain and its registered region.
// No queue may still exist: the NIC may be reading memory a queue points at.
func (d *Device) Close() error {
	if d.ctx == nil {
		return nil
	}
	C.pio_dv_close(d.ctx)
	d.ctx = nil
	d.reg = nil
	return nil
}

// RegisterRegion pins b and registers it with the device, returning the memory
// key that every work queue entry pointing into it must carry.
//
// b must not be Go heap memory: the NIC will read and write it by direct memory
// access for as long as the device is open, and the garbage collector is
// entitled to move or reuse anything on the heap. It comes from mmap.
func (d *Device) RegisterRegion(b []byte) (lkey uint32, err error) {
	if d.ctx == nil {
		return 0, fmt.Errorf("mlx5: device is closed")
	}
	if len(b) == 0 {
		return 0, fmt.Errorf("mlx5: empty region")
	}

	var (
		key    C.uint32_t
		errbuf [errBufSize]C.char
	)
	if C.pio_reg_mr(d.ctx, unsafe.Pointer(&b[0]), C.size_t(len(b)), &key, &errbuf[0], errBufSize) != 0 {
		return 0, cErr(&errbuf[0], "registering %d bytes", len(b))
	}
	d.lkey = uint32(key)
	d.reg = b
	return d.lkey, nil
}

// LKey is the memory key of the registered region.
func (d *Device) LKey() uint32 { return d.lkey }

// MinInline reports how many bytes of each packet's Ethernet header this port
// insists on having inside the work queue entry rather than pointed at: zero,
// or eighteen, which is a header plus one VLAN tag.
//
// The device is not asked directly, because there is no way to. It is measured:
// the mlx5 provider is allowed to lay out one send, the inline length it chose
// is read back out of the entry, and the send is abandoned before anything is
// transmitted.
func (d *Device) MinInline() (int, error) {
	if d.reg == nil {
		return 0, fmt.Errorf("mlx5: register a region before probing the inline length")
	}

	var (
		n      C.uint32_t
		errbuf [errBufSize]C.char
	)
	if C.pio_min_inline(d.ctx, unsafe.Pointer(&d.reg[0]), &n, &errbuf[0], errBufSize) != 0 {
		return 0, cErr(&errbuf[0], "probing the required inline length")
	}
	return int(n), nil
}

// TxQueue is the memory of one transmit queue, as the packet path sees it.
type TxQueue struct {
	// SQ is the work queue entry buffer and CQ the completion queue buffer.
	// Both alias memory libmlx5 allocated and the NIC reads or writes; they
	// stay valid until Close.
	SQ []byte
	CQ []byte

	// SQDbrec and CQDbrec are the doorbell records: one word of host memory
	// each, which the NIC reads to learn how far software has got.
	SQDbrec *uint32
	CQDbrec *uint32

	// UAR is the device register a doorbell is written to.
	UAR unsafe.Pointer

	// BFSize is the size of the BlueFlame buffer at UAR, or zero if the
	// register is not one. Nothing here uses BlueFlame; the register is used
	// as a plain doorbell either way.
	BFSize uint32

	// CQESize is the size of a completion entry, and QPN and CQN name the
	// queue pair and completion queue to the hardware.
	CQESize uint32
	QPN     uint32
	CQN     uint32

	q C.struct_pio_txq
}

// CreateTxQueue creates a raw Ethernet transmit queue with room for at least
// depth packets, drives it to the ready-to-send state and exposes its memory.
//
// The depth the hardware gives may be larger than the one asked for, since
// queues are rounded up to a power of two. Everything here reports what was
// actually created.
func (d *Device) CreateTxQueue(depth int) (*TxQueue, error) {
	if d.ctx == nil {
		return nil, fmt.Errorf("mlx5: device is closed")
	}
	if depth <= 0 {
		return nil, fmt.Errorf("mlx5: transmit queue depth %d", depth)
	}
	if uint32(depth) > d.caps.MaxQPWR {
		return nil, fmt.Errorf("mlx5: transmit queue depth %d, but %s allows at most %d", depth, d.caps.IBDev, d.caps.MaxQPWR)
	}

	t := &TxQueue{}
	var errbuf [errBufSize]C.char
	if C.pio_create_txq(d.ctx, C.uint32_t(depth), &t.q, &errbuf[0], errBufSize) != 0 {
		return nil, cErr(&errbuf[0], "creating a transmit queue of %d", depth)
	}

	if err := t.adopt(); err != nil {
		var e [errBufSize]C.char
		C.pio_destroy_txq(&t.q, &e[0], errBufSize)
		return nil, err
	}
	return t, nil
}

// adopt checks that the queue the provider built is one this code can drive,
// and turns its addresses into Go slices.
//
// The checks are not defensive programming for its own sake. Every one of them
// is an assumption the packet path makes and cannot check again per packet: a
// different block size, a larger completion entry or a queue whose length is
// not a power of two would all be decoded as silent nonsense.
func (t *TxQueue) adopt() error {
	q := &t.q

	if q.sq_stride != 64 {
		return fmt.Errorf("mlx5: the send queue has %d-byte blocks, want 64", q.sq_stride)
	}
	if q.cq_cqe_size != 64 {
		return fmt.Errorf("mlx5: the completion queue has %d-byte entries, want 64", q.cq_cqe_size)
	}
	if n := uint32(q.sq_wqe_cnt); n == 0 || n&(n-1) != 0 {
		return fmt.Errorf("mlx5: the send queue holds %d blocks, which is not a power of two", n)
	}
	if n := uint32(q.cq_cqe_cnt); n == 0 || n&(n-1) != 0 {
		return fmt.Errorf("mlx5: the completion queue holds %d entries, which is not a power of two", n)
	}
	if uintptr(q.sq_buf)&63 != 0 {
		return fmt.Errorf("mlx5: the send queue is not aligned to a block boundary")
	}
	if uintptr(q.cq_buf)&63 != 0 {
		return fmt.Errorf("mlx5: the completion queue is not aligned to an entry boundary")
	}

	t.SQ = unsafe.Slice((*byte)(q.sq_buf), uintptr(q.sq_wqe_cnt)*uintptr(q.sq_stride))
	t.CQ = unsafe.Slice((*byte)(q.cq_buf), uintptr(q.cq_cqe_cnt)*uintptr(q.cq_cqe_size))
	t.SQDbrec = (*uint32)(unsafe.Pointer(q.sq_dbrec))
	t.CQDbrec = (*uint32)(unsafe.Pointer(q.cq_dbrec))
	t.UAR = unsafe.Pointer(q.uar)
	t.BFSize = uint32(q.bf_size)
	t.CQESize = uint32(q.cq_cqe_size)
	t.QPN = uint32(q.qpn)
	t.CQN = uint32(q.cqn)
	return nil
}

// Blocks is how many 64-byte work queue buffer blocks the send queue holds, and
// so how many single-block packets can be outstanding.
func (t *TxQueue) Blocks() int { return int(t.q.sq_wqe_cnt) }

// CQEntries is how many completions the completion queue holds.
func (t *TxQueue) CQEntries() int { return int(t.q.cq_cqe_cnt) }

// Log2Blocks is the base-two logarithm of the send queue length.
func (t *TxQueue) Log2Blocks() int { return bits.TrailingZeros32(uint32(t.q.sq_wqe_cnt)) }

// Close destroys the queue. Nothing may be in flight: the NIC may still be
// reading frames the queue points at.
func (t *TxQueue) Close() error {
	if t.q.qp == nil && t.q.cq == nil {
		return nil
	}
	var errbuf [errBufSize]C.char
	if C.pio_destroy_txq(&t.q, &errbuf[0], errBufSize) != 0 {
		return cErr(&errbuf[0], "destroying a transmit queue")
	}
	t.SQ, t.CQ = nil, nil
	t.SQDbrec, t.CQDbrec, t.UAR = nil, nil, nil
	return nil
}

// RxQueue is the memory of one receive queue, as the packet path sees it.
type RxQueue struct {
	// RQ is the receive work queue entry buffer and CQ the completion queue
	// buffer. Both alias memory libmlx5 allocated and the NIC reads or writes.
	RQ []byte
	CQ []byte

	// RQDbrec is where the number of posted buffers is published, and CQDbrec
	// how far software has read the completions.
	RQDbrec *uint32
	CQDbrec *uint32

	// Stride is the size of one receive entry, CQESize of one completion.
	Stride  uint32
	CQESize uint32
	CQN     uint32

	q C.struct_pio_rxq
}

// CreateRxQueue creates a receive queue able to hold at least depth buffers.
//
// It receives nothing until Steer says what should come to it: the kernel still
// owns the port, and everything not claimed by a rule goes to the kernel as
// usual.
func (d *Device) CreateRxQueue(depth int) (*RxQueue, error) {
	if d.ctx == nil {
		return nil, fmt.Errorf("mlx5: device is closed")
	}
	if depth <= 0 {
		return nil, fmt.Errorf("mlx5: receive queue depth %d", depth)
	}

	r := &RxQueue{}
	var errbuf [errBufSize]C.char
	if C.pio_create_rxq(d.ctx, C.uint32_t(depth), &r.q, &errbuf[0], errBufSize) != 0 {
		return nil, cErr(&errbuf[0], "creating a receive queue of %d", depth)
	}
	if err := r.adopt(); err != nil {
		var e [errBufSize]C.char
		C.pio_destroy_rxq(&r.q, &e[0], errBufSize)
		return nil, err
	}
	return r, nil
}

func (r *RxQueue) adopt() error {
	q := &r.q

	// One buffer per entry means one sixteen-byte data segment per entry. A
	// longer stride would mean the provider gave several, which the refill
	// path does not write and the NIC would read as stale addresses.
	if q.rq_stride != 16 {
		return fmt.Errorf("mlx5: the receive queue has %d-byte entries, want 16", q.rq_stride)
	}
	if q.cq_cqe_size != 64 {
		return fmt.Errorf("mlx5: the completion queue has %d-byte entries, want 64", q.cq_cqe_size)
	}
	if n := uint32(q.rq_wqe_cnt); n == 0 || n&(n-1) != 0 {
		return fmt.Errorf("mlx5: the receive queue holds %d entries, which is not a power of two", n)
	}
	if n := uint32(q.cq_cqe_cnt); n == 0 || n&(n-1) != 0 {
		return fmt.Errorf("mlx5: the completion queue holds %d entries, which is not a power of two", n)
	}

	r.RQ = unsafe.Slice((*byte)(q.rq_buf), uintptr(q.rq_wqe_cnt)*uintptr(q.rq_stride))
	r.CQ = unsafe.Slice((*byte)(q.cq_buf), uintptr(q.cq_cqe_cnt)*uintptr(q.cq_cqe_size))
	r.RQDbrec = (*uint32)(unsafe.Pointer(q.rq_dbrec))
	r.CQDbrec = (*uint32)(unsafe.Pointer(q.cq_dbrec))
	r.Stride = uint32(q.rq_stride)
	r.CQESize = uint32(q.cq_cqe_size)
	r.CQN = uint32(q.cqn)
	return nil
}

// Entries is how many buffers the receive queue holds, and CQEntries how many
// completions its completion queue holds.
func (r *RxQueue) Entries() int   { return int(r.q.rq_wqe_cnt) }
func (r *RxQueue) CQEntries() int { return int(r.q.cq_cqe_cnt) }

// RxGroup is a set of receive queues that one steering rule can name, with
// packets spread across them by hash.
//
// A rule cannot name a queue directly, and two rules matching the same packets
// would not divide them: a packet takes the first rule that matches it. Sharing
// one kind of traffic between several queues therefore means one rule, one
// table, and the NIC choosing between them.
type RxGroup struct {
	g C.struct_pio_rx_group
}

// NewRxGroup makes the given queues one destination.
func (d *Device) NewRxGroup(queues []*RxQueue) (*RxGroup, error) {
	if d.ctx == nil {
		return nil, fmt.Errorf("mlx5: device is closed")
	}
	if len(queues) == 0 {
		return nil, fmt.Errorf("mlx5: a receive group of no queues")
	}

	// The C side takes an array of queue descriptions, so they are gathered
	// into one here rather than passed as Go pointers to Go pointers, which
	// cgo does not allow.
	qs := make([]C.struct_pio_rxq, len(queues))
	for i, q := range queues {
		qs[i] = q.q
	}

	g := &RxGroup{}
	var errbuf [errBufSize]C.char
	if C.pio_create_rx_group(d.ctx, &qs[0], C.uint32_t(len(queues)), &g.g, &errbuf[0], errBufSize) != 0 {
		return nil, cErr(&errbuf[0], "making %d receive queues one destination", len(queues))
	}
	return g, nil
}

// Close destroys the group: first the rule, so nothing arrives afterwards, then
// the queue pair and the table. The queues themselves outlive it.
func (g *RxGroup) Close() error {
	if g.g.qp == nil && g.g.ind_table == nil {
		return nil
	}
	var errbuf [errBufSize]C.char
	if C.pio_destroy_rx_group(&g.g, &errbuf[0], errBufSize) != 0 {
		return cErr(&errbuf[0], "destroying a receive group")
	}
	return nil
}

// Steering says which packets a receive group should be given.
//
// Every field the card can match is here, and the card matches them in
// hardware at no per-packet cost. A field left at its zero value is not
// matched, and anything a rule does not match still goes to the kernel -- which
// is what lets a program take the traffic it wants while the interface keeps
// working for everything else.
type Steering struct {
	// MAC is the destination address to match, when MACSet. Ignored when
	// Promiscuous.
	MAC    [6]byte
	MACSet bool

	// VLAN is the tag identifier to match, or -1 for any. Only the identifier
	// is matched, so a packet's priority bits do not decide whether it
	// arrives.
	VLAN int

	// EtherType is matched when non-zero, in host order.
	EtherType uint16

	// SrcIP and DstIP are matched under SrcMask and DstMask, all in network
	// order. A zero mask matches any address, which is how "only the port
	// matters" is expressed.
	SrcIP, DstIP     [4]byte
	SrcMask, DstMask [4]byte

	// IPProto is the IP protocol number to match, when set: 6 for TCP, 17 for
	// UDP. A port cannot be matched without one, because the card looks for a
	// port at an offset inside a protocol it was told to expect.
	IPProto    uint8
	IPProtoSet bool

	// SrcPort and DstPort are matched when their Set flag is on, in host
	// order.
	SrcPort, DstPort       uint16
	SrcPortSet, DstPortSet bool

	// Promiscuous takes every packet the port sees, whoever it is addressed
	// to. It needs the same privilege as the rest of this and is what a
	// capture or a benchmark sink wants.
	Promiscuous bool
}

// Steer installs the rules that send packets to this group, replacing any
// already in place. Packets that match no rule still go to the kernel.
//
// Each Steering is one conjunction, and a packet matching any of them arrives:
// a rule cannot express a union, so a filter that is one -- two ports, say --
// becomes one rule per alternative.
func (d *Device) Steer(g *RxGroup, ss ...Steering) error {
	if d.ctx == nil {
		return fmt.Errorf("mlx5: device is closed")
	}
	if len(ss) == 0 {
		return fmt.Errorf("mlx5: steering needs at least one rule")
	}
	ms := make([]C.struct_pio_match, len(ss))
	for i, s := range ss {
		ms[i] = s.toC()
	}
	var errbuf [errBufSize]C.char
	if C.pio_group_steer(d.ctx, &g.g, &ms[0], C.uint32_t(len(ms)), &errbuf[0], errBufSize) != 0 {
		return cErr(&errbuf[0], "steering packets to a receive group")
	}
	return nil
}

func (s Steering) toC() C.struct_pio_match {
	var m C.struct_pio_match
	for i, b := range s.MAC {
		m.dst_mac[i] = C.uint8_t(b)
	}
	m.have_mac = cbool(s.MACSet)
	m.vlan = C.int(-1)
	if s.VLAN >= 0 {
		m.vlan = C.int(s.VLAN)
	}
	m.ether_type = C.uint16_t(s.EtherType)
	m.have_ether_type = cbool(s.EtherType != 0)
	m.src_ip = C.uint32_t(nativeU32(s.SrcIP[:]))
	m.dst_ip = C.uint32_t(nativeU32(s.DstIP[:]))
	m.src_mask = C.uint32_t(nativeU32(s.SrcMask[:]))
	m.dst_mask = C.uint32_t(nativeU32(s.DstMask[:]))
	m.ip_proto = C.uint8_t(s.IPProto)
	m.have_proto = cbool(s.IPProtoSet)
	m.src_port = C.uint16_t(s.SrcPort)
	m.dst_port = C.uint16_t(s.DstPort)
	m.have_src_port = cbool(s.SrcPortSet)
	m.have_dst_port = cbool(s.DstPortSet)
	m.promisc = cbool(s.Promiscuous)
	return m
}

func cbool(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

// nativeU32 reads four address bytes as the card wants them: the bytes in wire
// order, in a word the host can hold.
func nativeU32(b []byte) uint32 {
	_ = b[3]
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// Close destroys the queue. Its group must be closed first, so that nothing
// arrives after the place it would arrive in has gone.
func (r *RxQueue) Close() error {
	if r.q.wq == nil && r.q.cq == nil {
		return nil
	}
	var errbuf [errBufSize]C.char
	if C.pio_destroy_rxq(&r.q, &errbuf[0], errBufSize) != 0 {
		return cErr(&errbuf[0], "destroying a receive queue")
	}
	r.RQ, r.CQ = nil, nil
	r.RQDbrec, r.CQDbrec = nil, nil
	return nil
}
