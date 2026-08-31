#include "shim.h"

/* Room for a header and the three specifications a rule may carry. */
#define PIO_FLOW_BUF (sizeof(struct ibv_flow_attr) + \
		      sizeof(struct ibv_flow_spec_eth) + \
		      sizeof(struct ibv_flow_spec_ipv4_ext) + \
		      sizeof(struct ibv_flow_spec_tcp_udp))

#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <stddef.h>
#include <string.h>
#include <stdarg.h>

#include <infiniband/verbs.h>
#include <infiniband/mlx5dv.h>
#include <endian.h>

struct pio_ctx {
	struct ibv_context *ctx;
	struct ibv_pd      *pd;
	struct ibv_mr      *mr;
	uint32_t            port;
};

static int fail(char *err, size_t errlen, const char *what)
{
	if (err && errlen)
		snprintf(err, errlen, "%s: %s", what, strerror(errno));
	return -1;
}

static int failf(char *err, size_t errlen, const char *fmt, ...)
{
	va_list ap;
	if (err && errlen) {
		va_start(ap, fmt);
		vsnprintf(err, errlen, fmt, ap);
		va_end(ap);
	}
	return -1;
}

int pio_open(const char *ibdev, uint32_t port, pio_ctx **out,
	     struct pio_caps *caps, char *err, size_t errlen)
{
	struct ibv_device **list;
	struct ibv_device  *dev = NULL;
	struct ibv_context *ctx = NULL;
	struct ibv_device_attr_ex attr;
	struct ibv_port_attr pattr;
	struct mlx5dv_context dv;
	pio_ctx *c;
	int n, i, rc;

	*out = NULL;
	memset(caps, 0, sizeof(*caps));

	list = ibv_get_device_list(&n);
	if (!list)
		return fail(err, errlen, "ibv_get_device_list");
	for (i = 0; i < n; i++) {
		if (strcmp(ibv_get_device_name(list[i]), ibdev) == 0) {
			dev = list[i];
			break;
		}
	}
	if (!dev) {
		ibv_free_device_list(list);
		return failf(err, errlen, "no verbs device named %s", ibdev);
	}
	if (!mlx5dv_is_supported(dev)) {
		ibv_free_device_list(list);
		return failf(err, errlen, "%s is not an mlx5 device", ibdev);
	}

	ctx = ibv_open_device(dev);
	/* The list may be freed once the device is open; the context keeps what
	 * it needs. */
	ibv_free_device_list(list);
	if (!ctx)
		return fail(err, errlen, "ibv_open_device");

	memset(&attr, 0, sizeof(attr));
	rc = ibv_query_device_ex(ctx, NULL, &attr);
	if (rc) {
		errno = rc;
		ibv_close_device(ctx);
		return fail(err, errlen, "ibv_query_device_ex");
	}

	memset(&pattr, 0, sizeof(pattr));
	rc = ibv_query_port(ctx, (uint8_t)port, &pattr);
	if (rc) {
		errno = rc;
		ibv_close_device(ctx);
		return fail(err, errlen, "ibv_query_port");
	}
	if (pattr.link_layer != IBV_LINK_LAYER_ETHERNET) {
		ibv_close_device(ctx);
		return failf(err, errlen, "%s port %u is not Ethernet", ibdev, port);
	}

	/* Direct Verbs decoding here assumes the first completion queue entry
	 * format, which every device since ConnectX-4 reports. */
	memset(&dv, 0, sizeof(dv));
	rc = mlx5dv_query_device(ctx, &dv);
	if (rc) {
		errno = rc;
		ibv_close_device(ctx);
		return fail(err, errlen, "mlx5dv_query_device");
	}
	if (!(dv.flags & MLX5DV_CONTEXT_FLAGS_CQE_V1)) {
		ibv_close_device(ctx);
		return failf(err, errlen, "%s does not report version 1 completion entries", ibdev);
	}

	c = calloc(1, sizeof(*c));
	if (!c) {
		ibv_close_device(ctx);
		return failf(err, errlen, "out of memory");
	}
	c->ctx = ctx;
	c->port = port;

	c->pd = ibv_alloc_pd(ctx);
	if (!c->pd) {
		int e = errno;
		free(c);
		ibv_close_device(ctx);
		errno = e;
		return fail(err, errlen, "ibv_alloc_pd");
	}

	snprintf(caps->ibdev, sizeof(caps->ibdev), "%s", ibdev);
	snprintf(caps->fw, sizeof(caps->fw), "%s", attr.orig_attr.fw_ver);
	caps->port = port;
	caps->port_state = pattr.state;
	caps->link_layer = pattr.link_layer;
	caps->max_qp_wr = attr.orig_attr.max_qp_wr;
	caps->max_sge = attr.orig_attr.max_sge;
	caps->max_cqe = attr.orig_attr.max_cqe;
	caps->dv_flags = dv.flags;
	caps->vendor_id = attr.orig_attr.vendor_id;
	caps->vendor_part_id = attr.orig_attr.vendor_part_id;

	*out = c;
	return 0;
}

void pio_dv_close(pio_ctx *c)
{
	if (!c)
		return;
	if (c->mr)
		ibv_dereg_mr(c->mr);
	if (c->pd)
		ibv_dealloc_pd(c->pd);
	if (c->ctx)
		ibv_close_device(c->ctx);
	free(c);
}

int pio_reg_mr(pio_ctx *c, void *addr, size_t length, uint32_t *lkey,
	       char *err, size_t errlen)
{
	if (c->mr)
		return failf(err, errlen, "a region is already registered");

	/* The NIC reads packets to transmit and writes packets it receives, so
	 * local write access is needed even for a transmit-only queue. */
	c->mr = ibv_reg_mr(c->pd, addr, length, IBV_ACCESS_LOCAL_WRITE);
	if (!c->mr)
		return fail(err, errlen, "ibv_reg_mr");
	*lkey = c->mr->lkey;
	return 0;
}

int pio_min_inline(pio_ctx *c, void *sample, uint32_t *out,
		   char *err, size_t errlen)
{
	struct ibv_qp_init_attr_ex qpia;
	struct mlx5dv_obj obj;
	struct mlx5dv_qp dvqp;
	struct ibv_qp_ex *qpx;
	struct ibv_cq *cq = NULL;
	struct ibv_qp *qp = NULL;
	const uint8_t *eseg;
	int rc = 0;

	if (!c->mr)
		return failf(err, errlen, "no region is registered to probe with");

	cq = ibv_create_cq(c->ctx, 1, NULL, NULL, 0);
	if (!cq)
		return fail(err, errlen, "ibv_create_cq for the inline probe");

	memset(&qpia, 0, sizeof(qpia));
	qpia.qp_type = IBV_QPT_RAW_PACKET;
	qpia.send_cq = cq;
	qpia.recv_cq = cq;
	qpia.pd = c->pd;
	qpia.cap.max_send_wr = 1;
	qpia.cap.max_send_sge = 1;
	qpia.comp_mask = IBV_QP_INIT_ATTR_PD | IBV_QP_INIT_ATTR_SEND_OPS_FLAGS;
	qpia.send_ops_flags = IBV_QP_EX_WITH_SEND;

	qp = ibv_create_qp_ex(c->ctx, &qpia);
	if (!qp) {
		rc = fail(err, errlen, "ibv_create_qp_ex for the inline probe");
		goto done;
	}
	qpx = ibv_qp_to_qp_ex(qp);
	if (!qpx) {
		rc = failf(err, errlen, "the provider offers no extended send operations");
		goto done;
	}

	memset(&obj, 0, sizeof(obj));
	memset(&dvqp, 0, sizeof(dvqp));
	obj.qp.in = qp;
	obj.qp.out = &dvqp;
	rc = mlx5dv_init_obj(&obj, MLX5DV_OBJ_QP);
	if (rc || !dvqp.sq.buf) {
		errno = rc ? rc : EINVAL;
		rc = fail(err, errlen, "mlx5dv_init_obj for the inline probe");
		goto done;
	}

	/* Let the provider lay out one ordinary send, then abandon it. Nothing is
	 * published and no doorbell is rung, so nothing is transmitted; all that
	 * is wanted is the inline length it chose, which is the one the kernel
	 * told it this port requires. */
	ibv_wr_start(qpx);
	ibv_wr_send(qpx);
	ibv_wr_set_sge(qpx, c->mr->lkey, (uintptr_t)sample, 18);
	eseg = (const uint8_t *)dvqp.sq.buf + 16;   /* past the control segment */
	*out = ((uint32_t)eseg[12] << 8) | eseg[13]; /* inline_hdr_sz, big-endian */
	ibv_wr_abort(qpx);
	rc = 0;

done:
	if (qp)
		ibv_destroy_qp(qp);
	if (cq)
		ibv_destroy_cq(cq);
	return rc;
}

int pio_create_txq(pio_ctx *c, uint32_t depth, struct pio_txq *q,
		   char *err, size_t errlen)
{
	struct ibv_cq_init_attr_ex cqa;
	struct mlx5dv_cq_init_attr dvcqa;
	struct ibv_cq_ex *cqex;
	struct ibv_qp_init_attr qpia;
	struct ibv_qp_attr qpa;
	struct mlx5dv_obj obj;
	struct mlx5dv_qp dvqp;
	struct mlx5dv_cq dvcq;
	int rc;

	memset(q, 0, sizeof(*q));

	/* A completion queue with entries this code knows how to read: 64 bytes,
	 * uncompressed. Compression is a receive-side optimisation and would need
	 * a different decoder. */
	memset(&cqa, 0, sizeof(cqa));
	cqa.cqe = depth;
	memset(&dvcqa, 0, sizeof(dvcqa));
	dvcqa.comp_mask = MLX5DV_CQ_INIT_ATTR_MASK_CQE_SIZE;
	dvcqa.cqe_size = 64;
	cqex = mlx5dv_create_cq(c->ctx, &cqa, &dvcqa);
	if (!cqex)
		return fail(err, errlen, "mlx5dv_create_cq");
	q->cq = ibv_cq_ex_to_cq(cqex);

	/* A raw packet queue pair: no transport, no addressing, just Ethernet
	 * frames. One data segment per packet, because a frame is one buffer. */
	memset(&qpia, 0, sizeof(qpia));
	qpia.qp_type = IBV_QPT_RAW_PACKET;
	qpia.send_cq = q->cq;
	qpia.recv_cq = q->cq;
	qpia.cap.max_send_wr = depth;
	qpia.cap.max_send_sge = 1;
	q->qp = ibv_create_qp(c->pd, &qpia);
	if (!q->qp) {
		rc = fail(err, errlen, "ibv_create_qp");
		goto fail_cq;
	}

	/* Reset to ready-to-send. A raw packet queue pair needs no path or packet
	 * sequence attributes, so each step carries only what it must. */
	memset(&qpa, 0, sizeof(qpa));
	qpa.qp_state = IBV_QPS_INIT;
	qpa.port_num = (uint8_t)c->port;
	rc = ibv_modify_qp(q->qp, &qpa, IBV_QP_STATE | IBV_QP_PORT);
	if (rc) {
		errno = rc;
		rc = fail(err, errlen, "ibv_modify_qp to init");
		goto fail_qp;
	}
	memset(&qpa, 0, sizeof(qpa));
	qpa.qp_state = IBV_QPS_RTR;
	rc = ibv_modify_qp(q->qp, &qpa, IBV_QP_STATE);
	if (rc) {
		errno = rc;
		rc = fail(err, errlen, "ibv_modify_qp to ready-to-receive");
		goto fail_qp;
	}
	memset(&qpa, 0, sizeof(qpa));
	qpa.qp_state = IBV_QPS_RTS;
	rc = ibv_modify_qp(q->qp, &qpa, IBV_QP_STATE);
	if (rc) {
		errno = rc;
		rc = fail(err, errlen, "ibv_modify_qp to ready-to-send");
		goto fail_qp;
	}

	/* Now ask where all of that actually lives. Asking about the completion
	 * queue hands its consumer index over to this code for good: nothing may
	 * call ibv_poll_cq on it afterwards. */
	memset(&obj, 0, sizeof(obj));
	memset(&dvqp, 0, sizeof(dvqp));
	memset(&dvcq, 0, sizeof(dvcq));
	obj.qp.in = q->qp;
	obj.qp.out = &dvqp;
	obj.cq.in = q->cq;
	obj.cq.out = &dvcq;
	rc = mlx5dv_init_obj(&obj, MLX5DV_OBJ_QP | MLX5DV_OBJ_CQ);
	if (rc) {
		errno = rc;
		rc = fail(err, errlen, "mlx5dv_init_obj");
		goto fail_qp;
	}

	q->sq_buf = dvqp.sq.buf;
	q->sq_wqe_cnt = dvqp.sq.wqe_cnt;
	q->sq_stride = dvqp.sq.stride;
	q->sq_dbrec = &dvqp.dbrec[1];   /* MLX5_SND_DBR */
	q->uar = dvqp.bf.reg;
	q->bf_size = dvqp.bf.size;
	q->qpn = q->qp ? ((struct ibv_qp *)q->qp)->qp_num : 0;

	q->cq_buf = dvcq.buf;
	q->cq_cqe_cnt = dvcq.cqe_cnt;
	q->cq_cqe_size = dvcq.cqe_size;
	q->cq_dbrec = &dvcq.dbrec[0];
	q->cqn = dvcq.cqn;

	if (!q->sq_buf || !q->cq_buf || !q->sq_dbrec || !q->cq_dbrec || !q->uar) {
		rc = failf(err, errlen, "the provider exposed an incomplete queue");
		goto fail_qp;
	}
	return 0;

fail_qp:
	ibv_destroy_qp(q->qp);
	q->qp = NULL;
fail_cq:
	ibv_destroy_cq(q->cq);
	q->cq = NULL;
	return rc;
}

int pio_destroy_txq(struct pio_txq *q, char *err, size_t errlen)
{
	int rc = 0, e;

	if (q->qp) {
		e = ibv_destroy_qp(q->qp);
		if (e && !rc) {
			errno = e;
			rc = fail(err, errlen, "ibv_destroy_qp");
		}
		q->qp = NULL;
	}
	if (q->cq) {
		e = ibv_destroy_cq(q->cq);
		if (e && !rc) {
			errno = e;
			rc = fail(err, errlen, "ibv_destroy_cq");
		}
		q->cq = NULL;
	}
	return rc;
}

/* The hash key mlx5 devices are conventionally given. Its value only matters
 * when a queue is one of several sharing an indirection table; with one queue
 * every packet lands in the same place whatever the hash. */
static const uint8_t pio_rss_key[40] = {
	0x2c, 0xc6, 0x81, 0xd1, 0x5b, 0xdb, 0xf4, 0xf7, 0xfc, 0xa2,
	0x83, 0x19, 0xdb, 0x1a, 0x3e, 0x94, 0x6b, 0x9e, 0x38, 0xd9,
	0x2c, 0x9c, 0x03, 0xd1, 0xad, 0x99, 0x44, 0xa7, 0xd9, 0x56,
	0x3d, 0x59, 0x06, 0x3c, 0x25, 0xf3, 0xfc, 0x1f, 0xdc, 0x2a,
};

int pio_create_rxq(pio_ctx *c, uint32_t depth, struct pio_rxq *q,
		   char *err, size_t errlen)
{
	struct ibv_cq_init_attr_ex cqa;
	struct mlx5dv_cq_init_attr dvcqa;
	struct ibv_cq_ex *cqex;
	struct ibv_wq_init_attr wqia;
	struct ibv_wq_attr wqa;
	struct mlx5dv_obj obj;
	struct mlx5dv_rwq dvrwq;
	struct mlx5dv_cq dvcq;
	int rc;

	memset(q, 0, sizeof(*q));

	memset(&cqa, 0, sizeof(cqa));
	cqa.cqe = depth;
	memset(&dvcqa, 0, sizeof(dvcqa));
	/* Sixty-four byte entries, uncompressed: what the Go decoder reads.
	 * Compression would pack several results into one entry and needs a
	 * different decoder. */
	dvcqa.comp_mask = MLX5DV_CQ_INIT_ATTR_MASK_CQE_SIZE;
	dvcqa.cqe_size = 64;
	cqex = mlx5dv_create_cq(c->ctx, &cqa, &dvcqa);
	if (!cqex)
		return fail(err, errlen, "mlx5dv_create_cq for receive");
	q->cq = ibv_cq_ex_to_cq(cqex);

	/* One buffer per entry: a packet is one frame. */
	memset(&wqia, 0, sizeof(wqia));
	wqia.wq_type = IBV_WQT_RQ;
	wqia.max_wr = depth;
	wqia.max_sge = 1;
	wqia.pd = c->pd;
	wqia.cq = q->cq;
	q->wq = ibv_create_wq(c->ctx, &wqia);
	if (!q->wq) {
		rc = fail(err, errlen, "ibv_create_wq");
		goto fail_cq;
	}

	memset(&wqa, 0, sizeof(wqa));
	wqa.attr_mask = IBV_WQ_ATTR_STATE;
	wqa.wq_state = IBV_WQS_RDY;
	rc = ibv_modify_wq(q->wq, &wqa);
	if (rc) {
		errno = rc;
		rc = fail(err, errlen, "ibv_modify_wq to ready");
		goto fail_wq;
	}

	memset(&obj, 0, sizeof(obj));
	memset(&dvrwq, 0, sizeof(dvrwq));
	memset(&dvcq, 0, sizeof(dvcq));
	obj.rwq.in = q->wq;
	obj.rwq.out = &dvrwq;
	obj.cq.in = q->cq;
	obj.cq.out = &dvcq;
	rc = mlx5dv_init_obj(&obj, MLX5DV_OBJ_RWQ | MLX5DV_OBJ_CQ);
	if (rc) {
		errno = rc;
		rc = fail(err, errlen, "mlx5dv_init_obj for receive");
		goto fail_wq;
	}

	q->rq_buf = dvrwq.buf;
	q->rq_wqe_cnt = dvrwq.wqe_cnt;
	q->rq_stride = dvrwq.stride;
	q->rq_dbrec = &dvrwq.dbrec[0];   /* MLX5_RCV_DBR */
	q->cq_buf = dvcq.buf;
	q->cq_cqe_cnt = dvcq.cqe_cnt;
	q->cq_cqe_size = dvcq.cqe_size;
	q->cq_dbrec = &dvcq.dbrec[0];
	q->cqn = dvcq.cqn;

	if (!q->rq_buf || !q->cq_buf || !q->rq_dbrec || !q->cq_dbrec) {
		rc = failf(err, errlen, "the provider exposed an incomplete receive queue");
		goto fail_wq;
	}
	return 0;

fail_wq:
	ibv_destroy_wq(q->wq);
	q->wq = NULL;
fail_cq:
	ibv_destroy_cq(q->cq);
	q->cq = NULL;
	return rc;
}

int pio_destroy_rxq(struct pio_rxq *q, char *err, size_t errlen)
{
	int rc = 0, e;

	if (q->wq) {
		if ((e = ibv_destroy_wq(q->wq)) && !rc) {
			errno = e;
			rc = fail(err, errlen, "ibv_destroy_wq");
		}
		q->wq = NULL;
	}
	if (q->cq) {
		if ((e = ibv_destroy_cq(q->cq)) && !rc) {
			errno = e;
			rc = fail(err, errlen, "ibv_destroy_cq");
		}
		q->cq = NULL;
	}
	return rc;
}

int pio_create_rx_group(pio_ctx *c, struct pio_rxq *qs, uint32_t n,
			struct pio_rx_group *g, char *err, size_t errlen)
{
	struct ibv_rwq_ind_table_init_attr rwqia;
	struct ibv_qp_init_attr_ex qpia;
	struct ibv_wq **tbl = NULL;
	uint32_t log_size = 0, size, i;
	int rc;

	memset(g, 0, sizeof(*g));
	if (n == 0)
		return failf(err, errlen, "a receive group of no queues");

	/* The table has a power-of-two number of entries. With a queue count that
	 * is not one, the queues are repeated around it, which spreads traffic as
	 * evenly as the table allows. */
	while ((1u << log_size) < n)
		log_size++;
	size = 1u << log_size;

	tbl = calloc(size, sizeof(*tbl));
	if (!tbl)
		return failf(err, errlen, "out of memory");
	for (i = 0; i < size; i++)
		tbl[i] = qs[i % n].wq;

	memset(&rwqia, 0, sizeof(rwqia));
	rwqia.log_ind_tbl_size = log_size;
	rwqia.ind_tbl = tbl;
	g->ind_table = ibv_create_rwq_ind_table(c->ctx, &rwqia);
	free(tbl);
	if (!g->ind_table)
		return fail(err, errlen, "ibv_create_rwq_ind_table");

	memset(&qpia, 0, sizeof(qpia));
	qpia.qp_type = IBV_QPT_RAW_PACKET;
	qpia.comp_mask = IBV_QP_INIT_ATTR_PD | IBV_QP_INIT_ATTR_IND_TABLE |
			 IBV_QP_INIT_ATTR_RX_HASH;
	qpia.pd = c->pd;
	qpia.rwq_ind_tbl = g->ind_table;
	qpia.rx_hash_conf.rx_hash_function = IBV_RX_HASH_FUNC_TOEPLITZ;
	qpia.rx_hash_conf.rx_hash_key_len = sizeof(pio_rss_key);
	qpia.rx_hash_conf.rx_hash_key = (uint8_t *)pio_rss_key;
	qpia.rx_hash_conf.rx_hash_fields_mask =
		IBV_RX_HASH_SRC_IPV4 | IBV_RX_HASH_DST_IPV4 |
		IBV_RX_HASH_SRC_PORT_UDP | IBV_RX_HASH_DST_PORT_UDP;
	g->qp = ibv_create_qp_ex(c->ctx, &qpia);
	if (!g->qp) {
		rc = fail(err, errlen, "ibv_create_qp_ex for receive");
		ibv_destroy_rwq_ind_table(g->ind_table);
		g->ind_table = NULL;
		return rc;
	}
	return 0;
}

/* pio_build_flow lays one rule into buf and returns its length, or 0 with err
 * set when the match asks for something the card cannot express. */
static size_t pio_build_flow(uint8_t *buf, uint8_t port,
			     const struct pio_match *m,
			     char *err, size_t errlen)
{
	/* A flow rule is a header followed by its match specifications laid out
	 * end to end, in ascending protocol order. The specifications are of
	 * different sizes, so the rule is built in a buffer rather than as a
	 * struct: the header records how many follow and how long the whole is. */
	struct ibv_flow_attr *attr = (struct ibv_flow_attr *)buf;
	uint8_t *p;
	int nspecs = 0;
	int want_ip, want_l4;

	want_l4 = m->have_src_port || m->have_dst_port;
	want_ip = m->src_mask || m->dst_mask || m->have_proto || want_l4;

	/* The card matches a port inside a protocol it was told to expect, so a
	 * port without a protocol would match the same offset in something else.
	 * Refuse rather than install a rule that means something other than what
	 * was asked for. */
	if (want_l4 && !m->have_proto) {
		failf(err, errlen, "a port can only be matched together with a protocol");
		return 0;
	}
	if (want_ip && m->have_ether_type && m->ether_type != 0x0800) {
		failf(err, errlen, "an IPv4 match needs ethertype 0x0800, not 0x%04x",
		      m->ether_type);
		return 0;
	}

	memset(buf, 0, PIO_FLOW_BUF);
	attr->type = IBV_FLOW_ATTR_NORMAL;
	/* The port the device was opened on, not port 1. On a dual-port card
	 * they are different interfaces, and steering the wrong one takes
	 * traffic away from the kernel on a port nobody named -- which with a
	 * promiscuous filter can blackhole a management link. */
	attr->port = port;
	p = buf + sizeof(struct ibv_flow_attr);

	/* Ethernet. Always present: it is what carries the destination address
	 * and the VLAN, and the card wants the layers in order. */
	{
		struct ibv_flow_spec_eth eth;
		memset(&eth, 0, sizeof(eth));
		eth.type = IBV_FLOW_SPEC_ETH;
		eth.size = sizeof(eth);
		if (!m->promisc && m->have_mac) {
			memcpy(eth.val.dst_mac, m->dst_mac, 6);
			memset(eth.mask.dst_mac, 0xff, 6);
		}
		if (!m->promisc && m->vlan >= 0) {
			/* The tag is matched on its identifier only, so that a
			 * packet's priority bits do not decide whether it arrives. */
			eth.val.vlan_tag = htobe16((uint16_t)(m->vlan & 0x0fff));
			eth.mask.vlan_tag = htobe16(0x0fff);
		}
		if (m->have_ether_type) {
			eth.val.ether_type = htobe16(m->ether_type);
			eth.mask.ether_type = htobe16(0xffff);
		} else if (want_ip) {
			eth.val.ether_type = htobe16(0x0800);
			eth.mask.ether_type = htobe16(0xffff);
		}
		memcpy(p, &eth, sizeof(eth));
		p += sizeof(eth);
		nspecs++;
	}

	if (want_ip) {
		struct ibv_flow_spec_ipv4_ext ip;
		memset(&ip, 0, sizeof(ip));
		ip.type = IBV_FLOW_SPEC_IPV4_EXT;
		ip.size = sizeof(ip);
		/* Addresses and masks are already in network order. A zero mask
		 * matches any address, which is how "only the port matters" is
		 * expressed. */
		ip.val.src_ip = m->src_ip & m->src_mask;
		ip.mask.src_ip = m->src_mask;
		ip.val.dst_ip = m->dst_ip & m->dst_mask;
		ip.mask.dst_ip = m->dst_mask;
		if (m->have_proto) {
			ip.val.proto = m->ip_proto;
			ip.mask.proto = 0xff;
		}
		memcpy(p, &ip, sizeof(ip));
		p += sizeof(ip);
		nspecs++;
	}

	if (want_l4) {
		struct ibv_flow_spec_tcp_udp l4;
		memset(&l4, 0, sizeof(l4));
		l4.type = m->ip_proto == 6 ? IBV_FLOW_SPEC_TCP : IBV_FLOW_SPEC_UDP;
		l4.size = sizeof(l4);
		if (m->have_dst_port) {
			l4.val.dst_port = htobe16(m->dst_port);
			l4.mask.dst_port = htobe16(0xffff);
		}
		if (m->have_src_port) {
			l4.val.src_port = htobe16(m->src_port);
			l4.mask.src_port = htobe16(0xffff);
		}
		memcpy(p, &l4, sizeof(l4));
		p += sizeof(l4);
		nspecs++;
	}

	attr->num_of_specs = (uint8_t)nspecs;
	attr->size = (uint16_t)(p - buf);
	return (size_t)(p - buf);
}

int pio_group_steer(pio_ctx *c, struct pio_rx_group *g,
		    const struct pio_match *m, uint32_t n, char *err, size_t errlen)
{
	uint8_t buf[PIO_FLOW_BUF];
	uint32_t i;

	if (n == 0 || n > PIO_MAX_FLOWS)
		return failf(err, errlen, "a filter of %u rule(s); 1 to %d are allowed",
			     n, PIO_MAX_FLOWS);

	/* Take the old rules down first. Installing the new ones one at a time
	 * would otherwise leave a window where both sets are live and a packet
	 * matches a rule that is on its way out. */
	for (i = 0; i < g->nflows; i++) {
		if (g->flows[i])
			ibv_destroy_flow(g->flows[i]);
		g->flows[i] = NULL;
	}
	g->nflows = 0;

	for (i = 0; i < n; i++) {
		size_t len = pio_build_flow(buf, (uint8_t)c->port, &m[i], err, errlen);
		int rc;

		if (len == 0) {
			rc = -1;
		} else {
			g->flows[i] = ibv_create_flow(g->qp, (struct ibv_flow_attr *)buf);
			if (g->flows[i]) {
				g->nflows++;
				continue;
			}
			rc = failf(err, errlen, "ibv_create_flow for rule %u of %u", i + 1, n);
		}
		/* Leave nothing half-installed, whether the rule was refused
		 * here or by the card. A partial rule set delivers some of what
		 * was asked for, which is worse than none: nothing downstream
		 * can tell the difference. */
		while (i-- > 0) {
			ibv_destroy_flow(g->flows[i]);
			g->flows[i] = NULL;
		}
		g->nflows = 0;
		return rc;
	}
	return 0;
}

int pio_destroy_rx_group(struct pio_rx_group *g, char *err, size_t errlen)
{
	int rc = 0, e;

	/* Stop traffic arriving before taking away the place it arrives in. */
	for (uint32_t i = 0; i < g->nflows; i++) {
		if (!g->flows[i])
			continue;
		if ((e = ibv_destroy_flow(g->flows[i])) && !rc) {
			errno = e;
			rc = fail(err, errlen, "ibv_destroy_flow");
		}
		g->flows[i] = NULL;
	}
	g->nflows = 0;
	if (g->qp) {
		if ((e = ibv_destroy_qp(g->qp)) && !rc) {
			errno = e;
			rc = fail(err, errlen, "ibv_destroy_qp");
		}
		g->qp = NULL;
	}
	if (g->ind_table) {
		if ((e = ibv_destroy_rwq_ind_table(g->ind_table)) && !rc) {
			errno = e;
			rc = fail(err, errlen, "ibv_destroy_rwq_ind_table");
		}
		g->ind_table = NULL;
	}
	return rc;
}
