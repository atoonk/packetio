# DPDK on EC2: the ENA

An EC2 instance is the exclusive-device path of [README.md](README.md) with three
differences, and each of them is invisible until it bites: Nitro exposes **no
IOMMU**, so the environment addresses memory **physically**; the ENA driver has
**no promiscuous mode**; and the VPC **drops any frame that does not carry the
instance's own source MAC and IP**. Everything below was run on a `c6i.xlarge`
(4 vCPU, Amazon Linux 2023, kernel 6.18, DPDK 23.11.4) sending to a second
instance on the same subnet.

The short version, for someone with the
[blast gist](https://gist.github.com/atoonk/63ad7ca0c7e4c0e0b167695b311a455e)
in one window:

```bash
# once: DPDK from source (AL2023 has no package), and a second ENI; see below

# each boot, in this order
sudo dpdk-hugepages.py -p 2M --setup 512M   # hugepages do not survive a reboot
sudo modprobe vfio-pci
echo 1 | sudo tee /sys/module/vfio/parameters/enable_unsafe_noiommu_mode
sudo systemctl stop systemd-networkd     # or whatever owns the interface
sudo ip addr flush dev ens6 && sudo ip link set ens6 down
sudo dpdk-devbind.py --force --bind=vfio-pci 0000:00:06.0

# in the gist: device = "0000:00:06.0", src MAC and IP = this ENI's,
# dst MAC = the peer's (or the subnet router's), queues = 2
PKG_CONFIG_PATH=/usr/local/lib64/pkgconfig go build -tags dpdk -o blast .
sudo ./blast
```

The rest of this page is why each line is there.

## The instance

**Two network interfaces.** DPDK takes its device away from the kernel
entirely, and on EC2 the kernel's interface is how you are logged in. Attach a
second ENI, on its own subnet, and only ever hand *that* one over. Binding
`0000:00:05.0` -- the primary interface on every instance type this was tried
on -- takes the instance off the network until someone in the console gives it
back. On these instances the second ENI was `0000:00:06.0`, `ens6`;
`dpdk-devbind.py --status-dev net` shows which is which.

**Source/destination check.** Off, on the DPDK ENI, if the program will ever
send from or to an address that is not the ENI's own -- a router, a load
balancer. Not needed for the gist.

**Cores.** The ENA offers as many queues as the instance has vCPUs
(`max_queues=4` on a `c6i.xlarge`), and each queue busy-polls a core. Leave one
for the rest of the machine: `queues = 2` on four vCPUs. With every core polling,
SSH still works but is sluggish -- the NIC is fine, `ens5` dropped nothing in
these runs, it is the scheduler that has nowhere to put `sshd`.

## DPDK

Amazon Linux 2023 ships no DPDK package. Building 23.11 from source with only
the drivers needed takes a few minutes on four cores:

```bash
sudo dnf install -y gcc make meson ninja-build numactl-devel python3-pip \
    tar xz libpcap-devel python3-pyelftools
curl -fsSL https://fast.dpdk.org/rel/dpdk-23.11.4.tar.xz | tar xJ
cd dpdk-stable-23.11.4
meson setup build -Dplatform=generic -Dtests=false \
    -Denable_drivers=net/ena,net/af_packet,net/null,net/tap
ninja -C build && sudo ninja -C build install && sudo ldconfig
```

It lands in `/usr/local`, which `pkg-config` does not look in by default, so
every `go build -tags dpdk` wants:

```bash
export PKG_CONFIG_PATH=/usr/local/lib64/pkgconfig
```

(Ubuntu on EC2 can `apt install libdpdk-dev dpdk` as usual; the ENA driver is
in the package.)

## Memory: no IOMMU, so physical addresses, so hugepages

`/sys/kernel/iommu_groups` is empty on Nitro. There is no IOMMU for `vfio-pci`
to program, so it has to be told that is acceptable:

```bash
sudo modprobe vfio-pci
echo 1 | sudo tee /sys/module/vfio/parameters/enable_unsafe_noiommu_mode
```

The parameter exists only once the module is loaded, and the bind below has to
come after it: bound first, the device lands on nothing and the environment
reports `probing 0000:00:06.0: -19 (No such device)`.

Without an IOMMU the NIC reads and writes **physical** addresses -- `info`
reports `addressed by physical`, DPDK calls it IOVA mode PA -- and that has two
consequences:

- **Hugepages are not optional.** Ordinary memory is neither pinned nor
  translatable for the device, and the environment refuses to start without
  them. `WithoutHugePages` is for virtual devices only.
- **Root.** Physical addresses are read from `/proc/self/pagemap`, which needs
  `CAP_SYS_ADMIN`; the `CAP_NET_RAW` + `CAP_IPC_LOCK` that suffices behind an
  IOMMU does not here.

Either size of page works. 2 MB pages need no reboot, but they need reserving
again after one (`vm.nr_hugepages` in `/etc/sysctl.d/` makes that automatic):

```bash
sudo dpdk-hugepages.py -p 2M --setup 512M     # mounts /dev/hugepages too
```

and 1 GB pages want a kernel command line and a reboot, after which they are
always there:

```bash
sudo grubby --update-kernel=ALL \
    --args="default_hugepagesz=1G hugepagesz=1G hugepages=2"
sudo reboot
```

The frame region does not need to be physically contiguous -- the memory is
handed to the driver a page at a time -- so 16 MB of frames on 2 MB pages is
fine. (Releases up to v0.1.3 reserved the region as one physically contiguous
block, which 2 MB pages rarely supply, and failed with `Cannot allocate
memory`; 1 GB pages were the workaround.)

## Handing the device over, and back

```bash
sudo systemctl stop systemd-networkd     # it would otherwise re-address ens6
sudo ip addr flush dev ens6
sudo ip link set ens6 down
sudo dpdk-devbind.py --force --bind=vfio-pci 0000:00:06.0
sudo dpdk-devbind.py --status-dev net    # drv=vfio-pci
```

`--force` because the interface was up a moment ago. To give it back:

```bash
sudo dpdk-devbind.py --bind=ena 0000:00:06.0
sudo systemctl restart systemd-networkd
```

The address comes back from the instance metadata within a few seconds.

## What is different once it runs

**No promiscuous mode.** The ENA driver does not have one, and inside a VPC
there is nothing it would admit: the fabric only delivers frames addressed to
the ENI. A `SteeringFilter{Promiscuous: true}` or `WithPromiscuous()` is
therefore accepted, `Info().Promiscuous` says `false`, and `Info().Steering`
reads "whatever the port's own address filter admits (this driver has no
promiscuous mode to widen it)". `dpdk-testpmd` logs the same thing and moves on.

**The VPC checks the source of every frame.** A frame whose source MAC is not
the ENI's, or whose source IP is not one of the ENI's, is dropped in the
fabric, silently, with the transmit counters advancing. So in the gist: the
source MAC is the ENI's (`Info().MAC` has it), the source IP is the ENI's
private address, and a generator that wants many flows varies the *ports*, not
the addresses. The destination for anything off the subnet is the subnet
router's MAC -- `ip neigh show` for `x.x.x.1`, read before the interface is
handed over, since the kernel's ARP cache goes with it.

**The IPv4 header checksum has to be right.** The receiving kernel drops a
packet with a bad one, silently. The gist ships with a checksum for its own
addresses; change the addresses and recompute it (a ten-line function, or
`WithChecksumOffload()` and let the card).

**No link speed.** The ENA does not report one: `Info().SpeedMbps` is 0 and
`info` prints none. `Info().LinkUp` is the part that matters.

**Rates are the instance's, not the NIC's.** A `c6i.xlarge` sent about
1.0 Mpps of 64-byte frames whether one queue or two were driving it, and the
peer -- another `c6i.xlarge` -- captured 4.3 M of every 5 M sent, with
`pps_allowance_exceeded` in its `ethtool -S ens6` at 1.4 M after the first run.
An instance has a packets-per-second allowance, and a small one is below what
a single DPDK core produces; larger instances have larger allowances, and
`ethtool -S` on the kernel side (or the ENA's xstats) is where the shortfall
shows.

**One Open per process.** After `Close`, opening the same ENA again in the same
process fails: the driver's re-probe cannot allocate its first memzone
(`ena_mem_alloc_coherent(): Failed to allocate ena_com memzone: ena_p0_mz0`).
Open once and keep it; a daemon that restarts its dataplane should restart the
process. This is the ENA driver in DPDK 23.11, not something this package can
work around, and it is why the open/close-in-a-loop tests fail with
`PACKETIO_DPDK_DEV` pointing at an ENA.

**LLQ** (the ENA's low-latency transmit queue) is on by default and is what
the numbers above were measured with; `WithDevArgs("enable_llq=0")` turns it
off, and the ENA guide says that costs a lot on 6th-generation instances.

## Checking it works

Run these from the packetio repository on the instance, with the ENA bound.
`info` first: it opens the device and prints who owns it and how memory is
addressed, and it is where a bind that landed on nothing shows up.

`sudo` drops the environment, `PKG_CONFIG_PATH` included, so build the
examples as yourself and run the binaries as root:

```bash
go build -tags dpdk -o bin/ ./examples/dpdk/...
sudo bin/info -i 0000:00:06.0
```

Then receive. On the peer, the kernel no longer answers ARP for the DPDK
instance's address, so give it the MAC by hand and ping:

```bash
# on the peer
sudo ip neigh replace 10.0.2.132 lladdr 0e:07:51:17:6a:d5 dev ens6 nud permanent
ping 10.0.2.132

# on the DPDK instance
sudo bin/drop -i 0000:00:06.0 -dump 2
```

`-dump` prints the first packets as hex. The point of it is the one thing the
counters cannot tell you: that the bytes are the bytes. A device given
addresses it cannot see reports every length correctly and delivers zeros.

Then send, with the ENI's own addresses as the source:

```bash
sudo bin/blast -i 0000:00:06.0 \
    -src-mac 0e:07:51:17:6a:d5 -dst-mac 0e:81:d4:de:b1:b7 \
    -src-ip 10.0.2.132 -dst-ip 10.0.2.196 -dst-port 9000 -queues 1
# on the peer
sudo tcpdump -i ens6 -nn udp and dst port 9000
```

Jumbo frames need a frame that holds them and an MTU the port is configured
for; both examples take the two flags, and this pair was verified on the ENA
both ways:

```bash
sudo bin/drop -i 0000:00:06.0 -frame-size 16384 -mtu 9001 -dump 1
sudo bin/blast -i 0000:00:06.0 -frame-size 16384 -mtu 9001 -size 9018 ...
```

And the gist, with its three constants changed, is the same thing in forty
lines of your own.
