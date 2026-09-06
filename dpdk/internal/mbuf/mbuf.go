// Package mbuf reads and writes the DPDK packet buffer header from Go.
//
// A DPDK PMD describes every packet with an rte_mbuf: a 128-byte header
// carrying the address of the data, how many bytes are valid, and who owns it.
// The packet path has to write those fields on the way out and read them on the
// way in, once per packet, so doing it through cgo would put a crossing on the
// hot path -- which is the one thing this backend is not allowed to do.
//
// It does not need to. The mbufs live inside the frame region, because the
// region is a DPDK mempool populated over it: one object per frame, laid out as
//
//	frame i:  [ 64 B mempool header | 128 B rte_mbuf | headroom | packet ... ]
//
// So an mbuf is at a known offset inside a region this package already has as
// an ordinary []byte, and every field is a plain indexed load or store. There
// is no unsafe here and no cgo; the only pointer arithmetic in the backend is
// turning the addresses a burst returns into offsets, which happens one level
// up and once per batch.
//
// The offsets below were measured, not guessed: see Offsets, and the layout
// test that checks them against the installed headers when DPDK is present.
package mbuf

import (
	"encoding/binary"
	"fmt"
	"math/bits"
)

// Size is sizeof(struct rte_mbuf). DPDK asserts this is two cache lines and so
// does the cgo layer, because everything below indexes from it.
const Size = 128

// Field offsets inside an rte_mbuf, from DPDK 23.11 on linux/amd64 with
// RTE_IOVA_IN_MBUF=1. Read out of the headers with offsetof on 28 August 2026
// and checked by the layout test wherever DPDK is installed.
//
// They are unexported because nothing outside this package should index an
// mbuf by hand; Offsets reports them for the test and for a bug report.
const (
	offBufAddr   = 0  // void *buf_addr
	offBufIOVA   = 8  // rte_iova_t buf_iova
	offRearm     = 16 // the 8 bytes covering the four fields below
	offDataOff   = 16 // uint16 data_off
	offRefcnt    = 18 // uint16 refcnt
	offNbSegs    = 20 // uint16 nb_segs
	offPort      = 22 // uint16 port
	offOlFlags   = 24 // uint64 ol_flags
	offPktLen    = 36 // uint32 pkt_len
	offDataLen   = 40 // uint16 data_len
	offVLANTCI   = 42 // uint16 vlan_tci
	offBufLen    = 54 // uint16 buf_len
	offPool      = 56 // struct rte_mempool *pool
	offNext      = 64 // struct rte_mbuf *next
	offTxOffload = 72 // uint64 tx_offload
)

// Offsets reports the field offsets this package uses, for the layout test and
// for anyone reading a bug report from a machine whose DPDK differs.
func Offsets() map[string]int {
	return map[string]int{
		"buf_addr": offBufAddr, "buf_iova": offBufIOVA, "rearm_data": offRearm,
		"data_off": offDataOff, "refcnt": offRefcnt, "nb_segs": offNbSegs,
		"port": offPort, "ol_flags": offOlFlags, "pkt_len": offPktLen,
		"data_len": offDataLen, "vlan_tci": offVLANTCI, "buf_len": offBufLen,
		"pool": offPool, "next": offNext, "tx_offload": offTxOffload,
	}
}

// Receive flags worth naming, from rte_mbuf_core.h. A backend that reports a
// checksum result to packetio needs exactly these two.
const (
	RxIPChecksumGood uint64 = 1 << 7 // RTE_MBUF_F_RX_IP_CKSUM_GOOD
	RxL4ChecksumGood uint64 = 1 << 8 // RTE_MBUF_F_RX_L4_CKSUM_GOOD
)

// Headroom is RTE_PKTMBUF_HEADROOM, the bytes a driver leaves in front of a
// received packet. It is a DPDK build constant rather than a per-device
// setting, and the cgo layer asserts the installed headers still say 128.
const Headroom = 128

// PortInvalid is RTE_MBUF_PORT_INVALID, what a port field means when nothing
// has claimed the packet.
const PortInvalid = 0xffff

// Layout says where things sit inside one frame.
//
// Every number here is decided at Open, when the mempool is built, and checked
// against what the mempool library actually did rather than assumed: DPDK will
// insert padding between objects unless it is told not to, and one object that
// is not exactly one frame breaks every address calculation in this backend.
type Layout struct {
	// FrameSize is one packetio frame, and also one mempool object. It must be
	// a power of two so that finding a frame from an address inside it is a
	// mask rather than a divide -- the same reason internal/pool insists on it.
	FrameSize int

	// ObjHeader is what the mempool puts in front of each object, and MbufSize
	// the rte_mbuf plus its private area. The packet's data buffer starts
	// after both.
	ObjHeader int
	MbufSize  int

	// Headroom is what the PMD leaves in front of a received packet
	// (RTE_PKTMBUF_HEADROOM), and what this backend also leaves in front of a
	// transmitted one so that both directions look the same to a caller.
	Headroom int
}

// Validate reports why a layout cannot be used, or nil.
func (l Layout) Validate() error {
	switch {
	case l.FrameSize <= 0 || bits.OnesCount64(uint64(l.FrameSize)) != 1:
		return fmt.Errorf("dpdk: a frame size of %d, which must be a power of two", l.FrameSize)
	case l.ObjHeader < 0 || l.MbufSize < Size || l.Headroom < 0:
		return fmt.Errorf("dpdk: an object header of %d, mbuf of %d and headroom of %d",
			l.ObjHeader, l.MbufSize, l.Headroom)
	case l.DataStart() >= l.FrameSize:
		// Nothing would be left to put a packet in.
		return fmt.Errorf("dpdk: a %d-byte frame has no room for a packet after %d bytes of "+
			"object header, mbuf and headroom", l.FrameSize, l.DataStart())
	}
	return nil
}

// MbufOffset is where frame i's mbuf begins, given the frame's own offset.
func (l Layout) MbufOffset(frame int) int { return frame + l.ObjHeader }

// BufferOffset is where frame i's data buffer begins: what the mbuf's buf_addr
// points at, before any headroom.
func (l Layout) BufferOffset(frame int) int { return frame + l.ObjHeader + l.MbufSize }

// DataStart is the offset within a frame at which a packet's first byte sits
// when the headroom is untouched. It is what a descriptor's Addr names.
func (l Layout) DataStart() int { return l.ObjHeader + l.MbufSize + l.Headroom }

// BufferLen is how many bytes of a frame the NIC may fill: everything after the
// header and the mbuf, headroom included, since the PMD is given buf_addr and
// counts from there.
func (l Layout) BufferLen() int { return l.FrameSize - l.ObjHeader - l.MbufSize }

// MaxPacket is the longest packet that fits behind the headroom.
func (l Layout) MaxPacket() int { return l.FrameSize - l.DataStart() }

// FrameOf rounds an offset anywhere inside a frame down to that frame's start.
// A mask, because FrameSize is a power of two, and this runs once per packet.
func (l Layout) FrameOf(off uint64) uint64 { return off &^ uint64(l.FrameSize-1) }

// The accessors below all take the region and an offset to an mbuf within it.
// Every field is host-order: this is a C struct in memory shared with a library
// running in the same process, not anything that goes on a wire.

// SetBuffer writes the fields that never change for a frame: where its data
// lives, what the NIC knows that address as, and how long the buffer is.
//
// The mempool's own initialiser writes these at Open, so this exists for the
// tests and for a backend that builds its region by hand.
func SetBuffer(b []byte, m int, addr, iova uint64, bufLen uint16) {
	binary.NativeEndian.PutUint64(b[m+offBufAddr:], addr)
	binary.NativeEndian.PutUint64(b[m+offBufIOVA:], iova)
	binary.NativeEndian.PutUint16(b[m+offBufLen:], bufLen)
}

// BufAddr is the address the PMD will read the packet from, and BufLen how many
// bytes it may use.
func BufAddr(b []byte, m int) uint64 { return binary.NativeEndian.Uint64(b[m+offBufAddr:]) }

// BufIOVA is the address the NIC has for the same buffer: the virtual address
// where IOVA is VA, a physical one where it is not.
func BufIOVA(b []byte, m int) uint64 { return binary.NativeEndian.Uint64(b[m+offBufIOVA:]) }

// BufLen is how many bytes of the data buffer the NIC may fill.
func BufLen(b []byte, m int) uint16 { return binary.NativeEndian.Uint16(b[m+offBufLen:]) }

// Pool is the mempool this mbuf will be freed into, and SetPool changes it.
//
// Changing it is what makes a forwarder work. A frame received on one queue and
// transmitted on another is freed by the PMD into whatever pool this field
// names, so setting it to the transmitting queue's pool at Transmit is what
// brings the frame back through that queue's Reclaim -- and from there to the
// receive queue it belongs to. Leave it alone and the frame lands in the wrong
// free list, which is the silent aliasing bug internal/pool exists to catch.
func Pool(b []byte, m int) uint64 { return binary.NativeEndian.Uint64(b[m+offPool:]) }

// SetPool names the mempool the PMD should free this mbuf into.
func SetPool(b []byte, m int, pool uint64) {
	binary.NativeEndian.PutUint64(b[m+offPool:], pool)
}

// rearm packs data_off, refcnt, nb_segs and port into the one eight-byte word
// DPDK calls rearm_data, so resetting an mbuf is a single store rather than
// four. It is the same trick the vector PMDs use.
func rearm(dataOff, refcnt, nbSegs, port uint16) uint64 {
	return uint64(dataOff) | uint64(refcnt)<<16 | uint64(nbSegs)<<32 | uint64(port)<<48
}

// Reset puts an mbuf back into the state a freshly allocated one has: one
// segment, one reference, nothing chained, the packet starting after the
// headroom. It is what Fill does before handing a frame to the PMD.
func Reset(b []byte, m int, headroom uint16) {
	binary.NativeEndian.PutUint64(b[m+offRearm:], rearm(headroom, 1, 1, PortInvalid))
	binary.NativeEndian.PutUint64(b[m+offOlFlags:], 0)
	binary.NativeEndian.PutUint64(b[m+offNext:], 0)
	binary.NativeEndian.PutUint64(b[m+offTxOffload:], 0)
	binary.NativeEndian.PutUint32(b[m+offPktLen:], 0)
	binary.NativeEndian.PutUint16(b[m+offDataLen:], 0)
}

// ResetRx puts an mbuf into the state the driver needs to receive into it,
// writing only the fields receiving will not overwrite: the rearm word, the
// chain pointer, and the owning pool.
//
// Reset writes six fields; this writes three, and the difference was 18% of a
// receive core in the profile that prompted it. The fields skipped are safe to
// skip for a reason each: ol_flags, pkt_len and data_len are written by the
// driver for every received packet before anything reads them, and tx_offload
// is only ever read by a transmit that also sets it -- the checksum path calls
// SetTxOffload, and the plain path leaves ol_flags zero, which tells the
// driver to ignore whatever tx_offload holds.
func ResetRx(b []byte, m int, headroom uint16, pool uint64) {
	binary.NativeEndian.PutUint64(b[m+offRearm:], rearm(headroom, 1, 1, PortInvalid))
	binary.NativeEndian.PutUint64(b[m+offNext:], 0)
	binary.NativeEndian.PutUint64(b[m+offPool:], pool)
}

// PrepareTx makes an mbuf describe one packet of length bytes starting dataOff
// into its buffer, owned by pool.
//
// Four stores, all in the mbuf's first cache line, which is the line the PMD
// reads. Nothing here touches the packet itself.
func PrepareTx(b []byte, m int, pool uint64, dataOff uint16, length uint32) {
	binary.NativeEndian.PutUint64(b[m+offRearm:], rearm(dataOff, 1, 1, PortInvalid))
	binary.NativeEndian.PutUint64(b[m+offOlFlags:], 0)
	binary.NativeEndian.PutUint32(b[m+offPktLen:], length)
	binary.NativeEndian.PutUint16(b[m+offDataLen:], uint16(length))
	binary.NativeEndian.PutUint64(b[m+offNext:], 0)
	SetPool(b, m, pool)
}

// RxRead is every field the receive path wants from one mbuf, in three wide
// loads on the first cache line instead of four narrow ones: data_off and
// nb_segs share the rearm word, data_len sits beside pkt_len, and ol_flags
// has a word of its own.
func RxRead(b []byte, m int) (dataOff, nbSegs, dataLen uint16, olFlags uint64) {
	rearmWord := binary.NativeEndian.Uint64(b[m+offRearm:])
	lenWord := binary.NativeEndian.Uint64(b[m+offPktLen:])
	olFlags = binary.NativeEndian.Uint64(b[m+offOlFlags:])
	return uint16(rearmWord), uint16(rearmWord >> 32), uint16(lenWord >> 32), olFlags
}

// DataOff is how far into its buffer this mbuf's packet starts, and DataLen how
// many bytes of it are valid.
func DataOff(b []byte, m int) uint16 { return binary.NativeEndian.Uint16(b[m+offDataOff:]) }

// DataLen is how many bytes of the packet are valid in this segment.
func DataLen(b []byte, m int) uint16 { return binary.NativeEndian.Uint16(b[m+offDataLen:]) }

// PktLen is the whole packet's length across every segment.
func PktLen(b []byte, m int) uint32 { return binary.NativeEndian.Uint32(b[m+offPktLen:]) }

// NbSegs is how many mbufs this packet occupies. Anything but one means the
// packet is chained, which this backend does not accept.
func NbSegs(b []byte, m int) uint16 { return binary.NativeEndian.Uint16(b[m+offNbSegs:]) }

// SetNbSegs says how many mbufs this packet occupies. Nothing on the packet
// path writes it -- a driver does, and this backend only reads it -- so this
// exists for the tests that have to produce a chained packet to prove it is
// refused.
func SetNbSegs(b []byte, m int, n uint16) {
	binary.NativeEndian.PutUint16(b[m+offNbSegs:], n)
}

// OlFlags is what the NIC reported about this packet, or what it is being asked
// to do to it.
func OlFlags(b []byte, m int) uint64 { return binary.NativeEndian.Uint64(b[m+offOlFlags:]) }

// SetOlFlags asks the NIC for offload work on a transmitted packet.
func SetOlFlags(b []byte, m int, v uint64) {
	binary.NativeEndian.PutUint64(b[m+offOlFlags:], v)
}

// VLANTCI is the tag the NIC stripped, when it was asked to strip one. This
// backend never asks, so the tag stays in the packet and this stays zero; it is
// here because a caller comparing against another DPDK program will look.
func VLANTCI(b []byte, m int) uint16 { return binary.NativeEndian.Uint16(b[m+offVLANTCI:]) }

// SetTxOffload writes the packed header lengths a NIC needs to compute
// checksums: l2_len in the low 7 bits, l3_len in the next 9.
func SetTxOffload(b []byte, m int, l2Len, l3Len uint64) {
	binary.NativeEndian.PutUint64(b[m+offTxOffload:], l2Len&0x7f|(l3Len&0x1ff)<<7)
}

// TxOffload is the packed header lengths.
func TxOffload(b []byte, m int) uint64 { return binary.NativeEndian.Uint64(b[m+offTxOffload:]) }

// Transmit offload flags, from rte_mbuf_core.h, read out of the installed
// headers on 28 August 2026 and checked by the layout test where DPDK is
// present.
//
// Two of these are easy to get wrong and both are load-bearing. A NIC asked for
// a checksum must also be told which IP version it is looking at, so TxIPv4 or
// TxIPv6 goes alongside every checksum request -- DPDK's own drivers and VPP
// both do this, and a missing version bit is a silently wrong checksum rather
// than an error. And the L4 field is an enumeration in bits 52-53, not a set of
// independent flags: writing UDP over TCP means clearing TxL4Mask first.
const (
	TxIPv4        uint64 = 1 << 55 // RTE_MBUF_F_TX_IPV4
	TxIPv6        uint64 = 1 << 56 // RTE_MBUF_F_TX_IPV6
	TxIPChecksum  uint64 = 1 << 54 // RTE_MBUF_F_TX_IP_CKSUM
	TxTCPChecksum uint64 = 1 << 52 // RTE_MBUF_F_TX_TCP_CKSUM
	TxUDPChecksum uint64 = 3 << 52 // RTE_MBUF_F_TX_UDP_CKSUM
	TxL4Mask      uint64 = 3 << 52 // RTE_MBUF_F_TX_L4_MASK
	TxTCPSeg      uint64 = 1 << 50 // RTE_MBUF_F_TX_TCP_SEG
)

// Receive flags for a checksum the NIC found wrong. A packetio descriptor says
// only that a checksum was verified good, so these are what distinguish "bad"
// from "the NIC did not look", which is the same absence of a Good flag.
const (
	RxIPChecksumBad uint64 = 1 << 4 // RTE_MBUF_F_RX_IP_CKSUM_BAD
	RxL4ChecksumBad uint64 = 1 << 3 // RTE_MBUF_F_RX_L4_CKSUM_BAD
)

// SetTxOffloadTSO writes the packed header lengths and segment size a NIC needs
// to cut a super-frame up: l2_len in bits 0-6, l3_len in 7-15, l4_len in 16-23
// and tso_segsz in 24-39.
func SetTxOffloadTSO(b []byte, m int, l2Len, l3Len, l4Len, segSize uint64) {
	binary.NativeEndian.PutUint64(b[m+offTxOffload:],
		l2Len&0x7f|(l3Len&0x1ff)<<7|(l4Len&0xff)<<16|(segSize&0xffff)<<24)
}

// L4 protocol numbers this package recognises, so a caller need not import
// anything to read a Headers value.
const (
	ProtoTCP = 6
	ProtoUDP = 17

	// The full headers, because a NIC asked for a checksum writes into them:
	// UDP's checksum is at offset 6 and TCP's at 16.
	udpHeaderLen = 8
	tcpHeaderLen = 20
)

// Headers is what Parse found in front of a packet's payload.
type Headers struct {
	// L2Len is the ethernet header including every VLAN tag, and L3Len the IP
	// header. A NIC computing a checksum is told both, and gets it wrong --
	// silently, on the wire -- if a tag is not counted.
	L2Len int
	L3Len int

	// IPv4 and IPv6 say which version was found; both false means neither, and
	// then nothing else here is meaningful.
	IPv4 bool
	IPv6 bool

	// Proto is the L4 protocol, ProtoTCP or ProtoUDP where this package
	// recognised one, and 0 otherwise.
	Proto uint8
}

// Ethertypes, including the tags. QinQ is here because a double-tagged frame is
// ordinary on a provider link and its outer tag carries either ethertype
// depending on who configured the switch.
const (
	etherIPv4  = 0x0800
	etherIPv6  = 0x86dd
	etherVLAN  = 0x8100
	etherQinQ  = 0x88a8
	etherQinQ2 = 0x9100
)

// Parse reads the ethernet and IP headers of a packet to find the lengths a NIC
// needs for a checksum.
//
// It walks VLAN tags rather than assuming a 14-byte ethernet header, because
// this library sends tagged traffic as a matter of course and an untagged
// assumption puts the checksum four bytes into the wrong place -- which the
// sending NIC reports as success and the receiver silently discards. That
// failure has already cost this project a day once, in the M0 spike, from the
// same arithmetic in a different place.
//
// It reads only headers, never trusts a length field to point anywhere, and
// reports ok=false rather than guessing when the packet is too short or is
// something it does not recognise.
func Parse(p []byte) (Headers, bool) {
	var h Headers
	if len(p) < 14 {
		return h, false
	}
	off := 12
	et := int(p[off])<<8 | int(p[off+1])
	off += 2
	// At most two tags: a third is not something this backend claims to know,
	// and an unbounded loop over attacker-supplied bytes is not something it
	// should contain.
	for tags := 0; tags < 2 && (et == etherVLAN || et == etherQinQ || et == etherQinQ2); tags++ {
		if len(p) < off+4 {
			return h, false
		}
		et = int(p[off+2])<<8 | int(p[off+3])
		off += 4
	}
	h.L2Len = off

	switch et {
	case etherIPv4:
		if len(p) < off+20 {
			return h, false
		}
		if p[off]>>4 != 4 {
			return h, false
		}
		ihl := int(p[off]&0x0f) * 4
		if ihl < 20 || len(p) < off+ihl {
			return h, false
		}
		h.IPv4, h.L3Len, h.Proto = true, ihl, p[off+9]
	case etherIPv6:
		// Fixed 40 bytes: an extension header chain would move L4 and this
		// package does not walk one, so a packet carrying one is refused
		// rather than given a checksum offset pointing into the wrong header.
		if len(p) < off+40 || p[off]>>4 != 6 {
			return h, false
		}
		next := p[off+6]
		if next != ProtoTCP && next != ProtoUDP {
			return h, false
		}
		h.IPv6, h.L3Len, h.Proto = true, 40, next
	default:
		return h, false
	}
	if h.Proto != ProtoTCP && h.Proto != ProtoUDP {
		return h, false
	}
	// The whole L4 header has to be present, not just its first four bytes:
	// the NIC writes the checksum at a fixed offset into it -- six bytes in
	// for UDP, sixteen for TCP -- and a packet too short to hold that field
	// has it written past the end of the packet. A forwarder handing received
	// bytes to a device opened with checksum offload can be given such a frame
	// from the wire, so this is not only a caller's mistake to catch.
	need := udpHeaderLen
	if h.Proto == ProtoTCP {
		need = tcpHeaderLen
	}
	return h, len(p) >= h.L2Len+h.L3Len+need
}

// ChecksumFlags is what to put in ol_flags to have a NIC compute the checksums
// for a packet these headers describe. The header lengths go with it in a
// separate call, SetTxOffload: the NIC needs both.
//
// The IP checksum is asked for on IPv4 only: IPv6 has none, and asking for one
// is how a driver ends up writing two bytes over somebody's payload.
func ChecksumFlags(h Headers) uint64 {
	var f uint64
	switch {
	case h.IPv4:
		f = TxIPv4 | TxIPChecksum
	case h.IPv6:
		f = TxIPv6
	default:
		return 0
	}
	switch h.Proto {
	case ProtoTCP:
		f |= TxTCPChecksum
	case ProtoUDP:
		f |= TxUDPChecksum
	}
	return f
}
