# dpdk - a poll-mode driver in your process

The portable fast backend. Where mlx5 needs a ConnectX and AF_XDP needs the
kernel in the path, this needs only a DPDK poll-mode driver - Intel, Broadcom,
virtio, and the same ConnectX - at the cost of a build that links against DPDK
and a machine set up for it.

It is not a DPDK application. The workers are ordinary Go goroutines, not
lcores, and packetio's frame region *is* the driver's mempool through a custom
mempool operator, so nothing is copied and no mbuf is allocated per packet. The
packet path crosses into C three times per *batch* - one burst each way, plus a
poke when a transmit queue goes idle. At a batch of 64 that is under a
nanosecond per packet.

## What you need

- **x86-64.** The cgo layer is built for it; the build tag says `amd64`.
- **DPDK 23.11 or later**: `apt install libdpdk-dev dpdk` on Ubuntu 24.04. The
  poll-mode drivers are plugins loaded at startup - installing the package is
  enough, there is nothing to compile.
- A **`-tags dpdk` build**.
- Root, or `CAP_NET_RAW` + `CAP_IPC_LOCK` and access to `/dev/vfio`.

```bash
go build -tags dpdk ./...
```

Everything else depends on **which kind of device you have**, and the difference
is bigger than the usual DPDK instructions suggest.

### A ConnectX (mlx5), or any bifurcated card

Nothing. No hugepages, no `dpdk-devbind.py`, no kernel command line, no reboot.
The kernel keeps the interface and the card serves both of you.

```bash
sudo go run -tags dpdk ./examples/dpdk/info -i 0000:c1:00.1
```

### An Intel, Broadcom or virtio card - the exclusive path

This is the DPDK you have read about. The device leaves the kernel entirely.

```bash
ls /sys/class/iommu/                      # must not be empty; if it is, enable
                                          # IOMMU in BIOS + kernel cmdline, or
                                          # see EC2.md: no IOMMU is normal there
sudo sysctl -w vm.nr_hugepages=64         # ~128 MB is plenty
sudo modprobe vfio-pci
sudo ip link set eno2 down
sudo dpdk-devbind.py --bind=vfio-pci 0000:01:00.0
```

**`eno2` is now gone** - no `ip addr`, no `tcpdump`, no SSH on that port. To
give it back:

```bash
sudo dpdk-devbind.py --bind=ixgbe 0000:01:00.0   # the card's kernel driver
sudo ip link set eno2 up
sudo sysctl -w vm.nr_hugepages=0
```

10GBASE-T takes ~20 seconds to re-negotiate link after rebinding. Wait before
you trust a `ping`.

`dpdk-devbind.py --status` lists devices and their drivers. `Capabilities()
.KernelCoexistence` tells your program which world it is in.

### EC2, or anything else without an IOMMU

The exclusive path with `vfio-pci` in no-IOMMU mode, physical addressing and
mandatory hugepages. The bind sequence, the VPC's rules about source addresses,
and what an ENA does not offer are in [EC2.md](EC2.md).

## Open it

```go
d, err := dpdk.Open("0000:c1:00.1")   // PCI address, interface name, or vdev
if err != nil {
        log.Fatal(err)
}
defer d.Close()
```

A bare `Open` gives you **one transmit queue and no receive queues**. For both:

```go
d, err := dpdk.Open("0000:c1:00.1",
        dpdk.WithQueues(4),
        dpdk.WithSteering(packetio.SteeringFilter{
                Match: []packetio.Match{packetio.MatchVLAN(2043)},
        }),
)
```

If the device is still bound to a kernel driver, `Open` refuses with the exact
command that would hand it over - it never rebinds anything itself:

    dpdk: 0000:01:00.0 is bound to the ixgbe driver, which this backend cannot
    share. Hand the device over first:
        sudo dpdk-devbind.py --bind=vfio-pci 0000:01:00.0

## Options

The ones every backend spells the same way:

    WithQueues(n) / WithTxQueues(n) / WithRxQueues(n)
    WithFrames(n) / WithFrameSize(n)
    WithSteering(f)
    WithAffinity(cpus...) / WithoutAffinity()

Specific to this backend:

    WithTxDepth(n) / WithRxDepth(n)   descriptor ring depths
    WithMTU(n)
    WithPromiscuous()
    WithChecksumOffload() / WithTSO()
    WithRingsPerQueue(n)              hardware send queues behind each TxQueue
    WithoutHugePages(megabytes)       virtual devices only
    WithDevArgs(s)                    driver arguments, e.g. "rx_vec_en=1"
    WithEALArgs(args...)              raw EAL arguments
    WithLogLevel(s)

## Gotchas

**A frame is not all packet.** DPDK describes every buffer with an `rte_mbuf`,
and here they live inside the frame region: each frame holds the mempool object
header, the mbuf, headroom, then the packet 320 bytes in. `Capabilities()
.MaxFrameSize` reports what is left, and a descriptor reaching back into the
mbuf is refused.

**Frames come back late.** A driver reads transmit completions inside a burst
and nowhere else, and not until enough packets have gone since the last time:
32 on mlx5. A queue that stops sending mid-batch keeps those frames until it
sends more. `Complete` and `Reclaim` poke the driver when the queue is idle,
which is as much as DPDK allows.

**Untagged frames on a tagged link go nowhere.** Not a DPDK problem, but it has
cost us a day: transmit reports millions sent and the peer counts exactly zero.
Check what the link expects before blaming the card.

**Ownership failures are fatal, by design.** An address from outside the region,
or a free that did not fit the returned ring, means the backend can no longer
prove where every frame is. The queue latches `ErrQueueFailed` and refuses
further work rather than quietly running short.

## Performance

64-byte frames, ConnectX-6 Dx at 100G, whole-machine CPU accounting.

All with default options, one goroutine per queue:

| | rate | cores |
| --- | ---: | ---: |
| transmit, one queue | 55.5 Mpps | 1 |
| transmit, two queues | 106.9 Mpps | 2 |
| transmit, line rate | **148.8 Mpps** | **3** |
| receive, one queue | **46.1 Mpps** | 1 |
| receive, line rate | **148.8 Mpps** | **8** |
| forwarding, one queue | 23.6 Mpps | 1 |
| forwarding, best efficient | **127.1 Mpps** | 8 |

The rings behind each queue are what make the transmit column scale: the
Mellanox PMD switches to copied descriptors at eight send queues, whose
per-queue ceiling multiplies, and a single send queue absorbs about a
quarter of what one core produces. `WithRingsPerQueue` overrides the choice
and `Info.RingsPerQueue` reports it.

On an Intel X550 at 10G, one core reaches **14.9 Mpps - line rate**.

For reference, C `testpmd` on the same hardware sends 47.5 Mpps on one core
where this backend sends about 56. Using DPDK from Go costs nothing that is Go.


## Examples

```bash
# what DPDK found, and who owns the device
sudo go run -tags dpdk ./examples/dpdk/info -i 0000:c1:00.1

# send at line rate -- queues are cores, and three are enough at 64 bytes
sudo go run -tags dpdk ./examples/dpdk/blast -i 0000:c1:00.1 \
    -src-mac 7c:c2:55:be:f4:e1 -dst-mac 7c:c2:55:be:f3:c7 \
    -vlan 2043 -dst-port 9000 -queues 4

# receive it on the other machine
sudo go run -tags dpdk ./examples/dpdk/drop -i 0000:c1:00.1 \
    -vlan 2043 -dst-port 9000 -queues 4
```

The smallest useful program is about 20 lines: `Open`, `TxQueue(0)`, and a
`SendFunc` loop. See the [main README's quick start](../README.md#quick-start).

Like the mlx5 backend, the queues-per-worker arrangement lives inside the
backend now: on a Mellanox PMD each `TxQueue` opened on a transmit-only
device with two or more queues is backed by four hardware send queues, dealt
to round-robin. One goroutine per queue is the whole recipe, and
`-queues-per-worker` survives only for measuring other arrangements.
