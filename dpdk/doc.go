// Package dpdk moves Ethernet frames through any NIC DPDK has a driver for.
//
// It is the portable fast backend. Where [github.com/atoonk/packetio/mlx5]
// needs a ConnectX and AF_XDP needs the kernel in the path, this needs only a
// poll-mode driver -- Intel, Broadcom, virtio, and the same ConnectX -- at the
// cost of a build that links against DPDK and a machine set up for it.
//
// # What it costs
//
// The packet path crosses into C three times a batch: one burst each way, plus
// a poke when a transmit queue has gone idle. A crossing measures about 36
// nanoseconds on the hardware here, so at a batch of 64 it is under a
// nanosecond a packet. Nothing is per packet.
//
// # What is different from the other backends
//
//   - A frame is not all packet. DPDK describes every buffer with an rte_mbuf,
//     and here the mbufs live inside the frame region: each frame holds the
//     mempool's object header, the mbuf, the headroom and then the packet, 320
//     bytes in. Capabilities reports what is left as MaxFrameSize, and a
//     descriptor reaching back past the headroom -- into the object header or
//     the mbuf, the first 192 bytes -- is refused.
//
//   - Frames come back late. A driver reads its transmit completions inside a
//     burst and nowhere else, and not until enough packets have gone since the
//     last time -- 32 on mlx5. So a queue that stops sending mid-batch keeps
//     those frames until it sends more. Complete and Reclaim poke the driver
//     when the queue is otherwise idle, which is as much as DPDK allows.
//
//   - Who owns the device decides what steering means. See [WithSteering] and
//     [Info.Coexists].
//
// # What it needs
//
// A build with the dpdk tag and libdpdk installed; hugepages, unless the device
// is a virtual one opened with [WithoutHugePages]; and for a device this
// process owns outright, that device bound to vfio-pci and an IOMMU. A device
// still bound to a kernel driver is refused with the command that would rebind
// it -- this library never rebinds anything itself.
//
// # Building
//
// The package is behind the dpdk build tag, which keeps cgo and DPDK out of
// everyone else's build:
//
//	go build -tags dpdk ./...
//
// Without the tag the package has no declarations, so pkg.go.dev shows this
// text and nothing else; go doc has no way to be told about a tag. The API is
// described in https://github.com/atoonk/packetio/tree/main/dpdk and, built
// with the tag, is what any editor or go list -tags dpdk will show.
//
// DPDK 23.11 or later. Building needs apt install libdpdk-dev on Ubuntu 24.04;
// running a binary built elsewhere needs apt install dpdk, which is where the
// librte_* libraries live. The poll-mode drivers are plugins loaded at startup,
// so installing the package is enough and there is nothing to compile.
//
// Opening a device needs root, or CAP_NET_RAW and CAP_IPC_LOCK with access to
// /dev/vfio.
package dpdk
