# mlx5 - Direct Verbs on ConnectX

The fastest backend here, and the one that costs you the least operationally.

The card's queue memory and doorbell register are mapped straight into your
process. After `Open` there is no kernel in the packet path and no C library
either: sending is writing a descriptor to memory the NIC is watching and one
8-byte store to ring the doorbell. **69.2 Mpps on one core.**

The kernel keeps the interface the whole time. `mlx5_core` stays bound, `eth0`
stays up, SSH keeps working, and the card serves both of you at once - you take
only what your steering filter names.

Why Direct Verbs rather than DPDK on the same card, and what it costs:
[Four ways to do super fast packet processing in Go](https://toonk.io/packetio/).

## What you need

- A ConnectX-4 or newer / BlueField card (measured on ConnectX-6 Dx).
- **rdma-core.** To build: `apt install libibverbs-dev` (Ubuntu 24.04), which
  brings the `libibverbs` and `libmlx5` link targets. To *run* a binary built
  elsewhere: `apt install libibverbs1 ibverbs-providers`. The backend links
  `-libverbs -lmlx5` and nothing else.
- A **`-tags mlx5` build** - the tag keeps cgo and rdma-core out of everyone
  else's build.
- `CAP_NET_RAW`, and a memory lock limit big enough for the frame region
  (`ulimit -l`, or run as root).
- `/dev/infiniband` present, which means the `mlx5_ib` module is loaded. If
  `Open` fails before it touches the card, check this first:

```bash
ls /dev/infiniband      # uverbs0, uverbs1, ... one per port
```

```bash
go build -tags mlx5 ./...
```

## Open it

```go
d, err := mlx5.Open("eth0")
if err != nil {
        log.Fatal(err)
}
defer d.Close()
```

That gives you **one transmit queue and no receive queues** - a generator
should not pay for receive memory it never uses. For both directions:

```go
d, err := mlx5.Open("eth0", mlx5.WithQueues(4))
```

To receive only what you asked for, and leave the rest of the traffic to Linux:

```go
d, err := mlx5.Open("eth0",
        mlx5.WithQueues(4),
        mlx5.WithSteering(packetio.SteeringFilter{
                Match: packetio.MatchDstPort(packetio.IPProtoUDP, 9000),
        }),
)
```

## The recipe: queues are cores

One goroutine per queue is the whole model. Open as many transmit queues as
cores you are willing to spend, give each its own goroutine, and the backend
does the rest - descriptor mode, hardware rings behind each queue, region
sizing, and seating each worker on a processor of its own. On a ConnectX-6 Dx
this reaches **64-byte line rate on three cores** (147.8 Mpps); the program
below opens four for margin:

```go
d, err := mlx5.Open("eno2", mlx5.WithTxQueues(4))
if err != nil {
        log.Fatal(err)
}
defer d.Close()

var wg sync.WaitGroup
for i := 0; i < d.NumTxQueues(); i++ {
        wg.Add(1)
        go func(tx packetio.TxQueue) {
                defer wg.Done()
                for running() {
                        tx.SendFunc(256, func(_ int, frame []byte) int {
                                return copy(frame, pkt) // your packet bytes
                        })
                }
        }(d.TxQueue(i))
}
wg.Wait()
```

Scale down the same way: one queue is 69.2 Mpps, two are 102, three are 148.
[`examples/blast`](../examples/blast) is this recipe with flags, rate control
and counters - `-queues 4` and nothing else reaches the wire.

## Options

The ones every backend spells the same way:

    WithQueues(n)          both directions at once
    WithTxQueues(n)        one direction
    WithRxQueues(n)
    WithFrames(n)          frames in the region
    WithFrameSize(n)
    WithSteering(f)        what to take; everything else stays with the kernel
    WithAffinity(cpus...)  place the workers yourself
    WithoutAffinity()      leave them to the scheduler

Specific to this backend:

    WithTxDepth(n) / WithRxDepth(n)   descriptor ring depths, powers of two
    WithMultiPacket(maxLen)           copy packets into descriptors (see below)
    WithoutMultiPacket()
    WithChecksumOffload()             let the NIC compute L3/L4 checksums
    WithInlineHeader(n)               bytes of header inlined per packet
    WithCompletionEvery(n)            how often the NIC signals completion
    WithHugePages()

## Timestamps

Every completion carries the card's own reading of when the frame arrived at
the port, in 1 ns steps:

```go
rx, _ := d.RxQueue(0).(packetio.TimestampReceiver)
descs, ts := rx.ReceiveTimestamps(256)
```

The stamp is taken before the transfer to memory and before your code runs, so
the interval between two of them is what happened on the wire however busy the
receive loop was. Converting the card's ticks to nanoseconds is a multiply and
a shift, done here, never a call into the driver: the rate is read once at
Open. Against the wall clock over three, four and six second windows the two
agreed to four decimal places.

`Capabilities().RxTimestamps` is true when the device reported a clock; where
it is false the queue still carries the method, and it returns nothing rather
than inventing zeroes.

The card's counter is narrow -- 41 bits on a ConnectX-6 Dx, a nanosecond a
tick, so it comes back round every **37 minutes**. The backend counts the wraps
and adds them back, so the times it reports keep rising for as long as the
queue is open, and an interval measured across a wrap is the real one. A wrap
is a step back of eighteen minutes and cannot be confused with the ordinary
sub-microsecond steps below.

The stamp says when a packet **arrived**, not the order it was **delivered**:
while the card is dropping traffic those two come apart, and about a third of
packets arrive out of stamp order at 148 Mpps offered to a queue taking 44. At
rates it keeps up with, two in nine million.

## Placement happens for you

The workers are placed on processors automatically, packing one cache complex
before starting the next and preferring the node the card is on. It is worth
having: **+29% at three workers** against leaving it to the scheduler.

The reason is the doorbell. Workers share a small number of write-combining
pages, and a page written from two last-level caches costs far more than one
written from a single cache - two workers in one complex measured 99.1 Mpps
against 83.4 split across two.

`WithAffinity(cpus...)` takes the decision back; `WithoutAffinity()` turns it
off.

## The two ceilings

This card has two transmit mechanisms and they fail in different directions:

| descriptors | ceiling | shape |
| --- | --- | --- |
| **pointer** (default) | ~76 Mpps | device-wide, however many queues |
| **copy** (`WithMultiPacket`) | ~17 Mpps | *per queue*, so it multiplies |

Pointer descriptors win below 76 Mpps - same work, a third of the
instructions. Above it, only copying gets there, because its ceiling multiplies
by send-queue count.

**You do not have to know any of this.** A transmit-only device opened with
two or more queues - or a forwarding device with four or more - switches to
copied descriptors by itself, and in that mode
each `TxQueue` brings **four hardware rings** and deals batches over them
round-robin - because one copied ring absorbs about a quarter of what one core
produces. One goroutine per queue is then the right thing to write:

| what you want | how | measured |
| --- | --- | ---: |
| the most from one core | `WithTxQueues(1)` - pointer descriptors | 69.2 Mpps |
| 64-byte line rate | `WithQueues(3)`, one goroutine each | **147.8 Mpps, 3 cores** |

`WithMultiPacket` / `WithoutMultiPacket` still pick a mode explicitly, and
`WithRingsPerQueue(n)` overrides the fan-out; `Info.RingsPerQueue` reports
what was chosen.

## Performance

64-byte frames, whole-machine CPU accounting.

All with default options, one goroutine per queue:

| | rate | cores |
| --- | ---: | ---: |
| transmit, one queue | **69.2 Mpps** | 1 |
| transmit, two queues | 100.7 Mpps | 2 |
| transmit, line rate | **148.8 Mpps** | **4** (147.8 on 3) |
| receive, one queue | 44.1 Mpps | 1 |
| receive, line rate | 148.6 Mpps | 10 |
| forwarding, one queue | **29.3 Mpps** | 1 |
| forwarding, line rate | **147.4 Mpps** | 12 |
| forwarding, near line rate | 147.6 Mpps | 12 |

At 1500-byte frames **one core saturates 100 Gbit/s**.


Forwarding earned two defaults of its own. Arriving frames are cache-cold
on hosts whose inbound DMA bypasses the cache, and the parse used to stall
a full memory latency per packet - the receive ring now prefetches every
frame as its completion is consumed. And a forwarding device with four or
more queues switches to copied descriptors, the same escape from the ~76
Mpps pointer wall that transmit uses. Together: 68.5 became 141.1 Mpps on
the same eight cores. What remains between 147.6 and the wire is the
receive side: striding RQ and CQE compression.

## Gotchas

- **Memory lock limit.** The frame region is pinned. A low `ulimit -l` shows up
  as a failure at `Open`, not later.
- **Receive has no striding queue or CQE compression yet**, which is the one
  place the kernel driver still beats this backend. It is the next milestone.
- **A zero-length data segment is not the same as no data segment.** The card
  answers the first with a length error and takes the queue out of service.
  Found on hardware; now a test.

## Examples

```bash
sudo go run -tags mlx5 ./examples/info  -i eth0
sudo go run -tags mlx5 ./examples/blast -i eth0 -dst-mac ... -queues 4
sudo go run -tags mlx5 ./examples/drop  -i eth0 -queues 4
sudo go run -tags mlx5 ./examples/steer -i eth0 -udp-port 9000
sudo go run -tags mlx5 ./examples/l3fwd -i eth0 -queues 4 \
  -route 10.0.0.0/8,7c:c2:55:be:f3:c7
```

[`send`](../examples/send) is the smallest one: a single frame, and what the
hardware said about it.
