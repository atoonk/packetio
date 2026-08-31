/* Bring-up for an mlx5 Direct Verbs packet queue.
 *
 * This is the only C in packetio, and it runs only when a device is opened or
 * closed. Its whole job is to persuade libibverbs and libmlx5 to create a raw
 * Ethernet queue pair and then to hand back the addresses of the memory they
 * allocated for it: the work queue, the completion queue, the doorbell records
 * and the device register. From that point on the packet path is Go.
 *
 * Every function returns 0 on success, or -1 with a message in err.
 */

#ifndef PACKETIO_MLX5_SHIM_H
#define PACKETIO_MLX5_SHIM_H

#include <stddef.h>
#include <stdint.h>

typedef struct pio_ctx pio_ctx;

/* What the device can do. Filled in by pio_open. */
struct pio_caps {
	char     ibdev[64];    /* the verbs device name, such as mlx5_0 */
	char     fw[64];       /* firmware version */
	uint32_t port;         /* the port this netdev belongs to */
	uint32_t port_state;   /* 4 is IBV_PORT_ACTIVE */
	uint32_t link_layer;   /* 2 is IBV_LINK_LAYER_ETHERNET */
	uint32_t max_qp_wr;    /* longest queue the device will create */
	uint32_t max_sge;      /* data segments per work request */
	uint32_t max_cqe;      /* longest completion queue */
	uint64_t dv_flags;     /* mlx5dv_context.flags: CQE_V1, ENHANCED_MPW and so on */
	uint32_t vendor_id;
	uint32_t vendor_part_id;
};

/* A transmit queue, as the hardware and the Go packet path see it.
 *
 * The buffers and records here belong to libmlx5 and stay valid until
 * pio_destroy_txq. Go writes work queue entries into sq_buf, reads completions
 * out of cq_buf, publishes indices to the doorbell records and rings the
 * doorbell at uar.
 */
struct pio_txq {
	void     *qp;
	void     *cq;

	void     *sq_buf;      /* work queue entries */
	uint32_t  sq_wqe_cnt;  /* how many 64-byte blocks it holds */
	uint32_t  sq_stride;   /* bytes per block; 64 on every device that matters */

	void     *cq_buf;      /* completion queue entries */
	uint32_t  cq_cqe_cnt;
	uint32_t  cq_cqe_size;

	uint32_t *sq_dbrec;    /* the send half of the queue pair's doorbell record */
	uint32_t *cq_dbrec;    /* the completion queue's doorbell record */

	void     *uar;         /* the device register a doorbell is written to */
	uint32_t  bf_size;     /* zero means the register is not a BlueFlame buffer */

	uint32_t  qpn;
	uint32_t  cqn;

};

/* pio_open finds the named verbs device, checks it is an mlx5 with an Ethernet
 * port, opens it and allocates a protection domain. */
int  pio_open(const char *ibdev, uint32_t port, pio_ctx **out,
	      struct pio_caps *caps, char *err, size_t errlen);
void pio_dv_close(pio_ctx *c);

/* pio_reg_mr pins a region of memory and registers it with the device, so the
 * NIC may read packets out of it and write packets into it. The key it returns
 * goes in every work queue entry that points at the region. */
int  pio_reg_mr(pio_ctx *c, void *addr, size_t length, uint32_t *lkey,
		char *err, size_t errlen);

/* pio_min_inline reports how many bytes of each packet's Ethernet header the
 * device insists on having inside the work queue entry: zero, or eighteen.
 *
 * There is no way to ask for this directly, so it is measured: let the mlx5
 * provider build one ordinary send, read the inline length out of the entry it
 * wrote, and abandon it before anything is transmitted. sample must be an
 * address inside the registered region. */
int  pio_min_inline(pio_ctx *c, void *sample, uint32_t *out,
		    char *err, size_t errlen);

/* pio_create_txq creates a raw Ethernet queue pair with room for depth work
 * requests, drives it to the ready-to-send state, and exposes its memory. */
int  pio_create_txq(pio_ctx *c, uint32_t depth, struct pio_txq *q,
		    char *err, size_t errlen);
int  pio_destroy_txq(struct pio_txq *q, char *err, size_t errlen);

/* A receive queue, as the hardware and the Go packet path see it.
 *
 * There is more machinery behind this than behind a transmit queue, because a
 * NIC has to be told which packets to put here. A work queue holds the buffers,
 * an indirection table and a queue pair name it as a destination, and a flow
 * rule says what goes to that destination. Everything else still reaches the
 * kernel, which owns the port.
 */
struct pio_rxq {
	void     *wq;
	void     *cq;

	void     *rq_buf;      /* receive work queue entries */
	uint32_t  rq_wqe_cnt;  /* how many it holds */
	uint32_t  rq_stride;   /* bytes per entry */

	void     *cq_buf;
	uint32_t  cq_cqe_cnt;
	uint32_t  cq_cqe_size;

	uint32_t *rq_dbrec;    /* where the number of posted buffers is published */
	uint32_t *cq_dbrec;

	uint32_t  cqn;

};


/* PIO_MAX_FLOWS bounds how many alternatives one filter may have. Each is a
 * separate rule in the card, and a filter needing more than this is better
 * expressed as a wider match than as a long list. */
#define PIO_MAX_FLOWS 16

/* A group of receive queues and the machinery that sends packets to them.
 *
 * A flow rule cannot name a work queue; it names a queue pair, which reaches
 * the queues through an indirection table. One rule and one table over several
 * queues is what spreads a flow of traffic across them by hash, which is the
 * only way several queues can share one kind of traffic: two rules matching the
 * same packets would not divide them, since a packet takes the first rule that
 * matches it. */
struct pio_rx_group {
	void *ind_table;
	void *qp;
	/* A flow rule is a single conjunction, so a filter that is a union (two
	 * ports, say) needs one rule per alternative. They all name this group. */
	void *flows[PIO_MAX_FLOWS];
	uint32_t nflows;
};

/* pio_create_rxq creates a receive queue able to hold depth buffers and brings
 * it to the ready state. It receives nothing until it is part of a group with
 * a steering rule. */
int  pio_create_rxq(pio_ctx *c, uint32_t depth, struct pio_rxq *q,
		    char *err, size_t errlen);
int  pio_destroy_rxq(struct pio_rxq *q, char *err, size_t errlen);

/* pio_create_rx_group makes n queues a destination that a rule can name.
 * Packets are spread across them by a hash of the addresses and ports. */
int  pio_create_rx_group(pio_ctx *c, struct pio_rxq *qs, uint32_t n,
			 struct pio_rx_group *g, char *err, size_t errlen);

/* pio_match is what a steering rule matches on. A zero-valued field with its
 * "have" flag clear is not matched, so the zero struct with promisc set takes
 * everything the port sees.
 *
 * The card matches these in hardware, at no cost per packet, and anything a
 * rule does not match still goes to the kernel -- which is what keeps the
 * interface usable while packets are being taken from it. */
struct pio_match {
	uint8_t  dst_mac[6];
	int      have_mac;

	int      vlan;		/* VLAN id, or -1 for any */

	uint16_t ether_type;	/* host order, e.g. 0x0800 */
	int      have_ether_type;

	uint32_t src_ip, dst_ip;	/* network order */
	uint32_t src_mask, dst_mask;	/* network order; 0 means not matched */

	uint8_t  ip_proto;	/* IPPROTO_UDP, IPPROTO_TCP */
	int      have_proto;

	uint16_t src_port, dst_port;	/* host order */
	int      have_src_port, have_dst_port;

	int      promisc;	/* take everything, ignoring dst_mac */
};

/* pio_group_steer tells the NIC which packets to put in this group, replacing
 * any rule already in place.
 *
 * The specifications are laid out in ascending protocol order, which is what
 * the verbs API requires: Ethernet, then IPv4, then TCP or UDP. Asking to
 * match a port without a protocol, or an address without an Ethernet
 * specification, is refused rather than silently widened. */
int  pio_group_steer(pio_ctx *c, struct pio_rx_group *g,
		     const struct pio_match *m, uint32_t n, char *err, size_t errlen);

int  pio_destroy_rx_group(struct pio_rx_group *g, char *err, size_t errlen);

#endif /* PACKETIO_MLX5_SHIM_H */
