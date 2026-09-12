//go:build linux && cgo && dpdk && (amd64 || arm64)

package eal

// What the installed DPDK headers say about the mbuf, so that the hand-copied
// constants in internal/mbuf can be checked against them.
//
// The mbuf package indexes a C struct by hand and must not import cgo -- that
// is the whole point of it, since the packet path may not cross into C. The
// cost of that is a table of offsets and flag values transcribed by a person,
// which is exactly the kind of thing that is right when it is written and wrong
// two DPDK releases later. This file reads the same values from the headers on
// the machine doing the build, and the test beside it compares the two. A
// mismatch is a build-time failure rather than a packet path writing into the
// wrong field.

/*
#cgo CFLAGS: -I/usr/include/dpdk -Wall
#cgo amd64 CFLAGS: -I/usr/include/x86_64-linux-gnu/dpdk
#cgo arm64 CFLAGS: -I/usr/include/aarch64-linux-gnu/dpdk
#include <string.h>
#include <stddef.h>
#include <rte_mbuf.h>
#include <rte_mempool.h>

// The mempool object header, which is cache-line aligned and so differs
// between x86-64 (64) and aarch64 (128).
static int pio_mempool_obj_header(void)
{ return (int)RTE_ALIGN_CEIL(sizeof(struct rte_mempool_objhdr), RTE_MEMPOOL_ALIGN); }

// offsetof, rather than cgo field access: several of these live inside
// anonymous unions, which cgo does not expose as fields.
static int pio_off_buf_addr(void)   { return offsetof(struct rte_mbuf, buf_addr); }
static int pio_off_buf_iova(void)   { return offsetof(struct rte_mbuf, buf_iova); }
static int pio_off_rearm(void)      { return offsetof(struct rte_mbuf, rearm_data); }
static int pio_off_data_off(void)   { return offsetof(struct rte_mbuf, data_off); }
static int pio_off_refcnt(void)     { return offsetof(struct rte_mbuf, refcnt); }
static int pio_off_nb_segs(void)    { return offsetof(struct rte_mbuf, nb_segs); }
static int pio_off_port(void)       { return offsetof(struct rte_mbuf, port); }
static int pio_off_ol_flags(void)   { return offsetof(struct rte_mbuf, ol_flags); }
static int pio_off_pkt_len(void)    { return offsetof(struct rte_mbuf, pkt_len); }
static int pio_off_data_len(void)   { return offsetof(struct rte_mbuf, data_len); }
static int pio_off_vlan_tci(void)   { return offsetof(struct rte_mbuf, vlan_tci); }
static int pio_off_buf_len(void)    { return offsetof(struct rte_mbuf, buf_len); }
static int pio_off_pool(void)       { return offsetof(struct rte_mbuf, pool); }
static int pio_off_next(void)       { return offsetof(struct rte_mbuf, next); }
static int pio_off_tx_offload(void) { return offsetof(struct rte_mbuf, tx_offload); }

static unsigned long long pio_mask_l2_len(void)
	{ struct rte_mbuf m; memset(&m, 0, sizeof(m)); m.l2_len = 0x7f; return m.tx_offload; }
static unsigned long long pio_mask_l3_len(void)
	{ struct rte_mbuf m; memset(&m, 0, sizeof(m)); m.l3_len = 0x1ff; return m.tx_offload; }
static unsigned long long pio_mask_l4_len(void)
	{ struct rte_mbuf m; memset(&m, 0, sizeof(m)); m.l4_len = 0xff; return m.tx_offload; }
static unsigned long long pio_mask_tso(void)
	{ struct rte_mbuf m; memset(&m, 0, sizeof(m)); m.tso_segsz = 0xffff; return m.tx_offload; }
*/
import "C"

// HeaderLayout is what the installed headers say, for the layout test.
type HeaderLayout struct {
	// MbufSize is sizeof(struct rte_mbuf) and Headroom RTE_PKTMBUF_HEADROOM.
	MbufSize int
	Headroom int

	// ObjHeader is what the mempool library puts in front of every object:
	// sizeof(struct rte_mempool_objhdr) rounded up to RTE_MEMPOOL_ALIGN,
	// which is the cache line size. 64 on x86-64, 128 on aarch64.
	ObjHeader int

	// Offsets maps the field names internal/mbuf uses to their offsets here.
	Offsets map[string]int

	// Flags maps the offload flag names to their values here.
	Flags map[string]uint64

	// TxOffloadMasks maps each packed tx_offload field to the word the
	// compiler produces when that field alone is set to all ones.
	TxOffloadMasks map[string]uint64
}

// Headers reads the mbuf layout out of the DPDK headers this build used.
func Headers() HeaderLayout {
	return HeaderLayout{
		MbufSize:  C.sizeof_struct_rte_mbuf,
		Headroom:  C.RTE_PKTMBUF_HEADROOM,
		ObjHeader: int(C.pio_mempool_obj_header()),
		Offsets: map[string]int{
			"buf_addr":   int(C.pio_off_buf_addr()),
			"buf_iova":   int(C.pio_off_buf_iova()),
			"rearm_data": int(C.pio_off_rearm()),
			"data_off":   int(C.pio_off_data_off()),
			"refcnt":     int(C.pio_off_refcnt()),
			"nb_segs":    int(C.pio_off_nb_segs()),
			"port":       int(C.pio_off_port()),
			"ol_flags":   int(C.pio_off_ol_flags()),
			"pkt_len":    int(C.pio_off_pkt_len()),
			"data_len":   int(C.pio_off_data_len()),
			"vlan_tci":   int(C.pio_off_vlan_tci()),
			"buf_len":    int(C.pio_off_buf_len()),
			"pool":       int(C.pio_off_pool()),
			"next":       int(C.pio_off_next()),
			"tx_offload": int(C.pio_off_tx_offload()),
		},
		Flags: map[string]uint64{
			"TxIPv4":           C.RTE_MBUF_F_TX_IPV4,
			"TxIPv6":           C.RTE_MBUF_F_TX_IPV6,
			"TxIPChecksum":     C.RTE_MBUF_F_TX_IP_CKSUM,
			"TxTCPChecksum":    C.RTE_MBUF_F_TX_TCP_CKSUM,
			"TxUDPChecksum":    C.RTE_MBUF_F_TX_UDP_CKSUM,
			"TxL4Mask":         C.RTE_MBUF_F_TX_L4_MASK,
			"TxTCPSeg":         C.RTE_MBUF_F_TX_TCP_SEG,
			"RxIPChecksumGood": C.RTE_MBUF_F_RX_IP_CKSUM_GOOD,
			"RxL4ChecksumGood": C.RTE_MBUF_F_RX_L4_CKSUM_GOOD,
			"RxIPChecksumBad":  C.RTE_MBUF_F_RX_IP_CKSUM_BAD,
			"RxL4ChecksumBad":  C.RTE_MBUF_F_RX_L4_CKSUM_BAD,
		},
		TxOffloadMasks: map[string]uint64{
			"l2_len":    uint64(C.pio_mask_l2_len()),
			"l3_len":    uint64(C.pio_mask_l3_len()),
			"l4_len":    uint64(C.pio_mask_l4_len()),
			"tso_segsz": uint64(C.pio_mask_tso()),
		},
	}
}
