# packetio
[![build and test](https://github.com/atoonk/packetio/actions/workflows/go.yml/badge.svg)](https://github.com/atoonk/packetio/actions/workflows/go.yml)

**A high-performance packet I/O library for Go.** One API over four ways of
reaching a NIC: mlx5 Direct Verbs for NVIDIA/Mellanox ConnectX and BlueField,
DPDK, AF_XDP, and AF_PACKET. You write the packet loop once and pick the
backend to suit the machine it lands on: a ConnectX card, an ordinary server,
a laptop. Changing backend is changing the import line.

```go
d, _ := mlx5.Open("eth0")          // or afxdp.Open, dpdk.Open, afpacket.Open
defer d.Close()

tx := d.TxQueue(0)
tx.SendFunc(64, func(i int, frame []byte) int {
        return copy(frame, myPacket)   // write straight into the NIC's memory
})
```

I built this because I have wanted it for a long time. Moving packets fast
from Go has always meant learning a pile of machinery first: DPDK and its
mempools, AF_XDP rings and UMEM, Direct Verbs queue setup, hugepages, memory
ownership. That machinery is capable, but the barrier to simply writing a fast
Go program that sends, receives or forwards packets is much higher than it
should be. This library went through many iterations and experiments before I
was happy with the abstraction; this is the version I wanted to open source.

The goal is to hide as much of that complexity as reasonably possible without
hiding the semantics that matter. Who owns a frame at every moment, how many
queues are open, what is steered to them, and what a given backend can and
cannot do all stay explicit. The backends are similar, not pretended to be
identical: `Capabilities()` says what you got, and a backend refuses what it
cannot honour rather than approximating it.

What is not traded away is performance. No per-packet allocation, no system
call, library call or cgo call per packet on the fast path, and no copies
anywhere the mechanism allows it: of the four backends only AF_PACKET copies,
because a packet socket is a copy. On a ConnectX-6 Dx, three cores transmit
64-byte line rate at 100G (about 148 Mpps) over mlx5 or DPDK, and it is a Go
program you `go build` like any other. The [Performance](#performance) section
has the full tables.

---

## Install

```bash
go get github.com/atoonk/packetio
```

Nothing else, if you use the AF_XDP or AF_PACKET backends. The mlx5 backend
needs rdma-core and a `-tags mlx5` build; DPDK needs libdpdk and a `-tags dpdk`
build. Each backend's README has the details.

## Quick start

Both programs below run as they are. They use AF_PACKET, which needs no special
hardware, so you can paste them on a laptop; **change the import and the `Open`
call and nothing else moves.** They are [`examples/hello`](examples/hello) in
two halves, and `sudo go run ./examples/hello -i eth0` runs the round trip as
one program.

### Send

```go
package main

import (
	"log"

	"github.com/atoonk/packetio/afpacket"
)

func main() {
	d, err := afpacket.Open("eth0")
	if err != nil {
		log.Fatal(err)
	}
	defer d.Close()

	pkt := myPacketBytes() // whatever you want on the wire

	tx := d.TxQueue(0)
	for {
		// One call is the whole cycle: reclaim finished frames, take fresh
		// ones, fill them, hand them to the NIC. 256 is the batch size.
		if _, err := tx.SendFunc(256, func(i int, frame []byte) int {
			return copy(frame, pkt)
		}); err != nil {
			log.Fatal(err)
		}
	}
}
```

### Receive

```go
package main

import (
	"fmt"
	"log"
	"time"

	"github.com/atoonk/packetio/afpacket"
)

func main() {
	d, err := afpacket.Open("eth0", afpacket.WithQueues(1))
	if err != nil {
		log.Fatal(err)
	}
	defer d.Close()

	rx := d.RxQueue(0)
	rx.Fill(rx.NumFreeFillSlots()) // give the NIC somewhere to put packets

	for {
		if _, err := rx.Poll(time.Second); err != nil {
			log.Fatal(err)
		}
		descs := rx.Receive(256)
		for _, d := range descs {
			pkt := rx.Region().Frame(d) // the bytes, where the NIC wrote them
			fmt.Println(len(pkt), "bytes")
		}
		rx.Recycle(descs)              // give the frames back
		rx.Fill(rx.NumFreeFillSlots()) // and re-arm
	}
}
```

That is the whole model: **a frame belongs to exactly one of three owners at a
time** (the pool, your program, or the NIC), and every method hands it between
them. `Alloc` → fill → `Transmit` → `Complete` going out; `Fill` → `Poll` →
`Receive` → `Recycle` coming in.

> **One gotcha worth knowing up front.** A bare `Open` gives you **one transmit
> queue and no receive queues** on mlx5 and dpdk, because a generator should not
> pay for receive memory it never uses. Ask for receive with `WithQueues(n)` or
> `WithRxQueues(n)`. AF_PACKET opens one of each.

## Which backend?

| | use it when | needs | one core sends |
| --- | --- | --- | ---: |
| **[mlx5](mlx5/)** | you have a ConnectX/BlueField card | rdma-core, `-tags mlx5` | **67.6 Mpps** |
| **[afxdp](afxdp/)** | any modern NIC, and you want to keep using it | a driver with XDP support | 18.8 Mpps |
| **[dpdk](dpdk/)** | Intel/Broadcom/virtio, or you need DPDK's drivers | libdpdk, `-tags dpdk`, usually hugepages | 56.2 Mpps |
| **[afpacket](afpacket/)** | it just has to run: a laptop, a VM, a container | nothing at all | 1.7 Mpps |

If you have an NVIDIA/Mellanox ConnectX card, use **mlx5**: it is the fastest
here and it costs you nothing operationally, because the kernel keeps the
interface. Otherwise start with **afxdp**, which is fast and leaves the NIC in
place. Reach for **dpdk** when AF_XDP is not enough or your card needs a DPDK
driver, and know that on non-ConnectX hardware it takes the NIC away from Linux
entirely. **afpacket** is the floor that always works.

Each backend's README covers what to install, what to run, and what bites.

```go
mlx5.Open("eth0")                              // -tags mlx5
afxdp.Open("eth0", afxdp.WithSteering(filter)) // AF_XDP always wants a filter
dpdk.Open("0000:c1:00.1")                      // -tags dpdk; PCI address or name
afpacket.Open("eth0")
```

## Steering: take the traffic you want, leave the rest

Steering is what makes this usable on a machine you are logged into. You name
the packets you want; the NIC or the kernel diverts exactly those to your
program, and **everything else carries on to the kernel**: your SSH session,
ARP, monitoring, all of it.

```go
d, err := mlx5.Open("eth0", mlx5.WithSteering(packetio.SteeringFilter{
        Match: append(
                []packetio.Match{packetio.MatchVLAN(2053)},
                packetio.MatchDstPort(packetio.IPProtoUDP, 9000)...),
}))
```

One vocabulary (destination MAC, VLAN, EtherType, IP protocol, source and
destination prefixes, TCP and UDP ports), compiled to whatever the backend has:
hardware flow rules on **mlx5** and **dpdk**, an eBPF program on **afxdp**.
**afpacket** has no steering because it cannot: it is a tap, the kernel sees
every packet anyway, and rather than offer something weaker under the same name
it offers nothing.

Repeated matches of one kind are alternatives, different kinds are ANDed: two
`MatchDstPort` and one `MatchVLAN` means "either port, on that VLAN".

A backend that cannot express a match **refuses it at `Open`** with
`ErrUnsupported` rather than installing a wider one. A filter that quietly
delivers more than you asked for is worse than no filter at all.

Whether unmatched traffic still reaches Linux is a property of the *device*, not
the filter: `Capabilities().KernelCoexistence` answers it. It is true
everywhere except DPDK on a device bound to `vfio-pci`, where there is no kernel
interface left to carry the rest.

## Performance

Every backend driven through the same three loops by the same program
([`examples/sweep`](examples/sweep)) on the same pair of machines: AMD EPYC
9275F, ConnectX-6 Dx at 100G, 64-byte frames on a tagged link. Rates are the
port's own counters; **cores are measured across the whole machine**, so the
soft-interrupt work AF_XDP and AF_PACKET do outside your process is counted
where it falls. One worker per queue, each backend placing its own workers.

**One core, one queue**, the number that says how efficient a backend is:

| | transmit | receive | forward |
| --- | ---: | ---: | ---: |
| **mlx5** | **67.6 Mpps** | 44.1 Mpps | **29.7 Mpps** |
| **dpdk** | 56.2 Mpps | **46.1 Mpps** | 23.1 Mpps |
| **afxdp** | 18.8 Mpps | 32.3 Mpps (1.6 cores) | 17.6 Mpps (2 cores) |
| **afpacket** | 1.7 Mpps | 0.04 Mpps (41 cores) | 0.03 Mpps (41 cores) |

**Scaling to 100G line rate** (148.8 Mpps at 64 bytes; medians of three
passes). One worker per queue, one queue per core, *default configuration
throughout*: no flags, no tuning. AF_XDP cells show whole-machine cores
with the soft-interrupt share in parentheses, because that is where the
kernel's half of its work lives; the bypass backends run 0.0 softirq.

**Transmit:**

| cores | mlx5 | dpdk | afxdp: Mpps on cores (softirq) |
| ---: | ---: | ---: | ---: |
| 1 | 67.6 | 56.2 | 18.8 on 1.0 (0.5) |
| 2 | 97.8 | 106.7 | 35.9 on 2.0 (1.0) |
| 3 | **147.9** | **148.8** | 53.6 on 3.0 (1.4) |
| 4 | - | - | 71.1 on 4.0 (2.1) |
| 6 | - | - | 90.1 on 6.0 (4.1) |
| 8 | - | - | 120.3 on 8.0 (5.5) |
| 12 | - | - | 133.6 on 12.0 (7.5) |
| 16 | - | - | **147.3** on 16.0 (9.6) |

**Receive**, offered 148.8:

| cores | mlx5 | dpdk | afxdp: Mpps on cores (softirq) |
| ---: | ---: | ---: | ---: |
| 1 | 44.1 | 46.1 | 32.3 on 1.6 (1.0) |
| 2 | 81.2 | 86.5 | 65.1 on 3.2 (2.0) |
| 3 | 84.5 | 89.3 | - |
| 4 | 117.0 | 124.8 | 128.3 on 6.5 (4.0) |
| 6 | 124.0 | 111.5 | - |
| 8 | 144.8 | **148.8** | **148.8** on 11.7 (7.7) |
| 10 | **148.6** | - | - |
| 16 | - | - | 140.4 on 11.6 (7.2) |

**Forwarding**, offered 148.8, next hop nobody owns:

| cores | mlx5 | dpdk | afxdp: Mpps on cores (softirq) |
| ---: | ---: | ---: | ---: |
| 1 | 29.7 | 23.1 | 17.6 on 2.0 (1.0) |
| 2 | 58.7 | 44.9 | 32.6 on 4.0 (2.0) |
| 3 | 61.2 | 62.4 | - |
| 4 | 87.8 | 74.5 | 52.9 on 8.0 (4.0) |
| 6 | 100.2 | 61.1 | - |
| 8 | 140.1 | 126.3 | 77.2 on 16.0 (8.0) |
| 10 | 144.3 | - | - |
| 12 | **147.5** | 97.1 | - |
| 16 | - | - | 112.9 on 23.2 (10.1) |

**Both bypass backends transmit line rate on three cores; mlx5 receives it
on ten and forwards it on twelve; DPDK receives it on eight.** AF_XDP
transmits line rate on sixteen queues, receives it on eight (11.7 machine
cores: a worker and most of a soft interrupt per queue), and forwards
about three quarters of it on sixteen workers spending 23 machine cores;
its run-to-run spread is honest at ±15%, the widest of the three.

A few things worth knowing behind those numbers:

- **Everything here is the same trade wearing different clothes**: pointer
  descriptors are cheap per packet but drain through one ~75 Mpps
  device-wide door; copied descriptors cost more per packet and scale per
  queue to the wire. The mlx5 backend switches modes by queue count
  (transmit-only at two, forwarders at four), DPDK's PMD switches at eight
  send queues (its forwarding dip at six and jump at eight is exactly
  that), and since go-afxdp v0.11.0 even the kernel's XDP path is switched
  the same way: small fleets run pointer descriptors, which is where the
  AF_XDP 1-4 queue transmit numbers come from.
- **Forwarding is memory-latency work on hosts whose inbound DMA bypasses
  the cache** (this EPYC among them): the first read of every arriving
  frame used to stall a full memory round-trip, capping mlx5 forwarding
  near 70 Mpps however many cores it got. The receive ring now prefetches
  each frame as its completion is consumed. One instruction, half the
  gap; the descriptor-mode switch above is the other half.
- **AF_XDP's kernel half is real work on real cores.** At line-rate
  receive, 7.7 of the 11.7 cores are soft interrupt: the driver's NAPI
  poll and the XDP redirect. Counting only your process would call that
  4 cores; the table counts the machine.
- **The receive flat spot at three workers** (both bypass backends: ~84-89
  Mpps at 3, barely above 2) reproduces across passes and days and is
  still unexplained; it disappears by four workers.
- **These receive figures are 64-byte frames, and that matters more than it
  should.** At 68 bytes, the smallest a VLAN-tagged frame may be, receive
  tops out near 130 Mpps on this card and never reaches the 142 Mpps line
  rate, in every stack tested including DPDK's. Four bytes on the wire, and
  a ceiling appears. Transmit is unaffected, and 128/512/1500-byte receive
  takes everything offered. The cause was chased through every
  layer software controls and pinned to the card's receive pipeline:
  the cliff sits at a 61-byte payload, in every stack.
- **The forwarding ceilings above are real, not tuning artifacts.** DPDK
  was re-run with the PMD's inline knobs forced every way they go
  (`txqs_min_inline=1`, `txq_inline_mpw=64`, both): ~127 at eight queues
  every time, worse elsewhere. AF_XDP was re-run with NAPI flush, ring
  depth and pool variants: all inside the noise of its default. The
  numbers each stack ships with are the numbers it has.


## Examples

Start with [`hello`](examples/hello): about a hundred lines, both directions,
runs on any interface on any Linux box.

```bash
sudo go run ./examples/hello -i eth0
```

| example | what it shows | backend |
| --- | --- | --- |
| [`hello`](examples/hello) | **the whole API in one file: start here** | any |
| [`send`](examples/send) | one frame, and what the hardware said about it | mlx5 |
| [`blast`](examples/blast) | a generator with rate control and CPU placement | mlx5 |
| [`drop`](examples/drop) | the receive cycle, and what it costs | mlx5 |
| [`steer`](examples/steer) | ask for two UDP ports, watch only those arrive | mlx5 |
| [`l3fwd`](examples/l3fwd) | an IPv4 router forwarding in the frame it arrived in | mlx5 |
| [`info`](examples/info) | what a card says about itself | mlx5 |
| [`dpdk/info`](examples/dpdk/info) | the driver, who owns the device, which offloads are real | dpdk |
| [`dpdk/blast`](examples/dpdk/blast) | the generator, over DPDK | dpdk |
| [`dpdk/drop`](examples/dpdk/drop) | the receive cycle with the NIC's own counters | dpdk |
| [`dpdk/l3fwd`](examples/dpdk/l3fwd) | the same router, sharing its forwarding code | dpdk |
| [`timestamps`](examples/timestamps) | when packets really arrived, and the gaps between them | any |
| [`pingpong`](examples/pingpong) | round trips between two machines, split by where the time went | mlx5 |
| [`sweep`](examples/sweep) | every backend through the same three loops; the Performance tables above | mlx5, dpdk, afxdp |
| [`netstack/examples/tcpecho`](netstack/examples/tcpecho) | a TCP echo server, and client, on a userspace TCP/IP stack over the device | any |

```bash
sudo go run -tags mlx5 ./examples/steer -i eth0 -udp-port 9000 -udp-port 9001
```

There is no separate AF_XDP example because none is needed: the loop in any of
these runs on it unchanged once you open with a steering filter, which is two
lines shown in its [README](afxdp/). AF_XDP-specific tooling lives in
[go-afxdp](https://github.com/atoonk/go-afxdp).

## TCP on top: netstack

Fast receive and transmit are only useful if something can speak the
protocols on them. [`netstack/`](netstack) runs gVisor's TCP/IP stack over any
packetio device and hands back `net.Listener` and `net.Conn`, so a TCP server
-- or a client, or a load balancer that terminates connections -- runs in
userspace with the kernel nowhere on the path. It is a separate Go module, so
programs that only move frames do not carry gVisor, and the gVisor it carries
is a patched one (`netstack/gvisor`, go-afxdp's ten patches on a pinned
upstream commit, the stack that produced its numbers). Its README says which
address to give the stack on each backend, which is the one thing that
differs between them.

## Timestamps: how long you held a packet

The device records when each packet arrived, before your program is involved.
Ask for those times and you can measure two things you otherwise cannot: **how
long packets spend inside your program**, and **how evenly traffic is
arriving**.

```go
if rx, ok := d.RxQueue(0).(packetio.TimestampReceiver); ok && d.Capabilities().RxTimestamps {
        descs, ts := rx.ReceiveTimestamps(256)   // ts[i] is when descs[i] arrived
}
```

That is the whole difference from an ordinary receive loop: one type
assertion, and `Receive` becomes `ReceiveTimestamps`.

**Why not just call `time.Now()` in the loop?** Because that measures the
loop. If your program is busy for a millisecond, the fifty packets that
arrived during it all get read at the same instant and look simultaneous. The
card stamped each one as it came off the wire, before the transfer to memory,
before the completion, before any of your code ran, so those stamps stay true
whatever your program was doing.

Nothing is written into the packet. The time goes in the completion the card
writes beside it, so the sender neither cooperates nor notices, and traffic
from anyone can be measured.

### What you can and cannot learn from it

You get one fixed reference point per packet: the moment it reached the port.
Both ends of any interval you build from it are on **your** machine.

    forwarding residency   arrival stamp  ->  when you hand it to transmit
    your receive path      arrival stamp  ->  when your code first sees it
    arrival jitter         one packet's stamp -> the next one's

What you cannot get is how long a packet was **on the wire**, or a one-way
delay from some sender to you. Both need a timestamp taken when the packet was
*sent*, and no such thing travels in the packet. Those need two clocks
disciplined to a common source, which this package does not do.

### What it costs

Nothing, unless you ask. `Receive` never reads the field, and no offload is
switched on at the device: the card writes the timestamp into every completion
whether or not anybody reads it. Measured against the build before the
feature, receive was 44.19 -> 44.17 Mpps on one queue and 144.14 -> 143.97 on
eight, both inside run-to-run noise.

### Two things to know

**The slices belong to the queue.** `descs` and `ts` are overwritten by the
next receive call. Copy anything you mean to keep.

**On a link with offloads on, one timestamp can cover many packets.** The
kernel coalesces received segments into one super-frame (GRO) and stamps that,
at the moment it coalesced rather than when each segment arrived. A stream that
would have been forty-odd samples becomes one, with a skew nobody sees in the
numbers. Turn offloads off on the link you are measuring, or measure something
that is not being coalesced.

**Arrival order is not delivery order.** A card stamps at the port and places
the packet in a queue afterwards, so while it is dropping traffic the two come
apart: at rates it keeps up with, stamps rise packet by packet (2 out of 9.1
million out of order), but offer 148 Mpps to a queue that can take 44 and about
a third arrive out of stamp order. Sort if you need order.

| backend | stamps with | resolution | epoch |
| --- | --- | --- | --- |
| **mlx5** | the card, at the port | 4 ns | the device's own, meaningless on its own |
| **afpacket** | the kernel, filling the ring | nanoseconds | CLOCK_REALTIME, so it can step |
| **afxdp**, **dpdk** | not yet | | |

### What it looks like in practice

[`examples/pingpong`](examples/pingpong) times round trips between two
machines and uses the stamps to say where the time went. Measured back to back
on ConnectX-6 Dx, one queue, one frame in flight, no tuning of any kind:

| frame | round trip (median) | of which, this program's receive path |
| --- | ---: | ---: |
| 64 B | 5.58 us | 0.49 us |
| 1500 B | 6.32 us | 0.42 us |

A round trip is a **software** number: it covers both machines' send and
receive paths, and on a back-to-back cable the wire is tens of nanoseconds. It
is not a network measurement, and the far end here is packetio too, so a full
reflector cycle is inside every figure. What the stamp adds is the second
column: without it there is only "5.58", and no way to say whose microseconds
those were.

`Capabilities().RxTimestamps` is the authority on whether a device really
stamps. A queue may carry the method without the device having a clock, and
then `ReceiveTimestamps` returns nothing rather than inventing zeroes: a zero
would be a claim that a packet arrived at the epoch, and nothing downstream
could tell that from a real reading.
[`examples/timestamps`](examples/timestamps) is a working jitter meter in about
a hundred lines, and it runs on any Linux box.

## Offload

`Offload` carries segmentation and checksum metadata alongside a frame, so a
64 KB TCP super-frame crosses the device whole and is cut up by the kernel or a
virtio peer instead of by you. It is `virtio_net_hdr` field for field, the
common currency of `PACKET_VNET_HDR`, vhost-user, tap and memif, and it rides
optional interfaces:

```go
if r, ok := rq.(packetio.OffloadReceiver); ok {
        descs, offs := r.ReceiveOffload(64)
}
```

A 64 KB packet does not need a 64 KB frame. Where `Capabilities().MultiBuffer`
says so, one packet may lie across several descriptors, each but the last
marked `OptContinued` - the AF_XDP convention. It goes both ways: hand
`Transmit` a chain and the device gathers it, and on receive you get the same
shape back. That lets a forwarder keep small frames and still carry
segmentation-offloaded traffic, instead of sizing every frame for the largest
packet it will ever see.

```go
for _, d := range rq.Receive(64) {
        if d.Options&packetio.OptContinued != 0 {
                // more of this packet follows
        }
}
```

If your packets already live in your own memory, `GatherTransmitter` skips the
region entirely and sends from your slices.

## What is here

    packetio               Desc, Region, TxQueue, RxQueue, Device: the API
    match.go               SteeringFilter and Match: what to steer
    offload.go             virtio_net_hdr: segmentation and checksum metadata
    internal/pool          the free-frame list, one per queue per direction
    internal/conform       the contract in BACKENDS.md, as a test every backend runs

    mlx5                   Direct Verbs; see mlx5/README.md
    afxdp                  AF_XDP over go-afxdp; see afxdp/README.md
    dpdk                   a poll-mode driver in-process; see dpdk/README.md
    afpacket               TPACKET_V3 and sendmmsg; see afpacket/README.md

[BACKENDS.md](BACKENDS.md) is the contract a backend must honour;
`internal/conform` is that contract as a runnable suite.

## Testing without a NIC

```bash
go test ./...
go test -race ./...
```

`mlx5/internal/mocknic` reads the same doorbell records, parses the same work
queue entries and writes the same completions as real hardware, over the same
memory: the ring under test cannot tell the difference. The suite was checked
by breaking the driver on purpose (publishing a doorbell index in the wrong byte
order, releasing a frame one slot too far, believing a completion that names the
wrong buffer) and confirming each is caught.

The conformance suite runs against afpacket on a veth pair, so it needs no
hardware at all.

## Requirements

Linux, amd64 or arm64 (the dpdk backend is amd64 only). `CAP_NET_RAW`
everywhere. mlx5 additionally needs rdma-core and a memory lock limit large
enough for the frame region; dpdk needs libdpdk and, on most cards, hugepages
and an IOMMU.

## License

Apache 2.0. See [LICENSE](LICENSE).
