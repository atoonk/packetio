//go:build linux && cgo && mlx5 && (amd64 || arm64)

package dv

/*
#include <stddef.h>
#include <infiniband/verbs.h>
#include <infiniband/mlx5dv.h>

static int off_ctrl_fm_ce_se(void)     { return offsetof(struct mlx5_wqe_ctrl_seg, fm_ce_se); }
static int off_ctrl_imm(void)          { return offsetof(struct mlx5_wqe_ctrl_seg, imm); }
static int off_eth_cs_flags(void)      { return offsetof(struct mlx5_wqe_eth_seg, cs_flags); }
static int off_eth_mss(void)           { return offsetof(struct mlx5_wqe_eth_seg, mss); }
static int off_eth_inline_hdr_sz(void) { return offsetof(struct mlx5_wqe_eth_seg, inline_hdr_sz); }
static int off_eth_inline_hdr(void)    { return offsetof(struct mlx5_wqe_eth_seg, inline_hdr_start); }
static int off_cqe_byte_cnt(void)      { return offsetof(struct mlx5_cqe64, byte_cnt); }
static int off_cqe_wqe_counter(void)   { return offsetof(struct mlx5_cqe64, wqe_counter); }
static int off_cqe_op_own(void)        { return offsetof(struct mlx5_cqe64, op_own); }
static int off_cqe_hds_ip_ext(void)    { return offsetof(struct mlx5_cqe64, hds_ip_ext); }
static int off_cqe_hdr_types(void)     { return offsetof(struct mlx5_cqe64, l4_hdr_type_etc); }
static int off_cqe_timestamp(void)     { return offsetof(struct mlx5_cqe64, timestamp); }
static int off_cqe_hash(void)          { return offsetof(struct mlx5_cqe64, flags_rqpn); }
static int off_cqe_vlan(void)          { return offsetof(struct mlx5_cqe64, vlan_info); }
static int off_err_syndrome(void)      { return offsetof(struct mlx5_err_cqe, syndrome); }
static int off_err_vendor(void)        { return offsetof(struct mlx5_err_cqe, vendor_err_synd); }
static int off_err_qpn(void)           { return offsetof(struct mlx5_err_cqe, s_wqe_opcode_qpn); }
*/
import "C"

// HeaderLayout is where the device's own library says the fields of a work
// queue entry and a completion are.
//
// The packet path writes and reads those fields by offset with no C involved,
// so this is what connects the two. A test compares it against the offsets that
// path uses; a build against a future rdma-core that moved something fails
// there rather than on the wire.
type HeaderLayout struct {
	CtrlSegSize    int
	CtrlFMCESE     int
	CtrlImm        int
	EthSegSize     int
	EthCSFlags     int
	EthMSS         int
	EthInlineHdrSz int
	EthInlineHdr   int
	DataSegSize    int

	CQESize       int
	CQEByteCount  int
	CQEWQECounter int
	CQEOpOwn      int
	CQEChecksum   int
	CQEHdrTypes   int
	CQETimestamp  int
	CQEHash       int
	CQEVLAN       int

	ErrSyndrome       int
	ErrVendorSyndrome int
	ErrQPN            int

	WQEBB         int
	OpcodeSend    int
	CtrlCQUpdate  int
	SndDBR        int
	RcvDBR        int
	InlineSegFlag uint32
	EthWQEL3Csum  int
	EthWQEL4Csum  int
	CQEReq        int
	CQERespSend   int
	CQEReqErr     int
	CQERespErr    int
	CQEInvalid    int
	SyndromeProt  int
	SyndromeFlush int
}

// Headers reports the layout the installed rdma-core headers describe.
func Headers() HeaderLayout {
	return HeaderLayout{
		CtrlSegSize:    C.sizeof_struct_mlx5_wqe_ctrl_seg,
		CtrlFMCESE:     int(C.off_ctrl_fm_ce_se()),
		CtrlImm:        int(C.off_ctrl_imm()),
		EthSegSize:     C.sizeof_struct_mlx5_wqe_eth_seg,
		EthCSFlags:     int(C.off_eth_cs_flags()),
		EthMSS:         int(C.off_eth_mss()),
		EthInlineHdrSz: int(C.off_eth_inline_hdr_sz()),
		EthInlineHdr:   int(C.off_eth_inline_hdr()),
		DataSegSize:    C.sizeof_struct_mlx5_wqe_data_seg,

		CQESize:       C.sizeof_struct_mlx5_cqe64,
		CQEByteCount:  int(C.off_cqe_byte_cnt()),
		CQEWQECounter: int(C.off_cqe_wqe_counter()),
		CQEOpOwn:      int(C.off_cqe_op_own()),
		CQEChecksum:   int(C.off_cqe_hds_ip_ext()),
		CQEHdrTypes:   int(C.off_cqe_hdr_types()),
		CQETimestamp:  int(C.off_cqe_timestamp()),
		CQEHash:       int(C.off_cqe_hash()),
		CQEVLAN:       int(C.off_cqe_vlan()),

		ErrSyndrome:       int(C.off_err_syndrome()),
		ErrVendorSyndrome: int(C.off_err_vendor()),
		ErrQPN:            int(C.off_err_qpn()),

		WQEBB:         C.MLX5_SEND_WQE_BB,
		OpcodeSend:    C.MLX5_OPCODE_SEND,
		CtrlCQUpdate:  C.MLX5_WQE_CTRL_CQ_UPDATE,
		SndDBR:        C.MLX5_SND_DBR,
		RcvDBR:        C.MLX5_RCV_DBR,
		InlineSegFlag: uint32(C.MLX5_INLINE_SEG),
		EthWQEL3Csum:  C.MLX5_ETH_WQE_L3_CSUM,
		EthWQEL4Csum:  C.MLX5_ETH_WQE_L4_CSUM,
		CQEReq:        C.MLX5_CQE_REQ,
		CQERespSend:   C.MLX5_CQE_RESP_SEND,
		CQEReqErr:     C.MLX5_CQE_REQ_ERR,
		CQERespErr:    C.MLX5_CQE_RESP_ERR,
		CQEInvalid:    C.MLX5_CQE_INVALID,
		SyndromeProt:  C.MLX5_CQE_SYNDROME_LOCAL_PROT_ERR,
		SyndromeFlush: C.MLX5_CQE_SYNDROME_WR_FLUSH_ERR,
	}
}
