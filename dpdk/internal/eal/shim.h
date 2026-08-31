/* Bring-up for a DPDK device, and the three calls the packet path makes.
 *
 * This header deliberately includes nothing from DPDK. cgo compiles it, and
 * DPDK's own headers cannot be compiled by cgo: pkg-config asks for -mrtm and
 * -march=corei7 to build the intrinsics in rte_memcpy and rte_rtm, and cgo
 * refuses to pass -m flags through. shim.c sets the target with a pragma
 * instead, on the one translation unit that needs it, and everything Go has to
 * see is declared here in plain C.
 *
 * Every function returns 0 on success, or -1 with a message in err.
 */

#ifndef PACKETIO_DPDK_SHIM_H
#define PACKETIO_DPDK_SHIM_H

#include <stddef.h>
#include <stdint.h>

/* pio_ring is one of the two arrays that stand between the driver and
 * packetio's free list.
 *
 * Go and the mempool ops share it. Each index has exactly one writer: Go moves
 * the supply's tail and the returned ring's head, the driver moves the other
 * two. Both run on the queue's own thread -- the driver only touches a ring
 * from inside a burst call that thread made -- so there is nothing to
 * synchronise and no atomic here.
 */
struct pio_ring {
	uint64_t *buf;   /* mbuf addresses; capacity is mask+1, a power of two */
	uint32_t  mask;
	uint32_t  pad;
	uint32_t  head;  /* consumer reads here */
	uint32_t  tail;  /* producer writes here */
};

/* pio_pool is what a mempool's pool_config points at: the supply the driver
 * takes receive buffers from, and the returned ring it frees finished buffers
 * into. drops counts frees that did not fit, which must never happen -- the
 * rings are sized from the queue depth so that they cannot. */
struct pio_pool {
	struct pio_ring supply;
	struct pio_ring returned;
	uint64_t drops;
	uint64_t empty;   /* bulk gets the supply could not satisfy */
};

/* What a device can do, filled in by pio_dev_info. */
struct pio_dev_info {
	char     driver[64];
	int      socket;
	uint16_t max_rx_queues;
	uint16_t max_tx_queues;
	uint16_t min_rx_bufsize;
	uint32_t max_rx_pktlen;
	uint64_t rx_offload_capa;
	uint64_t tx_offload_capa;
	uint16_t rx_desc_max;
	uint16_t tx_desc_max;
	uint8_t  mac[6];
	int      is_bifurcated;  /* the kernel keeps a netdev for this device */
};

/* Offload bits this backend asks for, translated in shim.c so that Go does not
 * have to carry DPDK's constants. */
#define PIO_RX_OFFLOAD_CHECKSUM 0x1
#define PIO_TX_OFFLOAD_CHECKSUM 0x2
#define PIO_TX_OFFLOAD_TSO      0x4

/* One match in a steering rule. A field is matched only when its "have" flag
 * is set, so the zero struct matches everything the port sees. This is the
 * same vocabulary the mlx5 backend compiles a packetio.SteeringFilter into. */
struct pio_match {
	uint8_t  dst_mac[6];
	int      have_mac;
	int      vlan;                 /* VLAN id, or -1 for any */
	uint16_t ether_type;
	int      have_ether_type;
	uint32_t src_ip, dst_ip;       /* network order */
	uint32_t src_mask, dst_mask;   /* network order; 0 means not matched */
	uint8_t  ip_proto;
	int      have_proto;
	uint16_t src_port, dst_port;   /* host order */
	int      have_src_port, have_dst_port;
};

/* ---------------------------------------------------------- the environment */

/* pio_eal_init starts the environment abstraction layer. It may be called only
 * once in the life of a process: DPDK cannot be initialised again after
 * rte_eal_cleanup, so this backend never cleans up and hot-plugs devices
 * instead. The caller must run it on a thread it is willing to give up, since
 * rte_eal_init sets that thread's affinity to the main lcore. */
int  pio_eal_init(char **argv, int argc, char *err, size_t errlen);

/* pio_log_open returns a file descriptor the EAL's own messages are written to,
 * so that a failure to open a device can carry what the driver said rather than
 * a bare errno. */
int  pio_log_open(char *err, size_t errlen);

/* pio_probe brings up one device and reports the port it became. devargs is a
 * PCI address or a vdev specification. */
int  pio_probe(const char *devargs, uint16_t *port, char *err, size_t errlen);

/* pio_find_port reports which port a device already brought up became. The
 * environment probes the first device itself, from its own arguments, and does
 * not say what port it made, so it has to be looked for. Returns 0 when found. */
int  pio_find_port(const char *devargs, uint16_t *port);
/* pio_dev_handle is the bus device behind a port, taken before the port is
 * closed: closing releases the port id, and after that there is no way back to
 * the device to detach it. */
void *pio_dev_handle(uint16_t port);

/* pio_remove detaches a device taken from pio_dev_handle, so the same one can
 * be opened again. */
int  pio_remove(void *dev, char *err, size_t errlen);

int  pio_dev_info(uint16_t port, struct pio_dev_info *out, char *err, size_t errlen);
int  pio_socket_id(uint16_t port);
int  pio_iova_mode(void);

/* --------------------------------------------------------------- the device */

int  pio_configure(uint16_t port, uint16_t nrx, uint16_t ntx, uint32_t mtu,
		   uint32_t offloads, uint32_t *applied, char *err, size_t errlen);
int  pio_rx_queue_setup(uint16_t port, uint16_t q, uint16_t desc, int socket,
			void *mp, char *err, size_t errlen);
int  pio_tx_queue_setup(uint16_t port, uint16_t q, uint16_t desc, int socket,
			char *err, size_t errlen);
int  pio_start(uint16_t port, char *err, size_t errlen);
int  pio_stop(uint16_t port, char *err, size_t errlen);
int  pio_eal_close(uint16_t port, char *err, size_t errlen);
int  pio_promiscuous(uint16_t port, int on, char *err, size_t errlen);

/* pio_link_status reports whether the port has carrier and at what speed.
 * A copper port negotiates for seconds after it is started, and anything
 * transmitted meanwhile piles up in a ring that is not draining. */
int  pio_link_status(uint16_t port, int *up, uint32_t *speed_mbps);

/* --------------------------------------------------------------- the memory */

/* pio_region_reserve takes one IOVA-contiguous memzone for the frames. Its
 * address is what every descriptor is an offset into, and its IOVA is what the
 * NIC knows the same memory by. */
int  pio_region_reserve(const char *name, size_t size, size_t align, int socket,
			void **addr, uint64_t *iova, char *err, size_t errlen);
int  pio_region_free(const char *name, char *err, size_t errlen);

/* pio_mempool_new builds one queue's mempool over its slice of the region.
 *
 * The mempool is populated over that memory rather than left empty: an
 * unpopulated one is refused by the mlx5 driver, whose memory-registration
 * cache walks a mempool's chunks and finds none. Populating it is also what
 * puts one object -- header, mbuf and data buffer -- in each frame, which is
 * what lets packetio's frame arithmetic survive.
 *
 * The objects populate hands back are discarded here: the caller's free list
 * already accounts for every frame. */
int  pio_mempool_new(const char *name, void *vaddr, uint64_t iova, uint32_t n,
		     uint32_t frame_size, uint32_t ring_size, int socket,
		     void **mp, struct pio_pool **pool, char *err, size_t errlen);
int  pio_mempool_free(void *mp, struct pio_pool *pool);

/* pio_mempool_layout reports what the mempool library actually built, so the
 * caller can check it against what the layout assumed rather than trust it. */
void pio_mempool_layout(void *mp, uint32_t *header, uint32_t *elt, uint32_t *trailer);

/* ------------------------------------------------------------- the fast path */

uint16_t pio_rx_burst(uint16_t port, uint16_t q, uint64_t *mbufs, uint16_t n);
uint16_t pio_tx_burst(uint16_t port, uint16_t q, uint64_t *mbufs, uint16_t n);

/* pio_tx_poke asks the driver to look at its transmit completions.
 *
 * DPDK has no call that means that. A driver reads completions inside its own
 * transmit burst and nowhere else, so a queue that has stopped sending never
 * gets its last frames back. rte_eth_tx_done_cleanup is the intended answer and
 * mlx5 does not implement it; rte_eth_tx_descriptor_status runs the handler as
 * a side effect and is the fallback. */
void pio_tx_poke(uint16_t port, uint16_t q);

/* ------------------------------------------------------------- the steering */

int  pio_flow_isolate(uint16_t port, int set, char *err, size_t errlen);
int  pio_flow_create(uint16_t port, const struct pio_match *m, uint16_t *queues,
		     uint16_t nq, int validate_only, char *err, size_t errlen);
int  pio_flow_flush(uint16_t port, char *err, size_t errlen);

/* ---------------------------------------------------------------- the counts */

struct pio_stats {
	uint64_t ipackets, opackets, ibytes, obytes;
	uint64_t imissed, ierrors, oerrors, rx_nombuf;
};
int  pio_stats(uint16_t port, struct pio_stats *out, char *err, size_t errlen);

#endif /* PACKETIO_DPDK_SHIM_H */
