//go:build linux && cgo && dpdk && amd64

package eal

/*
#include <stdlib.h>
#include "shim.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/atoonk/packetio/dpdk/internal/queue"
)

// Port is one DPDK ethernet port.
type Port uint16

// DevInfo is what a device says it can do.
type DevInfo struct {
	Driver        string
	Socket        int
	MaxRxQueues   int
	MaxTxQueues   int
	MinRxBufSize  int
	MaxRxPktLen   int
	RxOffloadCapa uint64
	TxOffloadCapa uint64
	MaxRxDesc     int
	MaxTxDesc     int
	MAC           [6]byte

	// Bifurcated is true when the kernel still has a network interface for
	// this device, which is what decides whether steering can promise that
	// unmatched traffic still reaches the host.
	Bifurcated bool
}

// Offload bits asked for at configure time.
const (
	RxChecksum = C.PIO_RX_OFFLOAD_CHECKSUM
	TxChecksum = C.PIO_TX_OFFLOAD_CHECKSUM
	TxTSO      = C.PIO_TX_OFFLOAD_TSO
)

// Probe brings up one device and returns the port it became.
func Probe(devargs string) (Port, error) {
	var (
		port   C.uint16_t
		errbuf [errLen]C.char
		rc     C.int
	)
	c := C.CString(devargs)
	defer C.free(unsafe.Pointer(c))
	onEAL(func() { rc = C.pio_probe(c, &port, &errbuf[0], errLen) })
	if rc != 0 {
		return 0, fmt.Errorf("%w%s", cerr(&errbuf[0], "opening %s", devargs), logHint())
	}
	return Port(port), nil
}

// PortOf reports which port a device already brought up became, which is how
// the first device is found: the environment probes it from its own arguments
// and does not say what port it made.
func PortOf(devargs string) (Port, bool) {
	var (
		port C.uint16_t
		rc   C.int
	)
	c := C.CString(devargs)
	defer C.free(unsafe.Pointer(c))
	onEAL(func() { rc = C.pio_find_port(c, &port) })
	if rc != 0 {
		return 0, false
	}
	return Port(port), true
}

// DeviceHandle is the bus device behind a port.
//
// It has to be taken before the port is closed. Closing releases the port id,
// and from then on there is no way back from the port to the device -- which
// means no way to detach it, and no way to open the same device again. That is
// how a program that opens and closes in a loop runs out of devices.
type DeviceHandle unsafe.Pointer

// Handle is the bus device behind a port, or nil if there is none.
func Handle(p Port) DeviceHandle {
	var h unsafe.Pointer
	onEAL(func() { h = C.pio_dev_handle(C.uint16_t(p)) })
	return DeviceHandle(h)
}

// Remove detaches a device, so the same one can be opened again later.
func Remove(h DeviceHandle) error {
	if h == nil {
		return nil
	}
	var (
		errbuf [errLen]C.char
		rc     C.int
	)
	onEAL(func() { rc = C.pio_remove(unsafe.Pointer(h), &errbuf[0], errLen) })
	if rc != 0 {
		return cerr(&errbuf[0], "detaching the device")
	}
	return nil
}

// Info reports what the device can do.
func Info(p Port) (DevInfo, error) {
	var (
		info   C.struct_pio_dev_info
		errbuf [errLen]C.char
		rc     C.int
	)
	onEAL(func() { rc = C.pio_dev_info(C.uint16_t(p), &info, &errbuf[0], errLen) })
	if rc != 0 {
		return DevInfo{}, cerr(&errbuf[0], "asking port %d what it can do", p)
	}
	d := DevInfo{
		Driver:        C.GoString(&info.driver[0]),
		Socket:        int(info.socket),
		MaxRxQueues:   int(info.max_rx_queues),
		MaxTxQueues:   int(info.max_tx_queues),
		MinRxBufSize:  int(info.min_rx_bufsize),
		MaxRxPktLen:   int(info.max_rx_pktlen),
		RxOffloadCapa: uint64(info.rx_offload_capa),
		TxOffloadCapa: uint64(info.tx_offload_capa),
		MaxRxDesc:     int(info.rx_desc_max),
		MaxTxDesc:     int(info.tx_desc_max),
		Bifurcated:    info.is_bifurcated != 0,
	}
	for i := range d.MAC {
		d.MAC[i] = byte(info.mac[i])
	}
	return d, nil
}

// SocketID is the memory node the device is attached to.
func SocketID(p Port) int { return int(C.pio_socket_id(C.uint16_t(p))) }

// IOVAMode reports how the NIC addresses memory: 1 is physical, 2 virtual.
func IOVAMode() int { return int(C.pio_iova_mode()) }

// Configure sets the queue counts and offloads. It must come before any queue
// is set up.
//
// It reports which of the requested offloads the device actually took, which is
// not always what was asked for: a PMD that does not offer one has it dropped
// silently rather than failing the whole configure. Capabilities are built from
// the returned mask, never from the request.
func Configure(p Port, nrx, ntx, mtu int, offloads uint32) (uint32, error) {
	var (
		errbuf  [errLen]C.char
		rc      C.int
		applied C.uint32_t
	)
	onEAL(func() {
		rc = C.pio_configure(C.uint16_t(p), C.uint16_t(nrx), C.uint16_t(ntx),
			C.uint32_t(mtu), C.uint32_t(offloads), &applied, &errbuf[0], errLen)
	})
	if rc != 0 {
		return 0, fmt.Errorf("%w%s", cerr(&errbuf[0], "configuring port %d", p), logHint())
	}
	return uint32(applied), nil
}

// SetupRx and SetupTx create one queue each.
func SetupRx(p Port, q, desc, socket int, mp *Mempool) error {
	var (
		errbuf [errLen]C.char
		rc     C.int
	)
	onEAL(func() {
		rc = C.pio_rx_queue_setup(C.uint16_t(p), C.uint16_t(q), C.uint16_t(desc),
			C.int(socket), mp.mp, &errbuf[0], errLen)
	})
	if rc != 0 {
		return fmt.Errorf("%w%s", cerr(&errbuf[0], "port %d", p), logHint())
	}
	return nil
}

// SetupTx creates one transmit queue.
func SetupTx(p Port, q, desc, socket int) error {
	var (
		errbuf [errLen]C.char
		rc     C.int
	)
	onEAL(func() {
		rc = C.pio_tx_queue_setup(C.uint16_t(p), C.uint16_t(q), C.uint16_t(desc),
			C.int(socket), &errbuf[0], errLen)
	})
	if rc != 0 {
		return fmt.Errorf("%w%s", cerr(&errbuf[0], "port %d", p), logHint())
	}
	return nil
}

// Start, Stop and Close move the port through its states.
func Start(p Port) error {
	var (
		errbuf [errLen]C.char
		rc     C.int
	)
	onEAL(func() { rc = C.pio_start(C.uint16_t(p), &errbuf[0], errLen) })
	if rc != 0 {
		return fmt.Errorf("%w%s", cerr(&errbuf[0], "port %d", p), logHint())
	}
	return nil
}

// Stop halts the port. The driver frees the buffers still in its rings, which
// arrive back through the queues' returned rings.
func Stop(p Port) error {
	var (
		errbuf [errLen]C.char
		rc     C.int
	)
	onEAL(func() { rc = C.pio_stop(C.uint16_t(p), &errbuf[0], errLen) })
	if rc != 0 {
		return cerr(&errbuf[0], "port %d", p)
	}
	return nil
}

// Close releases the port's resources.
func Close(p Port) error {
	var (
		errbuf [errLen]C.char
		rc     C.int
	)
	onEAL(func() { rc = C.pio_eal_close(C.uint16_t(p), &errbuf[0], errLen) })
	if rc != 0 {
		return cerr(&errbuf[0], "port %d", p)
	}
	return nil
}

// ErrNoPromiscuous is what Promiscuous returns from a driver that has no such
// mode at all, as distinct from one that has it and refused.
var ErrNoPromiscuous = errors.New("this driver has no promiscuous mode")

// Promiscuous asks the port to take every packet it sees.
func Promiscuous(p Port, on bool) error {
	var (
		errbuf [errLen]C.char
		rc     C.int
		v      C.int
	)
	if on {
		v = 1
	}
	onEAL(func() { rc = C.pio_promiscuous(C.uint16_t(p), v, &errbuf[0], errLen) })
	if rc == C.PIO_ENOTSUP {
		return ErrNoPromiscuous
	}
	if rc != 0 {
		return cerr(&errbuf[0], "port %d", p)
	}
	return nil
}

// Link reports whether the port has carrier, and at what speed in megabits.
func Link(p Port) (up bool, speed uint32) {
	var (
		u  C.int
		sp C.uint32_t
	)
	if C.pio_link_status(C.uint16_t(p), &u, &sp) != 0 {
		return false, 0
	}
	return u != 0, uint32(sp)
}

// WaitLink waits for the port to gain carrier, up to timeout, and reports
// whether it did and at what speed.
//
// A port is not ready the moment it starts. A copper port -- an X550 at
// 10GBASE-T, say -- negotiates for several seconds, and everything transmitted
// meanwhile piles into a ring that is not draining, so a program that sends
// immediately spins on a full ring instead of sending. Waiting is what a DPDK
// application is expected to do and what this backend does for the caller.
func WaitLink(p Port, timeout time.Duration) (bool, uint32) {
	deadline := time.Now().Add(timeout)
	for {
		up, speed := Link(p)
		if up || !time.Now().Before(deadline) {
			return up, speed
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Stats are the port's own counters, which are the ones to believe: a receive
// queue's software count says what the application took, not what arrived.
type Stats struct {
	InPackets, OutPackets uint64
	InBytes, OutBytes     uint64
	Missed, InErrors      uint64
	OutErrors, NoBuffer   uint64
}

// PortStats reads the port's counters.
func PortStats(p Port) (Stats, error) {
	var (
		st     C.struct_pio_stats
		errbuf [errLen]C.char
		rc     C.int
	)
	onEAL(func() { rc = C.pio_stats(C.uint16_t(p), &st, &errbuf[0], errLen) })
	if rc != 0 {
		return Stats{}, cerr(&errbuf[0], "port %d", p)
	}
	return Stats{
		InPackets: uint64(st.ipackets), OutPackets: uint64(st.opackets),
		InBytes: uint64(st.ibytes), OutBytes: uint64(st.obytes),
		Missed: uint64(st.imissed), InErrors: uint64(st.ierrors),
		OutErrors: uint64(st.oerrors), NoBuffer: uint64(st.rx_nombuf),
	}, nil
}

// ------------------------------------------------------------------ memory

// Region is the frame memory: one memzone, as a Go slice and as the address
// this process knows it by.
//
// There is deliberately no IOVA here. The address the NIC uses for a frame is
// the mbuf's business, written into it when the mempool is populated, and where
// the NIC addresses memory physically there is one per page rather than one
// per region. PageSize is how big those pages are.
type Region struct {
	Bytes    []byte
	VA       uint64
	PageSize uint64
	name     string
}

// ReserveRegion takes one memzone of size bytes, contiguous in this process's
// address space.
func ReserveRegion(name string, size, align, socket int) (*Region, error) {
	var (
		addr   unsafe.Pointer
		page   C.uint64_t
		errbuf [errLen]C.char
		rc     C.int
	)
	c := C.CString(name)
	defer C.free(unsafe.Pointer(c))
	onEAL(func() {
		rc = C.pio_region_reserve(c, C.size_t(size), C.size_t(align), C.int(socket),
			&addr, &page, &errbuf[0], errLen)
	})
	if rc != 0 {
		return nil, fmt.Errorf("%w%s", cerr(&errbuf[0], "frame memory"), logHint())
	}
	return &Region{
		Bytes:    unsafe.Slice((*byte)(addr), size),
		VA:       uint64(uintptr(addr)),
		PageSize: uint64(page),
		name:     name,
	}, nil
}

// Free releases the region.
func (r *Region) Free() error {
	if r == nil || r.name == "" {
		return nil
	}
	var (
		errbuf [errLen]C.char
		rc     C.int
	)
	c := C.CString(r.name)
	defer C.free(unsafe.Pointer(c))
	onEAL(func() { rc = C.pio_region_free(c, &errbuf[0], errLen) })
	r.Bytes, r.name = nil, ""
	if rc != 0 {
		return cerr(&errbuf[0], "freeing frame memory")
	}
	return nil
}

// Mempool is one queue's pool, and the two rings that stand between the driver
// and packetio's free list.
type Mempool struct {
	// mu guards the pool pointer against Free running under a monitoring
	// goroutine's Stats call. It is not on any packet path -- the driver
	// reaches the rings directly and never comes through here -- so the cost
	// is one uncontended lock per Stats, about once a second.
	mu   sync.Mutex
	mp   unsafe.Pointer
	pool *C.struct_pio_pool

	// The last counter values, kept so that Stats after Close reports what
	// the queue did rather than zero, and so that nothing reads the C struct
	// once it has been freed.
	lastDrops uint64
	lastEmpty uint64

	// Supply and Returned are the same rings the driver uses, seen from Go.
	Supply   *queue.Ring
	Returned *queue.Ring

	// VA is the mempool's address, which every mbuf handed to the driver is
	// stamped with so that it comes back to this queue.
	VA uint64
}

// NewMempool builds one queue's mempool over its slice of the region.
func NewMempool(name string, r *Region, firstFrame, frames, frameSize, ringSize, socket int) (*Mempool, error) {
	var (
		mp     unsafe.Pointer
		pool   *C.struct_pio_pool
		errbuf [errLen]C.char
		rc     C.int
	)
	c := C.CString(name)
	defer C.free(unsafe.Pointer(c))

	off := firstFrame * frameSize
	addr := unsafe.Pointer(&r.Bytes[off])
	onEAL(func() {
		rc = C.pio_mempool_new(c, addr, C.uint64_t(r.PageSize),
			C.uint32_t(frames), C.uint32_t(frameSize), C.uint32_t(ringSize),
			C.int(socket), &mp, &pool, &errbuf[0], errLen)
	})
	if rc != 0 {
		return nil, fmt.Errorf("%w%s", cerr(&errbuf[0], "the mempool for %s", name), logHint())
	}

	m := &Mempool{mp: mp, pool: pool, VA: uint64(uintptr(mp))}
	var err error
	if m.Supply, err = ringOver(&pool.supply, ringSize); err != nil {
		m.Free()
		return nil, err
	}
	if m.Returned, err = ringOver(&pool.returned, ringSize); err != nil {
		m.Free()
		return nil, err
	}
	return m, nil
}

// ringOver views one of the C rings from Go. The array and both indices are C
// memory; nothing is copied, which is what lets the driver and this process
// share them without a crossing.
func ringOver(r *C.struct_pio_ring, size int) (*queue.Ring, error) {
	buf := unsafe.Slice((*uint64)(unsafe.Pointer(r.buf)), size)
	return queue.NewRingOver(buf,
		(*uint32)(unsafe.Pointer(&r.head)),
		(*uint32)(unsafe.Pointer(&r.tail)))
}

// Layout reports what the mempool library actually built, so a caller can check
// it rather than trust it: DPDK pads between objects unless told not to, and an
// object that is not exactly one frame breaks every address in this backend.
func (m *Mempool) Layout() (header, elt, trailer int) {
	var h, e, t C.uint32_t
	C.pio_mempool_layout(m.mp, &h, &e, &t)
	return int(h), int(e), int(t)
}

// Drops is how many frees did not fit the returned ring, and Empty how many
// bulk gets the supply could not satisfy. A drop is a lost frame and must be
// zero; an empty is a receiver that did not fill fast enough.
//
// Both read zero once the pool is freed. The contract says every method on a
// closed queue must be safe, and Stats is the one a monitoring goroutine is
// most likely to call just after a device goes away.
func (m *Mempool) Drops() uint64 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pool == nil {
		return m.lastDrops
	}
	// The C side adds with a relaxed atomic; this load is the other half.
	return atomic.LoadUint64((*uint64)(unsafe.Pointer(&m.pool.drops)))
}

// Empty is how many bulk gets the supply could not satisfy.
func (m *Mempool) Empty() uint64 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pool == nil {
		return m.lastEmpty
	}
	return atomic.LoadUint64((*uint64)(unsafe.Pointer(&m.pool.empty)))
}

// Free releases the mempool and its rings.
func (m *Mempool) Free() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.mp == nil {
		m.mu.Unlock()
		return
	}
	mp, pool := m.mp, m.pool
	// Read the counters and drop the pointer before the memory goes, so that
	// a Stats racing this sees the last values rather than a freed struct.
	if pool != nil {
		m.lastDrops, m.lastEmpty = uint64(pool.drops), uint64(pool.empty)
	}
	m.mp, m.pool, m.Supply, m.Returned = nil, nil, nil, nil
	m.mu.Unlock()

	onEAL(func() { C.pio_mempool_free(mp, pool) })
}

// ---------------------------------------------------------------- fast path

// Queue is one port queue, and the PMD implementation the queue package drives.
// Its three methods are the only cgo on the packet path.
type Queue struct {
	Port  Port
	Index uint16
}

// TxBurst hands mbuf addresses to the driver and returns how many it took.
func (q Queue) TxBurst(mbufs []uint64) int {
	if len(mbufs) == 0 {
		return 0
	}
	return int(C.pio_tx_burst(C.uint16_t(q.Port), C.uint16_t(q.Index),
		(*C.uint64_t)(unsafe.Pointer(&mbufs[0])), C.uint16_t(len(mbufs))))
}

// RxBurst fills out with received mbuf addresses and returns how many.
func (q Queue) RxBurst(out []uint64) int {
	if len(out) == 0 {
		return 0
	}
	return int(C.pio_rx_burst(C.uint16_t(q.Port), C.uint16_t(q.Index),
		(*C.uint64_t)(unsafe.Pointer(&out[0])), C.uint16_t(len(out))))
}

// Poke asks the driver to look at its transmit completions.
func (q Queue) Poke() { C.pio_tx_poke(C.uint16_t(q.Port), C.uint16_t(q.Index)) }

// ----------------------------------------------------------------- steering

// Match is one steering rule, in the vocabulary the card matches.
type Match struct {
	MAC       [6]byte
	MACSet    bool
	VLAN      int // -1 for any
	EtherType uint16
	SrcIP     [4]byte
	DstIP     [4]byte
	SrcMask   [4]byte
	DstMask   [4]byte
	IPProto   uint8
	ProtoSet  bool
	SrcPort   uint16
	DstPort   uint16
	SrcSet    bool
	DstSet    bool
}

func (m Match) toC() C.struct_pio_match {
	var c C.struct_pio_match
	for i, b := range m.MAC {
		c.dst_mac[i] = C.uint8_t(b)
	}
	c.have_mac = cbool(m.MACSet)
	c.vlan = C.int(m.VLAN)
	c.ether_type = C.uint16_t(m.EtherType)
	c.have_ether_type = cbool(m.EtherType != 0)
	c.src_ip = C.uint32_t(wireU32(m.SrcIP))
	c.dst_ip = C.uint32_t(wireU32(m.DstIP))
	c.src_mask = C.uint32_t(wireU32(m.SrcMask))
	c.dst_mask = C.uint32_t(wireU32(m.DstMask))
	c.ip_proto = C.uint8_t(m.IPProto)
	c.have_proto = cbool(m.ProtoSet)
	c.src_port = C.uint16_t(m.SrcPort)
	c.dst_port = C.uint16_t(m.DstPort)
	c.have_src_port = cbool(m.SrcSet)
	c.have_dst_port = cbool(m.DstSet)
	return c
}

func cbool(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

// wireU32 reads four address bytes as the card wants them: the bytes in wire
// order, in a word the host can hold.
func wireU32(b [4]byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// Isolate puts the port in flow-isolated mode, where it receives nothing but
// what a rule matches and everything else stays with the kernel. It must be set
// before the port is configured.
func Isolate(p Port, on bool) error {
	var (
		errbuf [errLen]C.char
		rc     C.int
	)
	onEAL(func() { rc = C.pio_flow_isolate(C.uint16_t(p), cbool(on), &errbuf[0], errLen) })
	if rc != 0 {
		return cerr(&errbuf[0], "port %d", p)
	}
	return nil
}

// Flow installs one steering rule, or with validateOnly only asks whether the
// device would take it. Rules go in after the port is started: rte_flow_create
// refuses on a stopped port.
func Flow(p Port, m Match, queues []uint16, validateOnly bool) error {
	if len(queues) == 0 {
		return fmt.Errorf("dpdk: a steering rule with no queue to send to")
	}
	var (
		errbuf [errLen]C.char
		rc     C.int
	)
	cm := m.toC()
	qs := make([]C.uint16_t, len(queues))
	for i, q := range queues {
		qs[i] = C.uint16_t(q)
	}
	onEAL(func() {
		rc = C.pio_flow_create(C.uint16_t(p), &cm, &qs[0], C.uint16_t(len(qs)),
			cbool(validateOnly), &errbuf[0], errLen)
	})
	if rc != 0 {
		return cerr(&errbuf[0], "steering on port %d", p)
	}
	return nil
}

// FlushFlows removes every rule on the port.
func FlushFlows(p Port) error {
	var (
		errbuf [errLen]C.char
		rc     C.int
	)
	onEAL(func() { rc = C.pio_flow_flush(C.uint16_t(p), &errbuf[0], errLen) })
	if rc != 0 {
		return cerr(&errbuf[0], "port %d", p)
	}
	return nil
}
