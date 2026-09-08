//go:build linux

// Package netstack runs a TCP/IP stack -- gVisor's netstack -- over any
// packetio device, so a program that takes packets off the wire at line rate
// can also speak TCP on them, in userspace, with the kernel nowhere on the
// path. Listen and Dial give net.Listener and net.Conn; what runs on top is
// ordinary Go.
//
//	dev, _ := afpacket.Open("eth1")
//	st, _ := netstack.New(dev, netstack.Config{Addr: netip.MustParsePrefix("10.0.0.2/24"), MAC: mac})
//	ln, _ := st.Listen("tcp", ":8080")
//	for { c, _ := ln.Accept(); go io.Copy(c, c) }
//
// The same program runs on afxdp, dpdk, mlx5 and afpacket; the backend is
// the Open call. The stack does ARP, IPv4 and TCP; the endpoint underneath
// does Ethernet and 802.1Q, segments large packets into frames in place
// (GSO), optionally merges arriving segments (GRO), and finishes checksums
// in software.
//
// # Where the bytes go
//
// gVisor owns every byte the stack touches: a received frame is copied once
// into a pooled view and goes straight back to the receive ring; on transmit
// a packet's views are copied once, straight into one of the device's frames.
// One copy each way is the floor gVisor's API sets. Everything else the
// backend buys is kept: no per-packet syscalls, batched rings, no sk_buff.
//
// # The address, and who answers ARP
//
// The stack answers ARP for its own address and asks for its peers'. Whether
// it ever sees an ARP frame depends on the backend, and so does which
// address it may use:
//
//   - afpacket has no steering: the kernel sees every packet too. Give the
//     stack an address the kernel does not own (an unnumbered interface, or
//     a second address on the subnet not configured on the interface); the
//     kernel then answers neither ARP nor TCP for it. A kernel-owned address
//     gets a kernel RST for every SYN.
//   - afxdp and mlx5 keep the kernel's interface. With a steering filter for
//     the TCP port alone, the kernel goes on answering ARP for its own address
//     and the stack may share it -- the endpoint learns each peer's MAC from
//     its first frame. To give the stack an address of its own, steer ARP to
//     it as well: on afxdp afxdp.WithXDP(xdp.WithFilter(
//     xdp.MatchEtherType(0x0806), xdp.MatchTCPPort(port))); on mlx5 a whole
//     VLAN, MatchVLAN(id) with MatchDstMAC for the port's own MAC and for
//     broadcast, which the kernel has no interface on.
//   - dpdk on a device bound to vfio-pci (EC2's ENA among them) has no
//     kernel: every frame arrives here and the stack does ARP itself. On a
//     bifurcated card or a vdev it behaves like afxdp or afpacket respectively.
//
// A stack that dials needs the replies steered to it: by the peer's port, or
// all of TCP.
//
// The endpoint learns a peer's MAC from the frames the peer sends to the
// stack, which is what lets the kernel answer ARP on the stack's behalf.
// That learning is as trustworthy as ARP: unauthenticated. It is bounded --
// only frames to the stack's own MAC and address, from its own prefix, with
// good checksums; never over an entry from Config.Neighbors, which are
// pinned -- but on a link shared with hosts that are not trusted, the
// gateway belongs in Config.Neighbors.
//
// # Queues
//
// One receive goroutine per receive queue delivers into the stack; one
// transmit goroutine per transmit queue (gVisor's fifo queueing discipline)
// writes out of it.
//
// The receive goroutine does more than deliver. The patched gVisor this
// module carries processes an established connection's segments on the
// goroutine that delivered them, rather than waking a worker per segment,
// and runs the work it cannot do there -- handshakes, closes -- after each
// batch on that same goroutine. So a receive goroutine runs TCP, and with
// Config.DirectTx it transmits the replies too: for one connection's
// traffic it is the whole stack. That is where the speed comes from, and
// it is why anything expensive in a caller's handler belongs on a
// goroutine of the caller's own, not inline in the read loop. Each packetio queue is driven by exactly one goroutine,
// as packetio requires, and the caller's own goroutines never touch the
// device. On a backend that cannot sleep in Poll (dpdk, mlx5) a quiet
// receive goroutine backs off to short sleeps; Config.BusyPoll keeps it
// spinning. Config.DirectTx does without the transmit goroutines: whatever
// goroutine produced a packet transmits it, under a per-queue lock.
//
// One stack per device. Close resets the connections still open, then
// stops the goroutines; when it returns nothing is inside a queue, and the
// device may be closed. A queue that fails is reported by Err and Done.
package netstack
