# Writing a packetio backend

This is what a backend must guarantee. The interfaces in `queue.go` say what
the methods are; this says what they must *mean*, because that is where every
bug so far has been - one backend quietly meaning something different from
another, in a way that compiles and passes its own tests.

`internal/conform` is this document as a test. Every backend calls it; a new
one should too, on the first day rather than the last.

## The three owners of a frame

At any moment a frame belongs to exactly one of:

- **the pool** - free, waiting for `Alloc` or a receive
- **the application** - between `Alloc` and `Transmit`, or between `Receive`
  and `Recycle`
- **the NIC** - between `Transmit` and `Complete`/`Reclaim`

A frame in two places at once is the failure this whole design exists to
prevent, and it is invisible in every counter: two packets get built into one
buffer and the wire carries nonsense. `internal/pool` refuses a frame it does
not own and counts it; if `pool_rejected` in a queue's `Stats().Backend` is not
zero, something above the pool is returning frames twice or to the wrong queue.

## Rules

**One goroutine per queue.** Nothing in a queue is synchronised, and that is
what makes the packet path lock-free. Two queues of one device are independent
and may run concurrently. A receive queue and every transmit queue that carries
its frames must be driven by the same goroutine, because their pools are not
synchronised with each other.

**Transmit accepts a prefix.** It returns how many descriptors it took, and
those must be a prefix of what it was given. Frames in that prefix belong to
the NIC until `Complete` or `Reclaim` hands them back. **Frames in the
unaccepted suffix still belong to the caller** - a backend must not return them
to its own pool, however sure it is that they are finished. A caller following
the interface will `Free` them, and a backend that already freed them causes a
double return.

That rule holds even for a backend whose transmit is synchronous. afpacket's
`sendmmsg` has finished with a frame the moment it returns, and it still parks
accepted frames on a pending list until asked, because the forwarding cycle
below depends on it.

**Complete returns frames to this queue's pool. Reclaim hands them to the
caller.** They are not interchangeable:

```go
descs := rx.Receive(n)        // frames from the receive pool
sent := tx.Transmit(descs)    // the NIC owns them
back = tx.Reclaim(max, back[:0])
rx.Recycle(back)              // home again
```

`Complete` here would put receive frames on the *transmit* pool, where the
receive queue can never find them: the receive side starves and the transmit
pool fills with frames it does not own.

A backend whose `Reclaim` returns nothing breaks that forwarder silently, so
`Reclaim` must work wherever it can. Where it genuinely cannot - AF_XDP drains
its completion ring straight into a pool and never learns which addresses came
back - say so with `Capabilities.HandsBackFrames` false, and document what a
forwarder must do instead. A caller can then branch on the capability rather
than discover it as a queue that slowly stops receiving.

**SendFunc has one rule.** `build` returns the packet length. Zero means
nothing more to send: the batch ends there and what was built is transmitted.
Below zero, or past `len(frame)`, is `ErrBadLength` - the whole batch is
abandoned, every frame goes back to the pool, and it reports zero sent. Do not
invent a third meaning for zero.

**A descriptor must name at least one byte inside one frame.** A backend that
checks refuses the first descriptor that does not and returns the prefix before
it, so a short return with a free ring points at the offending descriptor. It
must never pass a bad address to the kernel or the card, and must never return
one to the pool.

**A received frame is on loan until Recycle.** Between `Receive` handing a
descriptor out and `Recycle` taking it back, the frame it names belongs to the
caller alone: no later `Receive`, no `Fill`, nothing the backend does may write
to it, hand it out again, or move it. `Region.Bytes` is one mapping for the
life of the device, so an address stays an address. This is what lets a
forwarder carry received frames through whatever it does next instead of
copying them out first, and it is the reason `Fill` draws only on frames the
pool holds. Breaking it does not produce an error anywhere: it produces a
packet that changes while somebody is reading it.

**Region.Frame and Region.Writable return nil** for a descriptor that is not
inside the region. They do not panic. Code written against one backend has to
work against another.

**Close is idempotent, and every method after it is safe.** Calling a queue
method on a closed queue returns zero, or `ErrClosed`, but never touches
unmapped memory. `Close` may be called twice. It may *not* be called while
another goroutine is inside `Receive` or `Transmit` - but a goroutine blocked
in `Poll` must be woken, or a program can never shut down.

**Errors are the package sentinels.** `ErrClosed`, `ErrBadLength`,
`ErrUnsupported`, `ErrQueueFailed`, wrapped with `%w`. `errors.Is` has to mean
the same thing on every backend or callers cannot branch on it. A backend built
on another library must translate that library's errors, not forward them:
`net.ErrClosed` is not `packetio.ErrClosed`, and a caller testing for one gets
a different answer per backend.

**A dead queue must be distinguishable from a busy one.** `Transmit` returns
zero for both, so `Err` is what tells them apart. A backend whose queue can
fail permanently reports it there, wrapping `ErrQueueFailed`; one whose queues
cannot returns nil until close. Without this a caller retrying a full ring and
a caller retrying a dead queue write the same loop, and the second never
terminates.

## Methods a backend may make trivial

Not every backend has every concept. These are the honest stubs, and what they
must return:

| method | when trivial | must return |
| --- | --- | --- |
| `Fill` | the kernel owns the receive buffer (afpacket) | how many frames are free to receive into |
| `Complete` | transmit is synchronous | the number actually returned to the pool |
| `NumInFlight` | transmit is synchronous | frames sent but not yet handed back - **not** zero, unless they really are all back |
| `NumFreeSlots` | there is no ring | how many more frames `Transmit` will take before it refuses |
| `Poll` | there is nothing to sleep on (mlx5) | spin up to the timeout; set `Capabilities.BlockingPoll` false |
| `Reclaim` | the backend cannot name the frames it reclaimed (afxdp) | complete, return `out` unchanged, and set `Capabilities.HandsBackFrames` false |
| `Err` | the backend has no failure outliving one call | `ErrClosed` once closed, otherwise nil |
| `Complete`/`Reclaim` | the driver signals completions on its own schedule (dpdk) | what has come back so far; **`NumInFlight` must still be truthful** about the rest |

**Count receive batches.** `RxStats.Batches` is receive calls that returned
packets, and packets divided by it is the effective batch size. It is the one
number that shows a receive loop taking the offered load a handful of packets
at a time -- paying a call's fixed cost over too few of them and leaving the
card short of posted buffers between bursts. Nothing else in `RxStats` shows
it: packets, bytes and drops are identical whether they arrived sixty at a
time or five. One backend lost half its receive rate to exactly that and it
survived a benchmark campaign, because the counter existed internally and was
never published. `internal/conform` now checks it.

**Scratch that two directions touch must be indexed, not truncated.** The bug
above was a fill path and a receive path sharing one array: fill truncated it
to what it posted, receive read its length where it meant its capacity, and
every burst after a small top-up was capped at that size. The mlx5 backend
cannot express that mistake, because its fill and receive index a fixed-size
ring by slot rather than re-slicing a shared buffer. Prefer that shape; where
a staging array is genuinely needed, give each direction its own.

`Capabilities` must be truthful: a false or zero field means "not available
here", never "unknown". `MaxQueues` is the ceiling, not the number opened;
that is `NumTxQueues`.

Truthful means *what the device took*, not what the backend asked for. A DPDK
poll-mode driver silently drops an offload it does not offer rather than
failing to configure, so `dpdk` builds its `Capabilities` from what
`rte_eth_dev_configure` kept. Asking is not having, and a caller told a NIC
computes a checksum it will not compute has no way to find out except from the
far end.

## Steering

A backend that steers - takes packets away from the kernel - accepts a
`packetio.SteeringFilter` and compiles `SteeringFilter.Rules()`, never the
filter itself: the expansion from "these
ports on this VLAN" into one conjunction per port is done once in the core, so
every backend means the same thing by a filter.

Two rules, and they are not negotiable:

- **Refuse, never widen.** A match this backend cannot express is
  `ErrUnsupported` at Open. A filter that delivers more than it was asked for
  is indistinguishable, downstream, from one that works.
- **Only a backend that steers takes a SteeringFilter.** It means "the kernel
  does not see what I take". AF_PACKET cannot deliver that - it is a tap, and
  the kernel sees everything - so it has no steering option at all rather than
  one that means something weaker under the same name.

`conform.RunSteering` checks that a promiscuous filter is accepted and a
contradictory one refused. It cannot check what arrives (that needs traffic),
so a backend should also have a test that sends two things and receives one.

## Optional interfaces

A backend that can do more implements one of the optional interfaces, and
callers reach it with a type assertion:

```go
if r, ok := rq.(packetio.OffloadReceiver); ok {
    descs, offs := r.ReceiveOffload(64)
}
```

The assertion answers "does this backend understand the operation"; whether
*this device* was opened able to do it is what `Capabilities` and the option
that enables it say. A backend may implement the interface on its queue type
unconditionally - `afpacket` and `dpdk` do - but a call the device cannot
honour must fail loudly with `ErrUnsupported`, never succeed doing nothing: an
assertion that lands in a method quietly returning zero values is worse than
no interface at all, because nothing downstream can tell.

**A packet may be more than one frame,** in both directions. Where a backend
can hand the hardware a scatter list, a caller may give one packet as several
descriptors, each but the last carrying `OptContinued`; and where a backend can
fill several frames from one arriving packet, it may deliver one the same way.
Receiving in chains is opt-in, because a caller that does not read
`OptContinued` would take the first frame of a packet for a whole one. A packet
is delivered whole or not at all, so one that will not fit the frames available
is left where it is rather than delivered in part. The exception is a packet
needing more slots than the caller's whole batch: no call of that size will ever
take it, and leaving it would stop the queue, so it is counted oversize and
dropped. A receiving caller therefore sizes its batch for the traffic, at least
`ceil(largest packet / MaxFrameSize)`.

Two rules follow. The metadata belongs to the packet, not to a frame of it, so
an `Offload` rides the first descriptor and the continuations carry none, and a
partial checksum on a chain is left for the caller to finish once the packet is
whole; and `Transmit`'s prefix is a prefix of packets as well as of frames,
because half a packet is not a shorter packet, it is a fragment nothing
downstream can use. `Capabilities.MultiBuffer` means both directions, and a
backend sets it when it can do both -- `afpacket` does under `WithMultiBuffer`.

`GatherTransmitter` is the one optional interface that steps outside the frame
ownership model, and it can only exist where there is no ownership to hand
over. AF_PACKET transmit is synchronous -- the kernel copies into an skb before
`sendmmsg` returns -- so a packet may be sent straight out of the caller's
memory, nothing is taken from a pool, nothing pends, and nothing is reclaimed.
Any device that reads the bytes by DMA after the call cannot offer this at any
price: its memory must be registered with the device first, which is what a
Region is. `Capabilities.GatherTx` says which kind a backend is, and it is
false everywhere a NIC does its own fetching.

`TimestampReceiver` follows the same shape and one extra rule, because a
timestamp has no defensible zero: an offload of all zeroes means "an ordinary
frame", but a time of zero is a claim that a packet arrived at the epoch, and
nothing downstream can tell that from a real reading.

So the rule is not "do not implement it" -- an interface in Go is satisfied by
the type, and whether a particular device has a clock is not known until it is
opened. The rule is that a queue whose device cannot stamp must **return
nothing**: no descriptors and no times. `Capabilities.RxTimestamps` is the
authority a caller should ask first, and `conform.RunTimestamps` checks both
halves, that the capability is honest and that a queue without a clock refuses
rather than invents.

A backend may implement one direction and not the other: `dpdk` is an
`OffloadTransmitter` - it can have the NIC compute checksums and segment a
super-frame - but not an `OffloadReceiver`, because there is no LRO to report.
`Capabilities.Offload` means *both*, so it stays false there.

Metadata that describes a packet must be checked against the packet. `dpdk`
parses the frame's own headers and refuses a `CsumStart` that disagrees with
them, because the alternative is a NIC writing two bytes of checksum over
somebody's payload - which no counter anywhere will show.

## Verifying a backend for real

`go test ./...` passing means very little on its own: most of the suite needs
hardware or privilege, and a subtest that cannot open a device skips. A skipped
test and a passing test print the same word.

That is not hypothetical. AF_XDP's conformance test opened a device in a way
that backend always refuses, so every subtest skipped from the first commit
onwards and the package reported `ok` for the whole life of the module. The
first time it actually ran, it failed on the first check - `Transmit` was
handing the kernel descriptors it should have refused.

So there are three levels, and only the third proves anything:

```bash
# 1. Compiles, and the pure logic is right. Proves the least.
go test ./...

# 2. The parts that need privilege but not a NIC.
sudo PACKETIO_VETH_TESTS=1 go test -race ./afpacket/ ./internal/...

# 3. The real thing. PACKETIO_CONFORM_REQUIRED turns "cannot open a device"
#    from a skip into a failure, so a backend whose opener is broken says so
#    instead of quietly reporting nothing.
sudo PACKETIO_IFACE=eno2 PACKETIO_VETH_TESTS=1 PACKETIO_CONFORM_REQUIRED=1 \
    go test -race -tags packetio_checked ./...

# The dpdk backend is behind a build tag, so it needs its own run.
sudo PACKETIO_DPDK_DEV=0000:c1:00.1 PACKETIO_VETH_TESTS=1 \
    PACKETIO_CONFORM_REQUIRED=1 go test -race -tags 'dpdk packetio_checked' ./dpdk/...
```

Run the third one before believing anything about a backend. If it skips, the
backend is unverified - not working.

## Checklist for a new backend

1. `var _ packetio.Device = (*Device)(nil)` and the same for both queue types.
2. Call `conform.Run` from your package's test, and check it actually runs:
   `PACKETIO_CONFORM_REQUIRED=1` with the hardware present must not skip.
3. Fill in `Capabilities` honestly.
4. Use `internal/pool` for the free list, one pool per queue per direction,
   over disjoint ranges of the region.
5. Wrap the package sentinels.
6. Say in the package doc what the backend needs, what it costs, and what it
   cannot do.
