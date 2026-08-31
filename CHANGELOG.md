# Changelog

## Unreleased

**afxdp: small fleets get the fast transmit descriptors - +20-30% on
1-4 queues, inherited from go-afxdp v0.11.0.** mlx5's `xdp_tx_mpwqe`
private flag switches the kernel's XDP transmit path between the same two
descriptor economies this project keeps meeting on the card: copied
multi-packet entries (the kernel default - more CPU per packet, with a 25%
step where an inlined packet crosses from four to five 16-byte slots at
61-byte frames, but a ceiling that multiplies with queues) and pointer
entries (17.5 Mpps per core against 8.5-14.3, no step, walled at ~75 Mpps
device-wide). go-afxdp's Open now makes the queue count's choice: four
queues or fewer turn the flag off while the fleet runs, restored on Close,
reported in Info, `WithoutMPWQETune` to decline; five or more leave the
kernel default, the only way past the wall. Nothing in this repo changed
for it - the wrapper measured 14.4/29.4/60.1 -> **18.7/35.9/71.2 Mpps**
on 1/2/4 queues with an 8-queue fleet untouched at 120.5; receive is
unchanged and low-queue forwarding gains ~16% (28.0 -> 32.7 at two
queues). The flag is driven through the SIOCETHTOOL ioctl directly; no
ethtool binary involved.

The sweep example now closes its device from a signal handler: the
harness kills it mid-window, a signal skips deferred calls, and the leak
put whatever the backend had tuned - NAPI settings then, the mpwqe flag
now - into every arm that followed. That is the mechanism behind the
"leaked NAPI settings" measurement trap this project had recorded twice.

**mlx5: forwarding doubles - the receive ring prefetches every frame it
hands out, and forwarders get the copied-descriptor escape at four
queues.** Eight forwarding workers went from 68.5 to **140.8 Mpps** on a
ConnectX-6 Dx, past the DPDK backend's 126.5 on the same card, with no
options - the ladder is 29.9 / 59.1 / 67.5 / 87.6 / 101.1 / 140.8 on
1/2/3/4/6/8 cores. Two mechanisms, both found by measurement:

- *The frames were cache-cold.* Inbound DMA lands in memory on hosts
  without a cache-placement path (this EPYC among them), so a forwarder's
  first read of each arriving frame stalled for the full memory latency,
  packet after packet - `perf` showed 37% of all cycles inside the
  16-instruction header parse, instructions per packet flat while cycles
  tripled with worker count. DPDK's PMD and VPP's rdma plugin both hide
  that latency with software prefetch; the receive ring now does the
  same - one `PREFETCHT0` per completion consumed, so the walk itself
  paces the hints and the misses overlap. The control: the same hint added
  to the DPDK backend's loop moved it 2%, because its PMD already does
  this. Receive-only is unregressed (44.1 / 144.7 Mpps at 1 / 8 queues).
- *The ~76 Mpps pointer wall governs forwarders too.* Every pointer-mode
  forwarding ladder capped near 70 regardless of workers; the drop
  accounting put the loss at the full transmit ring, not the parse. The
  auto-copy default (copied entries, four rings per queue) now extends to
  devices with receive queues from four transmit queues up - the measured
  crossover: at three queues copied ties pointer (64 vs 65), at four it
  wins (80.0 vs 69.8). One to three queues stay pointer, where they win.

The same prefetch was measured on the AF_XDP backend and does not
transfer: its forwarding is bounded by the kernel transmit drain (a third
of parsed packets die at a full tx ring), not by cold frames. The sweep
example grew the drop accounting (`DIAG taken/sent/parse_drop/tx_full`),
a `-prefetch` A/B flag, and `-completion-every`. This also settles the
DPDK backend's own forwarding shape - flat near 60 until eight queues,
126.5 at eight - as its PMD's `txqs_min_inline` switch, the same wall
escaped the same way.

**afxdp: receive-only no longer dies at scale - line rate on 8 queues.**
The backend's Fill posted descriptors but never woke a parked driver.
go-afxdp only issues the receive wakeup inside Poll, and the packetio
contract is Fill/Receive with no Poll, so with need-wakeup on, any queue
whose NAPI loop completed parked forever: instrumentation showed seven of
eight queues taking exactly their initial 1,024 frames and then nothing:
rx_kicks 0, fill rings provably full, 2.5 billion rx_out_of_buffer drops
in one window, queue 0 alone kept alive by stray kernel-bound traffic.
Forwarding masked it completely, because transmit's own wakeup drives the
same NAPI. The fix is three lines: Fill now checks NeedsWakeupRx (one
atomic load) and calls WakeupRx when the driver parked; rx_kicks and
rx_fill_ring_empty ride Stats().Backend so the next regression is visible
in counters. Measured before/after at 8 queues under a 148.8 Mpps flood:
11.5 → **148.8 Mpps, line rate**, and the full ladder is monotonic where
it previously fell from 37 to 5.9. go-afxdp's own drop example matches
the fixed numbers on the same arms, forwarding is unchanged, and XSK busy
polling measured worse and stays off. The receive sweep harness also now
sets the RSS indirection table (`ethtool -X equal N`) alongside `-L`,
reads both back, and never swallows ethtool's stderr - the silent-failure
trap the project's own notes had recorded twice.

**dpdk: the same treatment, the same day.** On a Mellanox PMD a
transmit-only device opened with two or more queues backs each `TxQueue`
with four hardware send queues - each ring a full unit of PMD queue,
mempool and frame slice, so ownership stays provable per ring and `Free`
routes every frame home by address. Measured with no options, one
goroutine per queue: 55.5 / 105.0 / **148.8 Mpps - line rate - on 1/2/3
cores**, where the old default needed sixteen. Non-Mellanox PMDs keep one
ring (no per-queue ceiling to escape); `WithRingsPerQueue` overrides;
receive and forwarding are untouched and re-measured unchanged;
conformance green on net_null, a veth and the real port.

**mlx5: one transmit queue is now worth one core, and line rate is the
default.** The card drains copied multi-packet entries at ~17 Mpps per send
queue - a quarter of what one core produces - so reaching 64-byte line rate
used to take benchmark folklore: sixteen queues, four per goroutine,
hand-rolled round-robin, and knowing to ask for `WithMultiPacket`. All of
that moved into the backend. A `TxQueue` may now be backed by several
hardware rings that `Transmit` deals batches over; a transmit-only device
opened with two or more queues switches to copied entries by itself and
backs each queue with four rings; and the region is sized from the queues
unless `WithFrames` pins it. Measured with no options, one goroutine per
queue: 67.2 / 100.5 / 147.1 / **148.8 Mpps - line rate - on 1/2/3/4 cores**,
`examples/blast -queues 4` included. Pointer mode, single-queue devices and
forwarders (devices with receive queues) are unchanged, re-validated:
conformance green on the real port, receive 46.2 and 146.5 Mpps on 1 and 8
queues, forwarding 29.7 and 58.4 on 1 and 2. `WithMultiPacket`,
`WithoutMultiPacket` and the new `WithRingsPerQueue` override every part of
the choice, and `Info.RingsPerQueue` reports it. DPDK measures the same
148.8 on 4 cores when a worker drives four of its queues; giving it the
same default is a known follow-up.

**The backends now spell the common options the same way.** The AF_XDP
backend named only `WithSteering` and `WithXDP`, so everything else had to be
written in go-afxdp's vocabulary and imported from that package:
`afxdp.WithXDP(xdp.WithQueues(4))` where the DPDK backend said
`dpdk.WithQueues(4)`. It now names `WithQueues`, `WithFrames`,
`WithFrameSize`, `WithAffinity` and `WithoutAffinity` itself, translating
each to its go-afxdp equivalent, and `mlx5` gains the `WithQueues` shorthand
the other backends already had. `WithXDP` remains the escape hatch for
AF_XDP-specific knobs, the way `WithDevArgs` is for DPDK.

No default changes, which is the point: a device opened with no options still
gets go-afxdp's automatic frame geometry, every queue bound, the driver's
interrupt tuning, and each worker placed beside its queue's interrupt. Checked
on the ConnectX rig - `afxdp.WithQueues(4)` and
`afxdp.WithXDP(xdp.WithQueues(4))` place workers on the identical processors
(0, 5, 7, 10), `WithAffinity(10,11)` lands exactly there, and
`WithoutAffinity()` places nothing.

Fixes from an external code review, with thanks. Two were real defects:

- **`Stats()` and `Err()` are now safe from a monitoring goroutine on every
  backend, and the interface says so.** dpdk, AF_PACKET and AF_XDP kept plain
  counters that the examples' own report loops were reading while the workers
  wrote them - a data race. Every value the two calls read is now atomic,
  updated once per batch, never per packet (the arrangement mlx5 already ran
  at line rate). That includes the gauges, which were easy to miss: dpdk's
  in-flight and outstanding counts, AF_PACKET's pending length, and mlx5's
  own `outstanding`, which had the same flaw in miniature. New tests drive a
  queue while another goroutine reads Stats, so the property is now enforced
  by `-race` on every CI run rather than by review.
- **A DPDK ownership violation now latches `ErrQueueFailed`.** An address
  from outside the region, or a free that did not fit the returned ring,
  means the backend can no longer prove where every frame is. The queue used
  to count the event and carry on with `Err() == nil` while quietly running
  short. It now fails permanently and refuses further work, which is the
  rule mlx5 already followed.

And the smaller corrections:

- `Capabilities.KernelCoexistence` says whether the kernel keeps its own
  interface to the device while it is open - true everywhere except DPDK on
  a vfio-pci device, and a property of the opened device, not the backend.
  The SteeringFilter and README steering docs now say which promise holds
  where.
- Truthful capabilities in the dpdk backend: `MaxQueues` is now the lower of
  the backend's ceiling and the device's own, and `ZeroCopy` is false on
  net_af_packet, which copies.
- The dpdk build tags now say `amd64`, which was already true of the cgo
  layer; arm64 can be added deliberately rather than failing mid-compile.
- DPDK frame division: when frames do not divide evenly by queues, the
  remainder is distributed to the first queues instead of the last few
  frames of the region belonging to nobody.
- Docs caught up with the fourth backend: "no copies" is now qualified
  (AF_PACKET copies by design, and says so), and dpdk appears in doc.go's
  backend list, the `Capabilities.Backend` values and the README steering
  section.

## v0.1.0

The first release. Nothing has been published before it, so there is nothing to
be compatible with; what follows is what the release contains and what was
verified, not a list of changes from a version anyone has.

### What it is

One API - `Device`, `TxQueue`, `RxQueue`, `Region`, `Desc` - over four
backends:

- **mlx5**: mlx5 Direct Verbs. The card's queue memory and doorbell are
  mapped into the process; there is no kernel in the packet path and no library
  call per packet. Needs a ConnectX or BlueField, cgo, and rdma-core.
- **afxdp**: AF_XDP over `github.com/atoonk/go-afxdp`. Works on any driver with
  XDP support.
- **dpdk**: a poll-mode driver running in this process, for cards with no
  native driver here. Not a DPDK application: the workers are Go goroutines,
  not lcores, and the only cgo crossings on the packet path are one burst in
  and one burst out. packetio's frame region *is* the driver's mempool, through
  a custom mempool operator, so nothing is copied and no mbuf is allocated per
  packet. Behind the `dpdk` build tag; needs libdpdk and hugepages.
- **afpacket**: TPACKET_V3 mmap ring in, batched `sendmmsg` out. Needs nothing
  and runs anywhere, including a laptop and a veth pair.

`SteeringFilter` says which packets a backend should take away from the kernel,
in one vocabulary that means the same thing on hardware flow rules and in an
eBPF program. A backend that cannot express a match refuses it at `Open` rather
than installing a wider one. AF_PACKET has no steering at all, because it is a
tap and the kernel sees every packet whatever a socket takes.

`dpdk/README.md` covers the DPDK backend's setup and its device-ownership
model.

`BACKENDS.md` is the contract a backend must honour. `internal/conform` is that
contract as a runnable suite; `PACKETIO_CONFORM_REQUIRED=1` turns "cannot open a
device" from a skip into a failure, so a backend whose opener is broken says so
instead of quietly reporting nothing.

### Measured

On a ConnectX-6 Dx at 100 Gbit/s, 64-byte frames unless stated.

| | |
| --- | --- |
| mlx5 transmit | 66 Mpps a core, 72.6 cycles a packet, one queue; line rate on four cores with sixteen copied-mode queues |
| mlx5 receive | 44.1 Mpps a core; 147.2 Mpps on eight cores, within 1% of line rate |
| afxdp transmit | 8.5 Mpps a core |
| afpacket transmit | 0.96 Mpps a socket, measured once |
| dpdk on mlx5, transmit | 57.2 Mpps a core, one queue |
| dpdk on mlx5, receive | 46.3 Mpps a core; line rate on eight cores |
| dpdk on ixgbe (X550), transmit | 13.8 Mpps, near the 10G line rate |

Receive cost depends heavily on offered load - 968 cycles a packet at 5 Mpps
offered against 66 at line rate - so every receive figure in this repository
records the conditions it was taken under. Quote cycles a packet, not a rate:
the same binary measures 132 to 147 Mpps on this machine depending on which
cores the scheduler boosts.

### The transmit descriptor change of 28 August

The mlx5 transmit path originally copied every packet into its multi-packet
descriptor. That reasoning - sparing the card a fetch - measured wrong: the
card drains pointer descriptors three times faster, and DPDK's own driver on
the same port proved it. Multi-packet descriptors now point at their packets
by default, which took one queue on one core from 16.8 to 66.3 Mpps.

The card has two distinct ceilings, both measured and verified: pointer descriptors stop at ~76 Mpps for the
whole device (a per-packet gather limit - the same at 128-byte frames), copied
ones at ~17 Mpps per queue but multiplying with queue count. So the default is
pointers, and small-frame line rate is the one configuration that wants
`WithMultiPacket` plus at least twelve queues - 148.8 Mpps on four cores,
reproduced on the current code.

### Known limits

- mlx5 receive does not reach line rate at any queue count; eight queues is the
  plateau and sixteen is worse.
- AF_XDP receive on a tagged interface needs the VLAN id registered with the
  kernel, or `rx-vlan-filter` off. The card drops an unregistered tag before the
  XDP program runs, and the symptom is zero packets with no error.
- AF_XDP steering cannot match a VLAN id or a destination MAC, and refuses
  rather than installing something wider.
- `Reclaim` on AF_XDP returns nothing: the backend cannot name the frames it
  reclaimed. `Capabilities().HandsBackFrames` says so.
- IPv6 steering on mlx5 is refused; the card can do it, nothing needed it yet.
- afpacket receive has never been benchmarked.
- The Intel X550 was recorded here as unable to receive under DPDK. That was
  wrong: it receives, and the earlier attempts were sending untagged frames on
  a link that carries tagged ones. Transmit and receive both work.
- DPDK completions arrive on the driver's schedule, not per batch: mlx5 asks
  for one only every 32 packets, so a smaller trailing batch stays with the
  driver until more is sent. `NumInFlight` reports it and it is not a leak.
- DPDK receive on a Mellanox card runs the scalar path: the vectorised one
  assumes a stock mempool and stalls on this one, so `rx_vec_en=0` is applied
  automatically.
- DPDK carries offload metadata outbound only - checksums and TCP segmentation
  on transmit - so `Capabilities().Offload` is false. There is no LRO.
- DPDK has no interrupt-driven `Poll`, no scatter, and no secondary processes.

### A note on the free list

`internal/pool` catches a frame returned twice only in a build tagged
`packetio_checked`, which the tests use. Always-on are the checks that matter
for memory safety and cost nothing: a frame outside the pool, an address that is
not a frame start, a push past capacity. The guard that stops a bad descriptor
reaching the card is in the mlx5 backend and is never compiled out. See
`internal/pool/check_off.go` for the measurement behind that split.
