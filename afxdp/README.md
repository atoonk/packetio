# afxdp - AF_XDP, the express lane

Fast, and it leaves your NIC where it is. This backend wraps
[go-afxdp](https://github.com/atoonk/go-afxdp) behind packetio's API.

A small eBPF program runs inside the NIC driver, before the kernel allocates an
`sk_buff` and before the network stack. For each packet it decides: up the
normal stack, or into a socket that shares a chunk of memory (the **UMEM**)
with your program. On drivers that support it the NIC writes packets directly
into your memory, and you read them where they landed.

Everything the filter does not match carries on into the kernel. `ip`,
`ethtool` and `tcpdump` all keep working.

## What you need

- A driver with **XDP support** (Intel, Mellanox, Broadcom and others; zero-copy
  varies).
- `CAP_NET_RAW`, and a memory lock limit for the UMEM (`ulimit -l unlimited`, or
  root).
- No build tag, no hugepages, no unbinding.

## Open it

AF_XDP is the one backend that makes you say what you want:

```go
d, err := afxdp.Open("eth0", afxdp.WithSteering(packetio.SteeringFilter{
        Match: packetio.MatchDstPort(packetio.IPProtoUDP, 9000),
}))
if err != nil {
        log.Fatal(err)
}
defer d.Close()
```

**A bare `afxdp.Open("eth0")` is refused, on purpose.** Without a filter the
XDP program would redirect every packet on the interface to your sockets and
keep it from the kernel - cutting off SSH and everything else. There is no safe
default to fall back to, so it asks. To take everything deliberately:

```go
afxdp.WithSteering(packetio.SteeringFilter{Promiscuous: true})
```

## Options

The ones every backend spells the same way:

    WithQueues(n)          how many queues to bind, from queue 0
    WithFrames(n)          frames in the UMEM
    WithFrameSize(n)       2048 by default; some drivers need 4096 for zero-copy
    WithSteering(f)        required - see above
    WithAffinity(cpus...)
    WithoutAffinity()

`WithXDP(opts ...xdp.Option)` passes go-afxdp's own options through, for what
this package does not name - busy-poll, wakeup flags, XDP mode, NAPI tuning:

```go
afxdp.Open("eth0", steer, afxdp.WithXDP(xdp.WithBusyPoll(50, 64)))
```

**None of the named options changes a default.** go-afxdp picks the frame
geometry, binds every queue, tunes the driver's interrupt behaviour and places
each worker beside its queue's interrupt on its own. Those options exist for
overriding a decision, not as setup you have to perform.

The one option this backend sets for you is **need-wakeup**, bound before
anything you pass so a `WithXDP` can still override it. It lets the driver
park when idle and say so, rather than spinning its NAPI loop for nothing:
go-afxdp measured 25 million polls a second and 65% of a twelve-core box in
soft interrupt while forwarding zero packets - and it spares transmit a
`sendto` per batch. go-afxdp does not default it because a caller driving the
rings directly must then wake the driver itself; this backend is that caller,
and `Fill` does it (below).

## Gotchas

These are the ones that cost real afternoons.

**Bind the queues you will actually drive.** One socket covers one hardware
queue, and a queue with no socket goes to the kernel instead - silently. A
single flow lands on one queue, so opening 4 of a card's 48 means a 1-in-12
chance of seeing anything, and the failure looks like an idle link. Either bind
them all, or narrow the card's hash to match:

```bash
sudo ethtool -L eth0 combined 4      # then open 4 queues
```

**Binding every queue turns placement off.** Automatic placement seats each
worker on a free core in the same complex as its queue's interrupt. Bind all 48
queues on a 48-core box and every core is an interrupt core, so there is nowhere
free and it declines rather than seating a worker on top of a softirq - which
measured *worse*. Measured on a ConnectX-6 Dx: `WithQueues(4)` places 4 of 4
workers (CPUs 0, 5, 7, 10); binding all 48 places none.

**On a tagged interface, register the VLAN.** A driver with `rx-vlan-filter on`
(the default on ConnectX) drops a tag no VLAN sub-interface has claimed, in
the card, before the XDP program runs. Zero packets, no error. Create the VLAN
interface or turn the filter off.

**AF_XDP filters cannot match a VLAN id.** The tag is skipped transparently so
the same program works whether or not the NIC strips it. Match on ports or
addresses instead; packetio refuses a VLAN match rather than installing a wider
filter.

**The cost lives outside your process.** The kernel driver still runs the
hardware, and its half shows up as softirq. Receiving or forwarding costs two
cores per queue: a worker and its soft interrupt. Measure the machine, not the
process.

**Forwarding needs `WithTxReuseRxFrames`.** `Capabilities().HandsBackFrames` is
false here: the completion ring is drained into a pool without saying which
frames came back, so `Reclaim` cannot name them. Without that option a receive
frame transmitted here leaks into the transmit pool and the receive side
starves.

```go
afxdp.Open("eth0", steer, afxdp.WithXDP(xdp.WithTxReuseRxFrames()))
```

**A parked driver needs a kick, and this backend's `Fill` delivers it.** With
need-wakeup on - which is this backend's default - a driver whose NAPI loop completes parks itself and posts no
more receive descriptors until a syscall wakes it. go-afxdp's own `Poll`
does that as a side effect, but the packetio contract is Fill/Receive with
no Poll - so this backend's `Fill` checks `NeedsWakeupRx` (one atomic load)
and calls `WakeupRx` when the driver parked. Before that kick existed, seven
of eight queues took exactly their initial 1,024 frames and then nothing,
while the port dropped 2.5 billion packets and every fill ring sat provably
full - and forwarding masked it entirely, because transmit's own kick
happens to drive the same NAPI. If you bypass this wrapper and drive a
`Socket` directly without ever blocking in `Poll`, call `WakeupRx` yourself;
`Stats().Backend["rx_kicks"]` is the counter that keeps this visible.

## Performance

64-byte frames, ConnectX-6 Dx at 100G, whole-machine CPU accounting with the
soft-interrupt share - the kernel's half of AF_XDP - shown alongside.
Medians of three passes, defaults only.

| | rate | cores (softirq) |
| --- | ---: | ---: |
| transmit, one queue | 18.8 Mpps | 1.0 (0.5) |
| transmit, line rate | **147.3 Mpps** | 16 queues, 16.0 (9.6) |
| receive, one queue | 32.3 Mpps | 1.6 (1.0) |
| receive, line rate | **148.8 Mpps** | **8 queues, 11.7 (7.7)** |
| forwarding, one queue | 17.6 Mpps | 2.0 (1.0) |
| forwarding, best | 112.9 Mpps | 16 queues, 23.2 (10.1) |

Transmit has two slopes, and the backend picks between them for you (via
go-afxdp v0.11.0): up to four queues it runs the driver's pointer
descriptors at ~17.9 Mpps per core (18.8 / 35.9 / 53.6 / 71.1 on 1-4),
then switches back to the kernel's copied multi-packet path, whose per-core
cost is higher but whose ceiling scales to the wire: 90.1 / 120.3 / 147.3
at 6 / 8 / 16. Receive and forward cost about **1.5-2 cores per queue**,
a worker and its soft interrupt - which is why sixteen forwarding queues
spend 23 machine cores, ten of them in softirq.

Placement matters more here than anywhere else: with go-afxdp choosing, one
transmit queue does 18.8 Mpps on a single core; pinned by hand to a core of
our choosing it did 6.5 on two. Let it place its own workers.

Receive reaches **line rate - 148.8 Mpps - on 8 queues** (11.7 machine
cores), 32.3 / 65.1 / 128.3 Mpps at 1 / 2 / 4. Expect ±15% run-to-run
spread on this backend - it is the widest of the four. An earlier version
degraded *down* to 5.9 Mpps at sixteen queues; the root cause and the
three-line fix are below.

## Examples

The examples in [`examples/`](../examples/) target mlx5 and dpdk, but the API is
the same; swap the import and the `Open` call. Start with
[`hello`](../examples/hello), which runs anywhere.

For AF_XDP-specific tooling - packet generators, tcpdump-expression filters,
jumbo frames - see [go-afxdp](https://github.com/atoonk/go-afxdp).
