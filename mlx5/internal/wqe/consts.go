// Package wqe encodes and decodes mlx5 work queue entries and completion queue
// entries.
//
// Everything here is pure Go byte manipulation over memory shared with the NIC,
// so it builds and is tested on any platform. Nothing in this package talks to
// a device, orders a memory access or rings a doorbell: publication is the
// caller's job, through package arch.
//
// All multi-byte fields in mlx5 descriptors are big-endian regardless of host
// byte order.
//
// The layouts follow rdma-core's infiniband/mlx5dv.h (struct mlx5_wqe_ctrl_seg,
// mlx5_wqe_eth_seg, mlx5_wqe_data_seg, mlx5_cqe64 and the mlx5dv_set_*_seg
// helpers), cross-checked against DPDK's mlx5 poll-mode driver.
package wqe

// Sizes, in bytes, of the pieces a work queue entry is built from.
const (
	// WQEBB is a work queue buffer block, the unit the send queue is measured
	// and indexed in.
	WQEBB = 64

	// Octoword is the unit the ds field of a control segment counts.
	Octoword = 16

	// CtrlSegSize is the fixed size of a control segment.
	CtrlSegSize = 16

	// EthSegHeaderSize is the part of an Ethernet segment that precedes the
	// inline packet header: reserved(4) csFlags(1) reserved(1) mss(2)
	// reserved(4) inlineHdrSz(2). Inline header bytes start here and the
	// segment is padded to a whole number of octowords.
	EthSegHeaderSize = 14

	// DataSegSize is the fixed size of a data segment: byteCount(4) lkey(4)
	// addr(8).
	DataSegSize = 16

	// CQESize is the completion queue entry size this package decodes. A
	// device may be configured for 128-byte entries, in which case the 64-byte
	// entry sits in the second half; callers must not use that configuration
	// without adjusting for it.
	CQESize = 64
)

// EthL2InlineHeaderSize is the inline header length an mlx5 device asks for
// when its vport is in L2 inline mode: an Ethernet header plus one VLAN tag,
// 14 + 4 = 18 bytes. It is the longest single-tagged L2 header, which is why
// the hardware picked it, and it is what rdma-core inlines
// unconditionally.
//
// It is not a property of the packet. A device in NONE inline mode needs no
// inline header at all, and a QinQ frame in L2 mode needs 22. Never assume a
// frame's Ethernet header is 18 bytes; ask the device what it requires and size
// the segment from that.
const EthL2InlineHeaderSize = 18

// MaxDS is the largest ds value a work queue entry may carry. The field is 6
// bits, but the hardware limit is 60 octowords, which is what DPDK
// (MLX5_WQE_SIZE_MAX) uses.
const MaxDS = 60

// Work queue entry opcodes, from the mlx5 programmer's reference manual.
const (
	OpcodeNOP  = 0x00
	OpcodeSend = 0x0a
	OpcodeTSO  = 0x0e

	// OpcodeEnhancedMPSW builds one work queue entry that carries several
	// packets. rdma-core does not export it; the value is the one DPDK
	// (MLX5_OPCODE_ENHANCED_MPSW) uses. Do
	// not emit it unless the device reports the enhanced-MPW capability.
	OpcodeEnhancedMPSW = 0x29
)

// Control segment fm_ce_se bits.
const (
	// CtrlCQUpdate asks for a completion queue entry when this work queue
	// entry finishes. Without it the entry completes silently and is accounted
	// for by a later entry's completion.
	CtrlCQUpdate = 2 << 2

	CtrlSolicited = 1 << 1
	CtrlFence     = 4 << 5
)

// Ethernet segment cs_flags bits, asking the NIC to compute checksums.
const (
	EthWQEL3Csum = 1 << 6
	EthWQEL4Csum = 1 << 7
)

// InlineSegFlag marks a data segment's byte count as carrying inline packet
// bytes rather than an address.
const InlineSegFlag = 0x80000000

// Doorbell record indices. A queue's doorbell record is two 32-bit words in
// host memory that the NIC reads; the receive queue publishes its producer
// index in the first and the send queue in the second. A completion queue has
// its own record whose first word is the consumer index.
const (
	RcvDBR = 0
	SndDBR = 1
	CQDBR  = 0
)

// Completion queue entry opcodes, the high nibble of the last byte.
const (
	CQEReq         = 0  // a send completed
	CQERespWrImm   = 1  //
	CQERespSend    = 2  // a packet was received
	CQERespSendImm = 3  //
	CQERespSendInv = 4  //
	CQEResizeCQ    = 5  //
	CQENoPacket    = 6  //
	CQESigErr      = 12 //
	CQEReqErr      = 13 // a send failed
	CQERespErr     = 14 // a receive failed
	CQEInvalid     = 15 // nothing here yet
)

// Completion queue entry format, bits 3:2 of the last byte. Anything other than
// CQEFormatNoData means the entry carries or stands for more than one result
// and must not be decoded as a plain entry.
const (
	CQEFormatNoData     = 0
	CQEFormatInline32   = 1
	CQEFormatInline64   = 2
	CQEFormatCompressed = 3
)

// Receive checksum results, in the hds_ip_ext byte of a completion (offset 28).
const (
	CQEL2OK = 1 << 0
	CQEL3OK = 1 << 1
	CQEL4OK = 1 << 2
)

// Parsed L3 header type, bits 3:2 of the l4_hdr_type_etc byte (offset 29).
const (
	CQEL3HdrTypeNone = 0
	CQEL3HdrTypeIPv6 = 1
	CQEL3HdrTypeIPv4 = 2
)

// An enhanced multi-packet entry carries several packets. Each one is either
// pointed at by a data segment or copied in behind a four-byte length, and the
// entry as a whole has one compact Ethernet segment instead of one each.
//
// This is what lets a device transmit small packets at line rate. A packet
// described by a pointer costs the NIC two fetches across the bus, the
// descriptor and the packet; copied in, it costs one, and packing several into
// one entry keeps that one fetch small.
const (
	// MPWEthSegSize is the Ethernet segment of a multi-packet entry: the same
	// fields as an ordinary one, without room for an inline header.
	MPWEthSegSize = 16

	// MPWInlineFlag marks a segment's length as introducing copied packet
	// bytes rather than an address.
	MPWInlineFlag = 1 << 31
)
