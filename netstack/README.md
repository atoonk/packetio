# netstack - a TCP/IP stack on packetio

gVisor's TCP/IP stack ([netstack](https://pkg.go.dev/gvisor.dev/gvisor/pkg/tcpip)),
with go-afxdp's performance patches, over any packetio device. Packets come off the wire through packetio, the
stack turns them into `net.Conn`, and the kernel is nowhere on the path.
`Listen` and `Dial` give `net.Listener` and `net.Conn`, so what runs on top
is ordinary Go: an echo server, an HTTP handler, a load balancer that
terminates TCP.

```go
dev, _ := afpacket.Open("eth1")
st, _ := netstack.New(dev, netstack.Config{Addr: netip.MustParsePrefix("10.0.0.2/24"), MAC: mac})
ln, _ := st.Listen("tcp", ":8080")
for { c, _ := ln.Accept(); go io.Copy(c, c) }
```

The same program runs on **afxdp, dpdk, mlx5 and afpacket**; the backend is
the `Open` call. [`examples/tcpecho`](examples/tcpecho) is that program with
a `-backend` flag and a client mode.

This is a separate Go module (`github.com/atoonk/packetio/netstack`), because
gVisor is a large dependency with a newer Go requirement than packetio's, and
a program that only moves frames should not carry it. The gVisor it uses is
in the module, under [`gvisor/`](gvisor): upstream at a pinned commit plus
a set of patches, trimmed to the packages the stack needs (see below).

```bash
cd netstack
go build ./...                                    # needs Go 1.26 (go.mod says; the go command fetches it)
sudo go run ./examples/tcpecho -backend afpacket -i eth1 -ip 10.0.0.2/24
```

## What it does

- **ARP, IPv4, TCP** (CUBIC) from gVisor. IPv6 and UDP are not wired up yet.
- **Ethernet and 802.1Q** in the endpoint, with a MAC table the endpoint
  learns from the peers that talk to it (see "Who the stack trusts").
- **GSO**: TCP hands the endpoint one packet of up to 64 KiB, and the endpoint
  splits it into MSS-sized frames in place, straight into the device's frame
  memory. One trip through the stack for up to 45 frames. On by default.
- **GRO**: a burst of one flow's segments is merged into one packet before
  the stack sees it. On by default (`Config.NoGRO` turns it off); without it
  one TCP goroutine cannot keep up with a peer at line rate. The endpoint
  verifies each frame's checksums first.
- **Checksums in software**, on every backend, in v1. Where a backend can have
  the NIC finish them (`packetio.OffloadTransmitter`) that is a later addition
  inside the endpoint; nothing in the API changes for it.
- **One goroutine per queue**, as packetio requires: a receive goroutine per
  receive queue delivers into the stack, and gVisor's `fifo` queueing
  discipline gives each transmit queue exactly one writer, in batches. Your
  own goroutines never touch the device. That receive goroutine also *runs*
  TCP for the connections whose segments it delivered, and with `DirectTx`
  transmits their replies, so work you do inline in a read loop is work the
  stack is not doing; see the package documentation.

Where the bytes go: gVisor owns every byte the stack touches, so a received
frame is copied once into a pooled view and goes straight back to the ring,
and a transmitted packet is copied once into one of the device's frames. One
copy each way is the floor gVisor's API sets; everything else the backend buys
is kept - no per-packet syscalls, batched rings, no sk_buff.

## The address, and who answers ARP

The stack answers ARP for its own address and asks for its peers'. Whether it
ever *sees* an ARP frame depends on the backend, and so does which address to
give it:

| backend | the kernel sees | ARP is answered by | address to use |
|---|---|---|---|
| **afpacket** | everything (it is a tap) | the stack | one the kernel does **not** have on the interface. A kernel-owned address gets a kernel RST for every SYN. |
| **afxdp** | what the filter leaves | the kernel, for its own address; the stack, if ARP is steered too | the interface's own (steer the TCP port; the endpoint learns each peer's MAC from its first frame), or a spare one with `afxdp.WithXDP(xdp.WithFilter(xdp.MatchEtherType(0x0806), xdp.MatchTCPPort(port)))` |
| **mlx5** | what the filter leaves | the stack, when it takes a whole VLAN; the kernel otherwise | on a tagged link, give the stack the VLAN: `MatchVLAN(id)` + `MatchDstMAC(own)` + `MatchDstMAC(broadcast)` takes every frame on it, ARP included, and the stack owns its address there (verified on a ConnectX-6 Dx). On an untagged link, the interface's own address and `MatchDstPort(TCP, port)`, the kernel answering ARP |
| **dpdk**, device bound to vfio-pci (EC2's ENA) | nothing - there is no kernel interface | the stack | the device's own address |
| **dpdk**, bifurcated card or `net_af_packet` vdev | as mlx5 / as afpacket | as mlx5 / as afpacket | as mlx5 / as afpacket |

A stack that **dials** needs the replies steered to it: by the peer's port
(`MatchSrcPort`), or all of TCP. `tcpecho -client` does that.

A packetio `SteeringFilter` is AND across match kinds, so "ARP *or* TCP port"
is not one filter. Two destination MACs are one kind, though, and "everything
to me or to broadcast on this VLAN" is what a stack that owns a VLAN needs;
on afxdp the go-afxdp filter above ORs its matches. `Config.Neighbors` seeds
the MAC table with a peer the stack must reach before hearing from it (a
gateway, say); such an entry is pinned.

## Config

```go
type Config struct {
    Addr       netip.Prefix                     // required: the address and its on-link prefix
    MAC        net.HardwareAddr                 // required: the interface's, or the device's (Info().MAC on dpdk/mlx5)
    VLAN       uint16                           // 802.1Q tag on every frame, or 0
    MTU        int                              // 0 = 1500, or what the frame allows
    Gateway    netip.Addr                       // next hop off the prefix; unset = everything on-link
    Neighbors  map[netip.Addr]net.HardwareAddr  // pinned MAC table entries: the gateway on a shared link
    NoGSO      bool                             // one packet per MSS instead of in-place segmentation
    NoGRO      bool                             // do not merge arriving segments before the stack sees them
    TxQueueLen int                              // packets waiting per transmit goroutine; 0 = 1000
    DirectTx   bool                             // transmit from the producing goroutine, no transmit goroutines
    SendBufferSize, ReceiveBufferSize int       // TCP defaults; 0 = 4 MiB
    BusyPoll   bool                             // spin when idle on a backend that cannot sleep (dpdk, mlx5)
    FastClock  bool                             // runtime nanotime instead of time.Now for TCP's clock
    Logf       func(string, ...any)             // where the endpoint's few messages go; nil = log.Printf
}
```

`New` brings the stack up on the device's queues, one stack per device;
`Close` resets any connection still open, stops everything, and leaves the
device open, since whoever opened it closes it. `Stats()` is the endpoint's
counters (frames in and out, GSO packets and frames, every kind of drop by
name); `TCPIP()` is the gVisor stack underneath, for its own stats and
anything this package does not name. A queue that fails (the interface went
away) is reported by `Err()` and `Done()`; the stack does not stop on its
own, and a server should treat `Done` as its cue to.

### Who the stack trusts

The MAC table is learned the way ARP is, without authentication: a host on
the link can send a frame from another's address and be believed. What the
endpoint does about that is bound it. It learns only from frames sent to its
own MAC and address by a source on its prefix, only after the checksums
verify, never over an entry from `Config.Neighbors` or `AddNeighbor`
(those are pinned), and a change or an eviction is throttled to one per 10
ms. On a link shared with untrusted hosts, put the gateway in `Neighbors`.
Fragments are not accepted at all: TCP never sends them.

## The example

```bash
cd netstack
sudo go run ./examples/tcpecho -backend afpacket -i eth1 -ip 10.0.0.2/24
sudo go run ./examples/tcpecho -backend afxdp    -i eth1 -ip 10.0.0.2/24 -port 8080
sudo go run -tags dpdk ./examples/tcpecho -backend dpdk -i 0000:00:06.0 -ip 10.0.2.132/24 -gw 10.0.2.1
sudo go run -tags mlx5 ./examples/tcpecho -backend mlx5 -i eno2 -vlan 2043 -ip 192.168.43.9/24

# from another machine
nc 10.0.0.2 8080

# or the same program as a client on its own stack: sends -bytes, checks the echo, prints the rate
sudo go run ./examples/tcpecho -backend afpacket -i eth1 -ip 10.0.0.3/24 -client 10.0.0.2:8080
```

`-mode sink` reads and discards, `-mode source` writes until the peer hangs
up, for one-way measurements. `-generic` puts afxdp in generic XDP mode, for
a NIC or veth without native XDP support (generic XDP hands the frame over
with its VLAN tag stripped, so `-vlan` cannot work there; afpacket puts the
tag back and can). `-no-hugepages 256` runs dpdk's `net_af_packet0,iface=eth1`
vdev on ordinary memory; `-busypoll` keeps the receive goroutines spinning
on dpdk and mlx5. `-stats 1s` prints counters, every receive-side drop
reason among them: a stack that hears nothing says why. On afpacket the
example refuses an address the kernel owns on the interface (`-force`
overrides), since the kernel would reset every connection.

**On one machine**, with no second host: a veth pair, the far end in a
namespace as the peer, the stack on the near end with an address the kernel
does not have.

```bash
sudo ip link add ns0a type veth peer name ns0b
sudo ip netns add peer && sudo ip link set ns0b netns peer
sudo ip link set ns0a up
sudo ip netns exec peer ip addr add 10.0.0.1/24 dev ns0b
sudo ip netns exec peer ip link set ns0b up
sudo ip netns exec peer ethtool -K ns0b tx off tso off gso off   # a veth hands over unfilled checksums otherwise

sudo go run ./examples/tcpecho -backend afpacket -i ns0a -ip 10.0.0.2/24      # near end: unnumbered
sudo ip netns exec peer nc 10.0.0.2 8080                                       # from the namespace

sudo ip link del ns0a && sudo ip netns del peer
```

## Tests

```bash
cd netstack
go test ./...            # the frame, GSO, GRO and stack logic, on an in-memory
                        # device two stacks talk to each other over; no root

# the stack over a veth pair on each backend: a kernel peer in a namespace,
# 200 round trips byte-exact, 4 MiB both ways at once (GSO), and the stack as a
# client dialling a kernel listener (ARP)
sudo -E env "PATH=$PATH" PACKETIO_VETH_TESTS=1 go test -race -count=1 ./...            # afpacket, afxdp
sudo -E env "PATH=$PATH" PACKETIO_VETH_TESTS=1 go test -tags dpdk -count=1 -run /dpdk ./...

# mlx5 needs a ConnectX and a peer that echoes (socat TCP-LISTEN:9090,fork,reuseaddr EXEC:cat)
sudo -E env "PATH=$PATH" PACKETIO_IFACE=eno2 PACKETIO_NETSTACK_VLAN=2043 \
  PACKETIO_NETSTACK_ADDR=192.168.43.1/24 PACKETIO_NETSTACK_PEER=192.168.43.2:9090 \
  go test -tags mlx5 -count=1 -run TestRigDial ./...
```

`PACKETIO_CONFORM_REQUIRED=1` turns "cannot open a device" from a skip into a
failure, as in the rest of the repository. The veth tests need root and the
explicit opt-in, and never touch an interface that already exists.

## Measured against the kernel

The question this module exists to answer: once a packet is off the wire at
line rate, can a userspace TCP stack use that speed? Two rigs, the same load
generator (go-afxdp's, kernel sockets, on the peer box), the same server
program (`tcpecho`, one stack over all queues, GRO and GSO on, software
checksums), 20 s runs, CPU as whole-machine busy cores over an 8 s
window. The kernel rows are the kernel's own stack on the same interface with
the same generator. Read the caveats below the tables before the numbers.

**100G, ConnectX-6 Dx, VLAN 2043, two 48-thread boxes** (sao-1 serves; "fork"
is the patched gVisor this module now ships, "upstream" the same commit
without the patches, which was the default at the time)

| workload | kernel | cores | afxdp, upstream gVisor | cores | afxdp, fork | cores | mlx5, upstream (16 q) | mlx5, fork |
|---|--:|--:|--:|--:|--:|--:|--:|--:|
| request/response 64 B, 64 conns | 349k req/s, p50 60 µs | 14.3 | 338k, 120 µs | 22.0 | **388k, 90 µs** (399k direct tx) | 19 | 218k, 160 µs | 277k, 120 µs |
| request/response, 1024 conns | 1.21M, 770 µs | 33.1 | 606k, 1.4 ms | 35.5 | 962k, 890 µs | — | 892k, 980 µs | **1.14M, 760 µs** |
| receive (`-mode sink`), 16 flows | 93.8 Gbit/s | 9.9 | 55.3 | 31.8 | 52.2 | 25.3 | 49.4 | — |
| transmit (`-mode source`), 16 flows | 93.8 Gbit/s | 2.7 | 84.6 | 37.3 | **93.6** | 15.0 | 29.0 † | 57.7 † |
| bidirectional echo, 1 flow | 26.9 Gbit/s | 3.3 | 7.6 | 4.3 | 8.6 | 3.4 | 1.9 † | — |
| bidirectional echo, 16 flows | 92.7 Gbit/s | 13.2 | 25.3 † | 16.5 | 54.2 | 32.0 | 16.7 † | 44.2 |

**EC2 c6i.xlarge (4 vCPU), ENA, 10.0.2.0/24** (dpdk on vfio-pci; the kernel gone)

| workload | kernel | cores | dpdk, upstream gVisor | cores | dpdk, fork | cores |
|---|--:|--:|--:|--:|--:|--:|
| request/response 64 B, 64 conns | 223k, p50 270 µs | 3.4 | 105k, 610 µs | 2.6 | 125k, 480 µs (**166k, 360 µs** direct tx) | 3.0 |
| request/response, 256 conns | 315k, 700 µs | 3.8 | 133k, 1.9 ms | 3.0 | 158k, 1.6 ms | 3.0 |
| receive (`sink`), 4 flows | 12.4 Gbit/s | 2.0 | 7.7 | 1.9 | 8.1 | 1.8 |
| transmit (`source`), 4 flows | 12.4 Gbit/s | 0.9 | 11.1 † | 2.5 | 11.1 † | 2.3 |
| bidirectional echo, 4 flows | 12.4 Gbit/s | 2.8 | 7.0 | 2.7 | 6.5 | 2.4 |
| bidirectional echo, 1 flow | 5.0 Gbit/s | 1.0 | 0.05 † | 0.7 | 4.5 | 2.0 |

† rows with retransmissions and retransmit timeouts on the netstack side.

What the numbers say, and what they do not:

- **With the fork, request/response beats the kernel at 64 connections on
  afxdp** (388k vs 349k req/s, 90 vs 60 µs) and comes within 6% of it at
  1024 on mlx5 (1.14M vs 1.21M); one-way transmit is at line rate (93.6 of
  93.8 Gbit/s); the single-flow echo stall is gone (0.05 → 4.5 Gbit/s on the
  ENA). Upstream gVisor through the fifo discipline pays a handoff to a TCP
  processor goroutine per segment and another to the transmit goroutine; the
  fork's inline processing removes the first and `Config.DirectTx` the
  second (166k vs 125k req/s on the ENA). What remains is one stack over all
  queues and software checksums; go-afxdp's shared-nothing layout got 1.3M
  req/s on this rig.
- **GRO is not optional.** Without it a peer at line rate sends one segment
  per stack trip and the TCP goroutine falls behind: dpdk bulk went from 2.6
  to 6.3 Gbit/s on the ENA by turning it on. It is on by default now.
- **Bidirectional bulk stalls on upstream gVisor.** A single echo flow runs
  at a fraction of one-way speed with retransmit timeouts at both ends;
  go-afxdp's history records the same (0.46 Gbit/s) until its patch to keep
  processing ACKs while the receive window is closed. One-way transfers
  do not stall: 84.6 Gbit/s transmit on afxdp with ten retransmits.
- **The mlx5 rows carry losses the afxdp rows do not** (78k retransmits on
  16-flow transmit, 1.8k on a single echo flow) and cost more cores: each
  of the 16 receive goroutines spins on a backend that cannot sleep in
  Poll. Neither is understood yet; both are the first thing to look at on
  mlx5, and until then afxdp is the faster backend for this stack.
- **Cores are whole-machine** and include the load generator's ACKs being
  processed, the fifo goroutines, and on dpdk/mlx5 the spinning receive
  loops. They are comparable within a table, not a per-packet cost.
- **Connection churn rows were dropped**: at 1,360 conn/s the generator ran
  out of ephemeral ports on both stacks alike, so they measured the client.

## The patched gVisor

[`gvisor/`](gvisor) is upstream gVisor at the commit in `gvisor/PIN` with
go-afxdp's patches plus the ones review here has added
(`gvisor/patches/`, one commit each; `gvisor/README.md` lists them), trimmed
to the 44 packages the stack imports and rewritten to this module's import
path. It is a
generated tree: `gvisor/import.sh` rebuilds it from the pin and the patches,
`import.sh -check` (run in CI) proves the committed tree is what the script
produces, and nothing in it is edited by hand. To move the pin or change a
patch, edit `PIN` or `patches/`, run the script, commit the result.

`New` applies the fork's settings once per process (`fork.go`): inline TCP
processing with the deferred work drained after every receive batch, a 1 ms
delayed ACK, the RACK and tail-loss-probe floors, precise RTT, the
zero-window fix, no chunk zeroing, no per-segment packet clone. Vet this
module's own packages (`go vet . ./examples/...`); the imported tree is
upstream's code and trips checks upstream's own lint configuration turns
off.

## Where this came from, and what is next

The endpoint is ported from go-afxdp's `examples/netstack`, which measured
this design against the kernel: parity at 10G on fewer cores, and 1.3M
requests/s at 100G. The patched gVisor that made those numbers is the one
in `gvisor/`. Still to come, in order of expected gain: one stack per queue
(shared-nothing), the fork's link backpressure (patch 0008; the endpoint
drops after a bounded wait instead, and does not yet return the would-block
status that engages it), NIC checksum offload through `OffloadTransmitter`,
and the mlx5 loss investigation. Patch 0004 (views over caller-owned
memory) is applied but unused: every received frame is copied into a pooled
view.

Verified: afpacket, afxdp (generic XDP) and dpdk's af_packet vdev on a veth
pair; dpdk on an EC2 ENA bound to vfio-pci (the stack alone on the ENI, doing
ARP); mlx5 on a ConnectX-6 Dx at 100G on VLAN 2043, both as server and as
client. Limits today: IPv4 and TCP only; one stack over all queues; software
checksums; packets that span several frames (`OptContinued`) are not accepted.
