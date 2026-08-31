// Package packetio is a small, backend-neutral Ethernet packet I/O API.
//
// The library moves frames between an application and a NIC with no per-packet
// allocation, and with no copies on every backend whose mechanism allows it --
// of the four, only AF_PACKET copies, because a packet socket is a copy. It is
// deliberately not a networking framework: it owns queues and buffers, and
// nothing above that.
//
// # Backends
//
// A backend implements Device, TxQueue and RxQueue over some kernel or
// hardware mechanism:
//
//	mlx5     NVIDIA/Mellanox ConnectX and BlueField via mlx5 Direct Verbs.
//	         Queue memory and doorbells are mapped into the process, so the
//	         packet path is ordinary loads and stores plus one MMIO write per
//	         batch: no syscall, no library call, no cgo call per packet.
//	afxdp    Linux AF_XDP, an adapter over github.com/atoonk/go-afxdp. Works
//	         on any driver with XDP support, and keeps the interface usable.
//	dpdk     Any NIC DPDK has a poll-mode driver for -- Intel, Broadcom,
//	         virtio, and the ConnectX too. A custom mempool keeps packetio's
//	         frame-ownership model intact under the driver; the packet path
//	         is one cgo crossing per burst, never one per packet. x86-64
//	         only, behind the dpdk build tag.
//	afpacket Linux AF_PACKET: a TPACKET_V3 mmap ring on receive and batched
//	         sendmmsg on transmit. Needs no hardware support and no cgo, and
//	         is far slower than the others. It is the fallback that works
//	         anywhere, and the floor the others are measured against.
//
// # The model
//
// Every backend exposes the same shape, borrowed from AF_XDP because it maps
// cleanly onto hardware descriptor rings as well:
//
//   - A Region is one contiguous chunk of frame memory the NIC can DMA to and
//     from. It is allocated and registered once, at open.
//   - A Desc names one frame inside that Region: an offset and a length. It is
//     a value, not a pointer, and it is the unit of ownership.
//   - Transmit is Alloc, fill, Transmit, Complete. Receive is Fill, Poll,
//     Receive, Recycle. In both directions a frame is owned by exactly one of
//     the pool, the application, or the NIC, and the verbs are the transitions.
//
// # What is deliberately absent
//
// The common API describes packet and buffer movement, not the mechanics of a
// particular kernel interface. AF_XDP's need-wakeup kicks and mlx5's completion
// queue arming are not methods on TxQueue or RxQueue; a backend performs
// whatever its hardware or kernel requires inside Transmit, Poll and friends.
// Where an application genuinely needs backend-specific control, it type
// asserts for an optional interface such as Offload metadata.
//
// # Concurrency
//
// A queue is owned by exactly one goroutine. Two goroutines may drive a TxQueue
// and an RxQueue of the same Device concurrently; two goroutines may not drive
// the same queue. This is what keeps the frame pools free of locks.
package packetio
