#include "shim.h"

/* DPDK's headers inline SSSE3 and RTM intrinsics -- rte_memcpy uses
 * _mm_alignr_epi8, rte_rtm uses _xbegin -- and pkg-config asks for
 * "-march=corei7 -mrtm" to compile them. cgo refuses to pass -m flags through,
 * so the target is set here, on the one translation unit that includes DPDK.
 * Without it the build fails on "inlining failed in call to always_inline
 * _mm_alignr_epi8: target specific option mismatch". */
#pragma GCC target("ssse3,sse4.2,rtm")

#include <rte_config.h>
#include <rte_eal.h>
#include <rte_ethdev.h>
#include <rte_errno.h>
#include <rte_mbuf.h>
#include <rte_mempool.h>
#include <rte_memzone.h>
#include <rte_malloc.h>
#include <rte_dev.h>
#include <rte_flow.h>
#include <rte_log.h>
#include <rte_version.h>

#include <errno.h>
#include <stdarg.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

/* Everything the Go side indexes by hand depends on these. */
_Static_assert(sizeof(struct rte_mbuf) == 128, "rte_mbuf is not 128 bytes");
_Static_assert(RTE_IOVA_IN_MBUF == 1, "buf_iova is not in the mbuf");
_Static_assert(RTE_PKTMBUF_HEADROOM == 128, "the headroom is not 128 bytes");

/* What the mempool library puts in front of every object. It is checked
 * against the mempool actually built rather than assumed. */
#define PIO_OBJ_HEADER 64

static int failf(char *err, size_t errlen, const char *fmt, ...)
{
	va_list ap;
	va_start(ap, fmt);
	vsnprintf(err, errlen, fmt, ap);
	va_end(ap);
	return -1;
}

/* ------------------------------------------------------------- the mempool */

/* The ops the driver calls. They move mbuf addresses between the two arrays
 * packetio owns; there is no other pool behind them. */

static int pio_mp_alloc(struct rte_mempool *mp)
{
	return mp->pool_config == NULL ? -EINVAL : 0;
}

static void pio_mp_free(struct rte_mempool *mp) { (void)mp; }

static int pio_mp_enqueue(struct rte_mempool *mp, void *const *obj, unsigned n)
{
	struct pio_pool *p = mp->pool_config;
	struct pio_ring *r = &p->returned;
	unsigned i;

	for (i = 0; i < n; i++) {
		if (r->tail - r->head > r->mask) {
			/* Losing an mbuf here loses a frame for good. The ring
			 * is sized from the queue depth so this cannot happen;
			 * counting it is how we would find out if it did. */
			__atomic_fetch_add(&p->drops, n - i, __ATOMIC_RELAXED);
			return -ENOBUFS;
		}
		r->buf[r->tail & r->mask] = (uint64_t)(uintptr_t)obj[i];
		r->tail++;
	}
	return 0;
}

static int pio_mp_dequeue(struct rte_mempool *mp, void **obj, unsigned n)
{
	struct pio_pool *p = mp->pool_config;
	struct pio_ring *r = &p->supply;
	unsigned i;

	/* All or nothing, as the mempool API promises the driver. */
	if (r->tail - r->head < n) {
		__atomic_fetch_add(&p->empty, 1, __ATOMIC_RELAXED);
		return -ENOENT;
	}
	for (i = 0; i < n; i++) {
		obj[i] = (void *)(uintptr_t)r->buf[r->head & r->mask];
		r->head++;
	}
	return 0;
}

static unsigned pio_mp_get_count(const struct rte_mempool *mp)
{
	struct pio_pool *p = mp->pool_config;
	return p->supply.tail - p->supply.head;
}

static struct rte_mempool_ops pio_mp_ops = {
	.name = "packetio",
	.alloc = pio_mp_alloc,
	.free = pio_mp_free,
	.enqueue = pio_mp_enqueue,
	.dequeue = pio_mp_dequeue,
	.get_count = pio_mp_get_count,
};
RTE_MEMPOOL_REGISTER_OPS(pio_mp_ops);

static int ring_init(struct pio_ring *r, uint32_t size, int socket)
{
	r->buf = rte_zmalloc_socket(NULL, (size_t)size * sizeof(uint64_t),
				    RTE_CACHE_LINE_SIZE, socket);
	if (!r->buf)
		return -1;
	r->mask = size - 1;
	r->head = r->tail = 0;
	return 0;
}

int pio_mempool_new(const char *name, void *vaddr, uint64_t iova, uint32_t n,
		    uint32_t frame_size, uint32_t ring_size, int socket,
		    void **mp_out, struct pio_pool **pool_out, char *err, size_t errlen)
{
	struct rte_mempool *mp;
	struct rte_pktmbuf_pool_private priv;
	struct pio_pool *pool;
	uint32_t elt_size;
	int rc;

	if (ring_size == 0 || (ring_size & (ring_size - 1)))
		return failf(err, errlen, "a ring of %u entries, which must be a power of two",
			     ring_size);

	pool = rte_zmalloc_socket(NULL, sizeof(*pool), RTE_CACHE_LINE_SIZE, socket);
	if (!pool)
		return failf(err, errlen, "allocating the pool arrays");
	if (ring_init(&pool->supply, ring_size, socket) != 0 ||
	    ring_init(&pool->returned, ring_size, socket) != 0) {
		pio_mempool_free(NULL, pool);
		return failf(err, errlen, "allocating the ring buffers");
	}

	/* One object per frame: the mempool's own object header, then the mbuf,
	 * then the data buffer. RTE_MEMPOOL_F_NO_SPREAD is not optional --
	 * without it the library pads between objects to stripe them across
	 * memory channels, and an object stops being exactly one frame.
	 *
	 * The header is PIO_OBJ_HEADER rather than RTE_MEMPOOL_HEADER_SIZE,
	 * which is the size of the mempool struct and not of an object header.
	 * Using it put 4 KiB between objects and fitted 511 of 1024 frames.
	 * What the library actually chose is checked below. */
	elt_size = frame_size - PIO_OBJ_HEADER;
	mp = rte_mempool_create_empty(name, n, elt_size, 0,
				      sizeof(struct rte_pktmbuf_pool_private),
				      socket, RTE_MEMPOOL_F_NO_SPREAD);
	if (!mp) {
		pio_mempool_free(NULL, pool);
		return failf(err, errlen, "rte_mempool_create_empty(%s): %s", name,
			     rte_strerror(rte_errno));
	}
	if (mp->header_size != PIO_OBJ_HEADER || mp->trailer_size != 0) {
		uint32_t h = mp->header_size, t = mp->trailer_size;
		rte_mempool_free(mp);
		pio_mempool_free(NULL, pool);
		return failf(err, errlen, "this DPDK puts %u bytes in front of a mempool object and "
			     "%u behind it; this backend's frame layout needs %d and 0",
			     h, t, PIO_OBJ_HEADER);
	}
	rc = rte_mempool_set_ops_byname(mp, "packetio", pool);
	if (rc != 0) {
		rte_mempool_free(mp);
		pio_mempool_free(NULL, pool);
		return failf(err, errlen, "set_ops_byname(packetio): %d", rc);
	}
	memset(&priv, 0, sizeof(priv));
	priv.mbuf_data_room_size = (uint16_t)(elt_size - sizeof(struct rte_mbuf));
	priv.mbuf_priv_size = 0;
	rte_pktmbuf_pool_init(mp, &priv);

	rc = rte_mempool_populate_iova(mp, vaddr, iova, (size_t)n * frame_size, NULL, NULL);
	if (rc < 0) {
		rte_mempool_free(mp);
		pio_mempool_free(NULL, pool);
		return failf(err, errlen, "rte_mempool_populate_iova(%s): %d (%s)", name, rc,
			     rte_strerror(-rc));
	}
	if ((uint32_t)rc != n) {
		rte_mempool_free(mp);
		pio_mempool_free(NULL, pool);
		return failf(err, errlen, "%s took %d objects of the %u it was given room for",
			     name, rc, n);
	}
	/* Bind every mbuf to the data buffer that follows it, once and for all. */
	rte_mempool_obj_iter(mp, rte_pktmbuf_init, NULL);

	/* Populate handed every object to our enqueue, so they are all sitting
	 * in the returned ring. The caller's free list already accounts for
	 * every frame, so drop them rather than counting them twice. */
	pool->returned.head = pool->returned.tail = 0;

	*mp_out = mp;
	*pool_out = pool;
	return 0;
}

int pio_mempool_free(void *mp, struct pio_pool *pool)
{
	if (mp)
		rte_mempool_free(mp);
	if (pool) {
		rte_free(pool->supply.buf);
		rte_free(pool->returned.buf);
		rte_free(pool);
	}
	return 0;
}

void pio_mempool_layout(void *mp_, uint32_t *header, uint32_t *elt, uint32_t *trailer)
{
	struct rte_mempool *mp = mp_;
	*header = mp->header_size;
	*elt = mp->elt_size;
	*trailer = mp->trailer_size;
}

/* --------------------------------------------------------- the environment */

int pio_eal_init(char **argv, int argc, char *err, size_t errlen)
{
	if (rte_eal_init(argc, argv) < 0)
		return failf(err, errlen, "rte_eal_init: %s", rte_strerror(rte_errno));
	return 0;
}

/* pio_log_open points the EAL's log stream at a pipe and hands back the read
 * end. Without it a refusal from a driver is lost to stderr and the caller is
 * left with a bare errno. */
int pio_log_open(char *err, size_t errlen)
{
	int fds[2];
	FILE *w;

	if (pipe(fds) != 0)
		return failf(err, errlen, "pipe for the EAL log: %s", strerror(errno));
	w = fdopen(fds[1], "w");
	if (!w) {
		close(fds[0]);
		close(fds[1]);
		return failf(err, errlen, "fdopen for the EAL log: %s", strerror(errno));
	}
	setvbuf(w, NULL, _IOLBF, 0);
	if (rte_openlog_stream(w) != 0) {
		fclose(w);
		close(fds[0]);
		return failf(err, errlen, "rte_openlog_stream");
	}
	return fds[0];
}

int pio_probe(const char *devargs, uint16_t *port, char *err, size_t errlen)
{
	uint16_t p;
	int rc, found = 0;

	/* Which port a probe created is not reported, so the set is compared
	 * before and after. RTE_ETH_FOREACH_DEV skips ports that are not owned
	 * and not started, which is exactly the new one. */
	uint64_t before = 0;
	RTE_ETH_FOREACH_DEV(p) before |= 1ULL << p;

	rc = rte_dev_probe(devargs);
	if (rc < 0)
		return failf(err, errlen, "probing %s: %d (%s)", devargs, rc,
			     rc == -EEXIST ? "already probed" : rte_strerror(-rc));

	RTE_ETH_FOREACH_DEV(p) {
		if (!(before & (1ULL << p))) {
			*port = p;
			found = 1;
			break;
		}
	}
	if (!found)
		return failf(err, errlen, "%s probed but produced no ethernet port", devargs);
	return 0;
}

int pio_find_port(const char *devargs, uint16_t *port)
{
	struct rte_eth_dev_info info;
	uint16_t p;
	size_t n;
	const char *comma = strchr(devargs, ',');

	/* Devargs may carry driver arguments after a comma; the device's own
	 * name is the part before it. */
	n = comma ? (size_t)(comma - devargs) : strlen(devargs);

	RTE_ETH_FOREACH_DEV(p) {
		const char *dn;

		/* struct rte_device is opaque to anything outside a driver, so the
		 * name comes from the accessor rather than the field. */
		if (rte_eth_dev_info_get(p, &info) != 0 || !info.device)
			continue;
		dn = rte_dev_name(info.device);
		if (dn && strlen(dn) == n && strncmp(dn, devargs, n) == 0) {
			*port = p;
			return 0;
		}
	}
	return -1;
}

void *pio_dev_handle(uint16_t port)
{
	struct rte_eth_dev_info info;

	if (rte_eth_dev_info_get(port, &info) != 0)
		return NULL;
	return info.device;
}

int pio_remove(void *dev, char *err, size_t errlen)
{
	int rc;

	if (!dev)
		return 0;
	rc = rte_dev_remove(dev);
	if (rc < 0 && rc != -ENOENT)
		return failf(err, errlen, "detaching the device: %d (%s)", rc, rte_strerror(-rc));
	return 0;
}

int pio_socket_id(uint16_t port)
{
	int s = rte_eth_dev_socket_id(port);
	return s < 0 ? SOCKET_ID_ANY : s;
}

int pio_iova_mode(void) { return (int)rte_eal_iova_mode(); }

int pio_dev_info(uint16_t port, struct pio_dev_info *out, char *err, size_t errlen)
{
	struct rte_eth_dev_info info;
	struct rte_ether_addr mac;

	memset(out, 0, sizeof(*out));
	if (rte_eth_dev_info_get(port, &info) != 0)
		return failf(err, errlen, "rte_eth_dev_info_get(%u)", port);

	snprintf(out->driver, sizeof(out->driver), "%s",
		 info.driver_name ? info.driver_name : "unknown");
	out->socket = pio_socket_id(port);
	out->max_rx_queues = info.max_rx_queues;
	out->max_tx_queues = info.max_tx_queues;
	out->min_rx_bufsize = info.min_rx_bufsize;
	out->max_rx_pktlen = info.max_rx_pktlen;
	out->rx_offload_capa = info.rx_offload_capa;
	out->tx_offload_capa = info.tx_offload_capa;
	out->rx_desc_max = info.rx_desc_lim.nb_max;
	out->tx_desc_max = info.tx_desc_lim.nb_max;
	if (rte_eth_macaddr_get(port, &mac) == 0)
		memcpy(out->mac, mac.addr_bytes, 6);
	/* A device the kernel still has a netdev for is one this backend can
	 * share: its steering diverts traffic and the rest reaches the host.
	 * if_index is how the ethdev layer reports that. */
	out->is_bifurcated = info.if_index != 0;
	return 0;
}

/* -------------------------------------------------------------- the device */

int pio_configure(uint16_t port, uint16_t nrx, uint16_t ntx, uint32_t mtu,
		  uint32_t offloads, uint32_t *applied, char *err, size_t errlen)
{
	struct rte_eth_conf conf;
	struct rte_eth_dev_info info;
	int rc;

	if (rte_eth_dev_info_get(port, &info) != 0)
		return failf(err, errlen, "rte_eth_dev_info_get(%u)", port);

	memset(&conf, 0, sizeof(conf));
	conf.rxmode.mq_mode = RTE_ETH_MQ_RX_NONE;
	conf.txmode.mq_mode = RTE_ETH_MQ_TX_NONE;
	conf.rxmode.mtu = mtu;
	if (nrx > 1) {
		conf.rxmode.mq_mode = RTE_ETH_MQ_RX_RSS;
		conf.rx_adv_conf.rss_conf.rss_hf =
			(RTE_ETH_RSS_IP | RTE_ETH_RSS_UDP | RTE_ETH_RSS_TCP) &
			info.flow_type_rss_offloads;
	}
	/* Only offloads the device actually reports are asked for; anything
	 * else is refused by configure with a message about capabilities. */
	if (offloads & PIO_RX_OFFLOAD_CHECKSUM)
		conf.rxmode.offloads |= (RTE_ETH_RX_OFFLOAD_IPV4_CKSUM |
					 RTE_ETH_RX_OFFLOAD_UDP_CKSUM |
					 RTE_ETH_RX_OFFLOAD_TCP_CKSUM) &
					info.rx_offload_capa;
	if (offloads & PIO_TX_OFFLOAD_CHECKSUM)
		conf.txmode.offloads |= (RTE_ETH_TX_OFFLOAD_IPV4_CKSUM |
					 RTE_ETH_TX_OFFLOAD_UDP_CKSUM |
					 RTE_ETH_TX_OFFLOAD_TCP_CKSUM) &
					info.tx_offload_capa;
	if (offloads & PIO_TX_OFFLOAD_TSO)
		conf.txmode.offloads |= RTE_ETH_TX_OFFLOAD_TCP_TSO & info.tx_offload_capa;

	rc = rte_eth_dev_configure(port, nrx, ntx, &conf);
	if (rc != 0)
		return failf(err, errlen, "configuring port %u for %u rx and %u tx queues: %d (%s)",
			     port, nrx, ntx, rc, rte_strerror(-rc));

	/* What the device took, which is not always what was asked for: the
	 * masks above drop anything the PMD does not report. Capabilities are
	 * built from this rather than from the request, so a caller is never
	 * told a NIC computes a checksum it will not compute. Rx checksums are
	 * claimed only when all three arrived, since packetio's OptChecksumOK
	 * covers L3 and L4 together. */
	if (applied) {
		const uint64_t rxall = RTE_ETH_RX_OFFLOAD_IPV4_CKSUM |
				       RTE_ETH_RX_OFFLOAD_UDP_CKSUM |
				       RTE_ETH_RX_OFFLOAD_TCP_CKSUM;
		const uint64_t txall = RTE_ETH_TX_OFFLOAD_IPV4_CKSUM |
				       RTE_ETH_TX_OFFLOAD_UDP_CKSUM |
				       RTE_ETH_TX_OFFLOAD_TCP_CKSUM;
		*applied = 0;
		if ((conf.rxmode.offloads & rxall) == rxall)
			*applied |= PIO_RX_OFFLOAD_CHECKSUM;
		if ((conf.txmode.offloads & txall) == txall)
			*applied |= PIO_TX_OFFLOAD_CHECKSUM;
		if (conf.txmode.offloads & RTE_ETH_TX_OFFLOAD_TCP_TSO)
			*applied |= PIO_TX_OFFLOAD_TSO;
	}
	return 0;
}

int pio_rx_queue_setup(uint16_t port, uint16_t q, uint16_t desc, int socket,
		       void *mp, char *err, size_t errlen)
{
	int rc = rte_eth_rx_queue_setup(port, q, desc, socket, NULL, mp);
	if (rc != 0)
		return failf(err, errlen, "receive queue %u of %u descriptors: %d (%s)",
			     q, desc, rc, rte_strerror(-rc));
	return 0;
}

int pio_tx_queue_setup(uint16_t port, uint16_t q, uint16_t desc, int socket,
		       char *err, size_t errlen)
{
	int rc = rte_eth_tx_queue_setup(port, q, desc, socket, NULL);
	if (rc != 0)
		return failf(err, errlen, "transmit queue %u of %u descriptors: %d (%s)",
			     q, desc, rc, rte_strerror(-rc));
	return 0;
}

int pio_start(uint16_t port, char *err, size_t errlen)
{
	int rc = rte_eth_dev_start(port);
	if (rc != 0)
		return failf(err, errlen, "starting port %u: %d (%s)", port, rc,
			     rte_strerror(-rc));
	return 0;
}

/* Only a negative return is a failure. The ethdev API documents stop and close
 * as "zero on success, negative if something went wrong", and a driver may
 * return a positive value that means nothing to us: ixgbe's close returns 1,
 * which read as an error made every Close on an Intel card fail, while mlx5
 * returns 0 and never showed it. */
int pio_stop(uint16_t port, char *err, size_t errlen)
{
	int rc = rte_eth_dev_stop(port);
	if (rc < 0 && rc != -ENODEV)
		return failf(err, errlen, "stopping port %u: %d (%s)", port, rc,
			     rte_strerror(-rc));
	return 0;
}

int pio_eal_close(uint16_t port, char *err, size_t errlen)
{
	int rc = rte_eth_dev_close(port);
	if (rc < 0 && rc != -ENODEV)
		return failf(err, errlen, "closing port %u: %d (%s)", port, rc,
			     rte_strerror(-rc));
	return 0;
}

int pio_promiscuous(uint16_t port, int on, char *err, size_t errlen)
{
	int rc = on ? rte_eth_promiscuous_enable(port) : rte_eth_promiscuous_disable(port);
	if (rc != 0 && rc != -ENOTSUP)
		return failf(err, errlen, "promiscuous mode on port %u: %d", port, rc);
	if (rc == -ENOTSUP)
		return failf(err, errlen, "this device cannot be put in promiscuous mode");
	return 0;
}

int pio_link_status(uint16_t port, int *up, uint32_t *speed_mbps)
{
	struct rte_eth_link link;

	memset(&link, 0, sizeof(link));
	if (rte_eth_link_get_nowait(port, &link) != 0)
		return -1;
	*up = link.link_status == RTE_ETH_LINK_UP;
	*speed_mbps = link.link_speed;
	return 0;
}

/* -------------------------------------------------------------- the memory */

int pio_region_reserve(const char *name, size_t size, size_t align, int socket,
		       void **addr, uint64_t *iova, char *err, size_t errlen)
{
	const struct rte_memzone *mz;

	mz = rte_memzone_reserve_aligned(name, size, socket, RTE_MEMZONE_IOVA_CONTIG, align);
	if (!mz)
		return failf(err, errlen, "reserving %zu bytes of frame memory: %s "
			     "(are hugepages reserved, and is the memory IOVA-contiguous?)",
			     size, rte_strerror(rte_errno));
	*addr = mz->addr;
	*iova = mz->iova;
	return 0;
}

int pio_region_free(const char *name, char *err, size_t errlen)
{
	const struct rte_memzone *mz = rte_memzone_lookup(name);
	if (!mz)
		return 0;
	if (rte_memzone_free(mz) != 0)
		return failf(err, errlen, "freeing the frame memory");
	return 0;
}

/* ------------------------------------------------------------ the fast path */

uint16_t pio_rx_burst(uint16_t port, uint16_t q, uint64_t *mbufs, uint16_t n)
{
	return rte_eth_rx_burst(port, q, (struct rte_mbuf **)mbufs, n);
}

uint16_t pio_tx_burst(uint16_t port, uint16_t q, uint64_t *mbufs, uint16_t n)
{
	return rte_eth_tx_burst(port, q, (struct rte_mbuf **)mbufs, n);
}

void pio_tx_poke(uint16_t port, uint16_t q)
{
	if (rte_eth_tx_done_cleanup(port, q, 0) == -ENOTSUP)
		rte_eth_tx_descriptor_status(port, q, 0);
}

/* ------------------------------------------------------------ the steering */

int pio_flow_isolate(uint16_t port, int set, char *err, size_t errlen)
{
	struct rte_flow_error fe;

	memset(&fe, 0, sizeof(fe));
	if (rte_flow_isolate(port, set, &fe) != 0)
		return failf(err, errlen, "isolated mode: %s",
			     fe.message ? fe.message : "not supported by this driver");
	return 0;
}

int pio_flow_create(uint16_t port, const struct pio_match *m, uint16_t *queues,
		    uint16_t nq, int validate_only, char *err, size_t errlen)
{
	struct rte_flow_attr attr;
	struct rte_flow_item items[6];
	struct rte_flow_action actions[2];
	struct rte_flow_item_eth eth_spec, eth_mask;
	struct rte_flow_item_vlan vlan_spec, vlan_mask;
	struct rte_flow_item_ipv4 ip_spec, ip_mask;
	struct rte_flow_item_udp udp_spec, udp_mask;
	struct rte_flow_item_tcp tcp_spec, tcp_mask;
	struct rte_flow_action_rss rss;
	struct rte_flow_action_queue queue;
	struct rte_flow_error fe;
	int n = 0, need_ip;

	memset(&attr, 0, sizeof(attr));
	memset(items, 0, sizeof(items));
	memset(actions, 0, sizeof(actions));
	memset(&eth_spec, 0, sizeof(eth_spec));
	memset(&eth_mask, 0, sizeof(eth_mask));
	memset(&vlan_spec, 0, sizeof(vlan_spec));
	memset(&vlan_mask, 0, sizeof(vlan_mask));
	memset(&ip_spec, 0, sizeof(ip_spec));
	memset(&ip_mask, 0, sizeof(ip_mask));
	memset(&udp_spec, 0, sizeof(udp_spec));
	memset(&udp_mask, 0, sizeof(udp_mask));
	memset(&tcp_spec, 0, sizeof(tcp_spec));
	memset(&tcp_mask, 0, sizeof(tcp_mask));
	memset(&rss, 0, sizeof(rss));
	memset(&queue, 0, sizeof(queue));
	memset(&fe, 0, sizeof(fe));

	attr.ingress = 1;

	/* The items go in ascending protocol order, which the API requires. */
	items[n].type = RTE_FLOW_ITEM_TYPE_ETH;
	if (m->have_mac || m->have_ether_type) {
		if (m->have_mac) {
			memcpy(eth_spec.hdr.dst_addr.addr_bytes, m->dst_mac, 6);
			memset(eth_mask.hdr.dst_addr.addr_bytes, 0xff, 6);
		}
		if (m->have_ether_type) {
			eth_spec.hdr.ether_type = rte_cpu_to_be_16(m->ether_type);
			eth_mask.hdr.ether_type = RTE_BE16(0xffff);
		}
		items[n].spec = &eth_spec;
		items[n].mask = &eth_mask;
	}
	n++;

	if (m->vlan >= 0) {
		vlan_spec.hdr.vlan_tci = rte_cpu_to_be_16((uint16_t)m->vlan);
		vlan_mask.hdr.vlan_tci = RTE_BE16(0x0fff);
		items[n].type = RTE_FLOW_ITEM_TYPE_VLAN;
		items[n].spec = &vlan_spec;
		items[n].mask = &vlan_mask;
		n++;
	}

	need_ip = m->src_mask || m->dst_mask || m->have_proto ||
		  m->have_src_port || m->have_dst_port;
	if (need_ip) {
		items[n].type = RTE_FLOW_ITEM_TYPE_IPV4;
		if (m->src_mask || m->dst_mask || m->have_proto) {
			ip_spec.hdr.src_addr = m->src_ip;
			ip_mask.hdr.src_addr = m->src_mask;
			ip_spec.hdr.dst_addr = m->dst_ip;
			ip_mask.hdr.dst_addr = m->dst_mask;
			if (m->have_proto) {
				ip_spec.hdr.next_proto_id = m->ip_proto;
				ip_mask.hdr.next_proto_id = 0xff;
			}
			items[n].spec = &ip_spec;
			items[n].mask = &ip_mask;
		}
		n++;
	}

	if (m->have_src_port || m->have_dst_port) {
		if (m->ip_proto == IPPROTO_TCP) {
			if (m->have_src_port) {
				tcp_spec.hdr.src_port = rte_cpu_to_be_16(m->src_port);
				tcp_mask.hdr.src_port = RTE_BE16(0xffff);
			}
			if (m->have_dst_port) {
				tcp_spec.hdr.dst_port = rte_cpu_to_be_16(m->dst_port);
				tcp_mask.hdr.dst_port = RTE_BE16(0xffff);
			}
			items[n].type = RTE_FLOW_ITEM_TYPE_TCP;
			items[n].spec = &tcp_spec;
			items[n].mask = &tcp_mask;
		} else {
			if (m->have_src_port) {
				udp_spec.hdr.src_port = rte_cpu_to_be_16(m->src_port);
				udp_mask.hdr.src_port = RTE_BE16(0xffff);
			}
			if (m->have_dst_port) {
				udp_spec.hdr.dst_port = rte_cpu_to_be_16(m->dst_port);
				udp_mask.hdr.dst_port = RTE_BE16(0xffff);
			}
			items[n].type = RTE_FLOW_ITEM_TYPE_UDP;
			items[n].spec = &udp_spec;
			items[n].mask = &udp_mask;
		}
		n++;
	}
	items[n].type = RTE_FLOW_ITEM_TYPE_END;

	if (nq > 1) {
		rss.queue = queues;
		rss.queue_num = nq;
		rss.types = RTE_ETH_RSS_IP | RTE_ETH_RSS_UDP | RTE_ETH_RSS_TCP;
		actions[0].type = RTE_FLOW_ACTION_TYPE_RSS;
		actions[0].conf = &rss;
	} else {
		queue.index = queues[0];
		actions[0].type = RTE_FLOW_ACTION_TYPE_QUEUE;
		actions[0].conf = &queue;
	}
	actions[1].type = RTE_FLOW_ACTION_TYPE_END;

	if (rte_flow_validate(port, &attr, items, actions, &fe) != 0)
		return failf(err, errlen, "this device will not match that: %s",
			     fe.message ? fe.message : "refused with no reason given");
	if (validate_only)
		return 0;
	if (!rte_flow_create(port, &attr, items, actions, &fe))
		return failf(err, errlen, "installing the rule: %s",
			     fe.message ? fe.message : "refused with no reason given");
	return 0;
}

int pio_flow_flush(uint16_t port, char *err, size_t errlen)
{
	struct rte_flow_error fe;

	memset(&fe, 0, sizeof(fe));
	if (rte_flow_flush(port, &fe) != 0)
		return failf(err, errlen, "removing the steering rules: %s",
			     fe.message ? fe.message : "refused");
	return 0;
}

/* --------------------------------------------------------------- the counts */

int pio_stats(uint16_t port, struct pio_stats *out, char *err, size_t errlen)
{
	struct rte_eth_stats st;

	if (rte_eth_stats_get(port, &st) != 0)
		return failf(err, errlen, "reading the counters of port %u", port);
	out->ipackets = st.ipackets;
	out->opackets = st.opackets;
	out->ibytes = st.ibytes;
	out->obytes = st.obytes;
	out->imissed = st.imissed;
	out->ierrors = st.ierrors;
	out->oerrors = st.oerrors;
	out->rx_nombuf = st.rx_nombuf;
	return 0;
}
