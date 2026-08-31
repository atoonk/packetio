# afpacket - the one that always works

No hardware requirements, no build tag, no cgo, no setup. If the machine runs
Linux, this backend runs: a laptop, a VM, a container, a veth pair in CI.

It is also by far the slowest - roughly **1-2 Mpps**, against 66 for Direct
Verbs. It is here so that code written against this library runs anywhere, and
as the floor the other backends are measured against.

Receive is a TPACKET_V3 mmap ring; transmit is batched `sendmmsg`. A transmit
ring was tried and measured slower, so it is deliberately absent.

## What you need

`CAP_NET_RAW`. That is the whole list.

```go
d, err := afpacket.Open("eth0")
if err != nil {
        log.Fatal(err)
}
defer d.Close()
```

Unlike mlx5 and dpdk, a bare `Open` here gives you **one queue in each
direction**, so both examples in the [main README](../README.md#quick-start)
run as written.

## Options

    WithQueues(n) / WithTxQueues(n) / WithRxQueues(n)
    WithFrames(n) / WithFrameSize(n)
    WithSocketBuffer(bytes)
    WithPromiscuous()
    WithGSO()                 segmentation and checksum metadata per frame

There is no `WithSteering` and no affinity option, and both absences are
deliberate - see below.

## Why there is no steering

AF_PACKET is a **tap**, not a diversion. The kernel hands your socket a copy and
processes the original anyway. A filter here would only choose which copies you
receive; it could never keep traffic from the kernel, which is what steering
means on every other backend.

Rather than offer something weaker under the same name, this backend offers
nothing: its receive queues take everything and you select in your own loop.
`Capabilities()` reports no steering, so a program can ask rather than assume.

That tap behaviour is also the performance story. Point a line-rate flood at an
AF_PACKET receiver and the machine burns **~31 cores of softirq** to deliver
1.7 Mpps of useful packets - the rest is the kernel dutifully processing (and
dropping) the originals of everything you are reading copies of.

## Why there is no affinity option

This backend has no per-queue worker placement to configure - no `Pin`, no
placement machinery. Its ceiling is the kernel stack, not core placement, so
adding the option names would be uniformity theatre. mlx5, afxdp and dpdk all
place workers automatically and accept `WithAffinity`.

## Offload

`WithGSO()` turns on `PACKET_VNET_HDR`, so segmentation and checksum metadata
travels with each frame and a 64 KB super-frame crosses the device whole:

```go
d, _ := afpacket.Open("eth0", afpacket.WithGSO())
if r, ok := d.RxQueue(0).(packetio.OffloadReceiver); ok {
        descs, offs := r.ReceiveOffload(64)
}
```

A received frame whose checksum is only the pseudo-header partial is completed
on the way out, and a VLAN tag the kernel stripped is put back with the checksum
offsets shifted to match.

## Performance

68-byte frames on a ConnectX-6 Dx, one core:

| | rate | cores |
| --- | ---: | ---: |
| transmit, one queue | 1.7 Mpps | 1 |
| transmit, sixteen queues | 17.0 Mpps | 16 |
| receive, under a line-rate flood | 1.6 Mpps | **41** |
| forwarding | 1.3 Mpps | **41** |

Those core counts are not a typo and they are the whole story: under a
148.8 Mpps flood the kernel processes every frame whether or not your program
reads a copy, so the machine burns 41 cores to hand you 1.6 Mpps.

More transmit queues do **not** help: the device queue saturates and every
socket contends on it.

## What it is good for

- **Development and CI.** The conformance suite runs against this backend on a
  veth pair, so the contract in [BACKENDS.md](../BACKENDS.md) is verified on
  every machine with no hardware at all.
- **Tools and probes** where a million packets a second is plenty.
- **Portability insurance.** Write against the API, ship everywhere, and switch
  the import on the machines that have the hardware.

## Examples

[`hello`](../examples/hello) uses this backend, is about a hundred lines, does both
directions, and runs on any interface:

```bash
sudo go run ./examples/hello -i eth0
```
