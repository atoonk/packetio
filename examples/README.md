# Examples

Every example is a complete program you can run. They are ordered here roughly
by how much they assume.

## Start here - runs on any Linux box

[`hello`](hello) is about a hundred lines, does both directions, and uses the AF_PACKET
backend so it needs no special hardware:

```bash
sudo go run ./examples/hello -i eth0
```

    afpacket on eth0
    sent 1 frame(s)
    listening for 3s...
      110 bytes, 66:ce:87:8a:7f:ec -> 33:33:00:00:00:16, ethertype 0x86dd
    received 1 frame(s)

Read it before the others: it is the whole API - open, allocate, fill,
transmit, fill, poll, receive, recycle, close - in one file with nothing else
going on.

## When packets arrived - runs anywhere too

[`timestamps`](timestamps) asks the device when each packet turned up, rather
than asking your own clock when you got round to looking, and prints the gaps
between them. It uses AF_PACKET, so like `hello` it needs no hardware:

```bash
sudo go run ./examples/timestamps -i eth0 -d 10s
```

    listening on eth0 for 10s

    412 packets, 411 gaps, in milliseconds:
      shortest    0.099
      median      1.204
      p99        68.310
      longest   201.887

The gaps come from the time the kernel (or, on a ConnectX, the card) recorded
as each packet arrived, so a busy moment in this program does not make the
traffic look bursty. Swap the import and the Open call for `mlx5` and the same
loop reads the card's own 4 ns clock.

## Direct Verbs (ConnectX) - `-tags mlx5`

| | what it shows |
| --- | --- |
| [`info`](info) | what the card says about itself: queues, MTU, offloads, link |
| [`send`](send) | one frame, and exactly what the hardware reported back |
| [`blast`](blast) | a generator: rate control, multiple queues, CPU placement |
| [`drop`](drop) | the receive cycle and what it costs, with the port's counters |
| [`steer`](steer) | ask for two UDP ports; watch only those arrive |
| [`pingpong`](pingpong) | round trips between two machines, and where the time went |
| [`l3fwd`](l3fwd) | an IPv4 router forwarding in the frame the packet arrived in |

```bash
sudo go run -tags mlx5 ./examples/info -i eth0

# generator → receiver, on two machines. -queues 4 is 64-byte line rate
# (148.8 Mpps) on four cores: the backend picks the descriptor mode and the
# rings behind each queue, so queues = cores is the whole recipe.
sudo go run -tags mlx5 ./examples/blast -i eth0 -dst-mac 7c:c2:55:be:f3:c7 -queues 4
sudo go run -tags mlx5 ./examples/drop  -i eth0 -queues 4

# take only what you asked for; the kernel keeps the rest
sudo go run -tags mlx5 ./examples/steer -i eth0 -udp-port 9000 -udp-port 9001
```

## DPDK - `-tags dpdk`

| | what it shows |
| --- | --- |
| [`dpdk/info`](dpdk/info) | the driver, who owns the device, and which offloads are real |
| [`dpdk/blast`](dpdk/blast) | the generator again, over DPDK |
| [`dpdk/drop`](dpdk/drop) | the receive cycle, with the NIC's own counters |
| [`dpdk/l3fwd`](dpdk/l3fwd) | the same router, sharing its forwarding code with the one above |

On a ConnectX this needs no setup at all. On an Intel or Broadcom card the
device must be handed to `vfio-pci` first - see [dpdk/README.md](../dpdk/README.md).

```bash
sudo go run -tags dpdk ./examples/dpdk/info -i 0000:c1:00.1

# -queues 4 is 64-byte line rate here too (148.8 Mpps; three cores is
# already enough) -- queues are cores on this backend as well
sudo go run -tags dpdk ./examples/dpdk/blast -i 0000:c1:00.1 \
    -src-mac 7c:c2:55:be:f4:e1 -dst-mac 7c:c2:55:be:f3:c7 \
    -vlan 2043 -dst-port 9000 -queues 4

sudo go run -tags dpdk ./examples/dpdk/drop -i 0000:c1:00.1 \
    -vlan 2043 -dst-port 9000 -queues 4
```

`-h` on any of them lists its flags.

## The smallest useful generator

If you only want to see how little code it takes, this is a complete sender:
open, one queue, one loop:

```go
d, err := dpdk.Open("0000:c1:00.1")   // or mlx5.Open("eth0")
if err != nil {
        log.Fatal(err)
}
defer d.Close()

tx := d.TxQueue(0)
for {
        if _, err := tx.SendFunc(256, func(i int, frame []byte) int {
                return copy(frame, pkt)
        }); err != nil {
                log.Fatal(err)
        }
}
```

Measured at 58 Mpps on a ConnectX-6 Dx and 14.9 Mpps - 10G line rate - on an
Intel X550, with no options and no tuning.

## Shared code

[`internal/forward`](internal/forward) is the routing table and packet rewriting
both `l3fwd` examples use, so the same forwarding logic runs over Direct Verbs
and over DPDK. [`internal/metrics`](internal/metrics) reads the port counters and
whole-machine CPU time the benchmarks are built on;
[`internal/frame`](internal/frame) builds the test packets.
