//go:build linux

package netstack

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
)

// Config configures a Stack. Addr and MAC are required; everything else has
// a default that works.
type Config struct {
	// Addr is the stack's IPv4 address with its on-link prefix. See the
	// package doc for which address to use on each backend: on a device the
	// kernel still owns it must be one the kernel does not answer for.
	Addr netip.Prefix

	// MAC is the address frames are sent from and that inbound frames are
	// classified against. Ordinarily the interface's own; the example takes
	// it from net.InterfaceByName, and dpdk/mlx5 report it in their Info.
	MAC net.HardwareAddr

	// VLAN, when nonzero, is the 802.1Q tag every frame must carry and every
	// transmitted frame gets.
	VLAN uint16

	// MTU is the IP MTU. Zero means 1500, or less where a frame cannot hold
	// that much (packetio.Capabilities.MaxFrameSize is the ceiling).
	MTU int

	// Gateway, when set, is the next hop for every destination off the
	// prefix. Unset, everything is treated as on-link and resolved by ARP.
	Gateway netip.Addr

	// Neighbors seeds the MAC table with peers this stack must send to
	// before it has heard from them, such as the gateway on a backend where
	// the kernel answers ARP and the stack never sees a reply. An entry given
	// here is pinned: nothing heard on the wire changes it. Entries the stack
	// learns on its own, from frames addressed to it, are as trustworthy as
	// ARP is -- a host on the link can claim another's address -- so on a
	// shared link the gateway belongs here.
	Neighbors map[netip.Addr]net.HardwareAddr

	// NoGSO turns segmentation offload off: TCP then hands the endpoint one
	// packet per MSS instead of one of up to 64 KiB that the endpoint splits
	// in place. GSO is on unless this is set, because it is a large part of
	// the throughput.
	NoGSO bool

	// NoGRO turns off gVisor's software receive offload, which merges a
	// burst of one flow's segments into one packet before the stack sees it.
	// GRO is on unless this is set, because a peer at line rate sends a
	// segment per stack trip otherwise, and one TCP goroutine cannot keep
	// up: measured on an ENA, 2.6 Gbit/s without it and 6.3 with. The
	// endpoint verifies every frame's checksums itself before merging.
	NoGRO bool

	// TxQueueLen is how many packets may wait for each transmit queue's
	// goroutine. Zero means 1000.
	TxQueueLen int

	// DirectTx makes the stack transmit from whatever goroutine produced a
	// packet, one packet at a time under a per-queue lock, instead of
	// handing it to a transmit goroutine per queue (gVisor's fifo
	// discipline) that batches. No handoff, no batching: lower latency for
	// request/response, and with the gVisor fork's inline processing the
	// reply to a segment leaves on the goroutine that received it (166k
	// against 125k req/s on an ENA with the fork).
	//
	// On dpdk and mlx5, whose queues place the goroutine that first
	// transmits on the queue's own CPU and leave it there, that goroutine
	// is then whichever one produced the packet -- a timer, or one of the
	// caller's. Open those devices with their placement option off when
	// setting this.
	DirectTx bool

	// SendBufferSize and ReceiveBufferSize are the TCP defaults, in bytes.
	// Zero means 4 MiB each.
	SendBufferSize, ReceiveBufferSize int

	// BusyPoll keeps a receive goroutine spinning when the queue is idle, on
	// a backend that cannot sleep in Poll (dpdk, mlx5). Off, such a goroutine
	// backs off to short sleeps after a quiet spell, which costs latency on
	// the first packet after it and saves a core the rest of the time.
	BusyPoll bool

	// FastClock reads the monotonic clock through the runtime's nanotime
	// rather than time.Now, which TCP asks for several times per segment. It
	// relies on a go:linkname into the runtime; off by default.
	FastClock bool

	// Logf, when set, receives the endpoint's own messages: a receive queue
	// that failed, and nothing on the packet path. Nil means log.Printf.
	Logf func(format string, args ...any)
}

const (
	defaultMTU = 1500
	// minMSS is the smallest segment a peer may ask for: what a GSO packet
	// is cut into, in the worst case, when bounding it against the queue.
	minMSS            = 536
	defaultTxQueueLen = 1000
	maxTxQueueLen     = 1 << 20
	defaultBuffer     = 4 << 20
	minBuffer         = 4 << 10
	maxBuffer         = 32 << 20
)

// withDefaults checks the Config and returns it with the defaults filled in.
func (c Config) withDefaults() (Config, error) {
	if !c.Addr.IsValid() || !c.Addr.Addr().Is4() {
		return c, errors.New("netstack: Addr must be an IPv4 prefix")
	}
	// An address no host may answer for is a stack that transmits happily
	// and hears nothing, with every counter at zero: worth refusing here
	// rather than leaving someone to find it on the wire.
	switch a := c.Addr.Addr(); {
	case a.IsUnspecified() || a.IsLoopback() || a.IsMulticast():
		return c, fmt.Errorf("netstack: Addr %v is not a host address", a)
	case c.Addr.Bits() < 31 && a == c.Addr.Masked().Addr():
		return c, fmt.Errorf("netstack: Addr %v is the network address of %v", a, c.Addr.Masked())
	case c.Addr.Bits() < 31 && a == broadcastOf(c.Addr):
		return c, fmt.Errorf("netstack: Addr %v is the broadcast address of %v", a, c.Addr.Masked())
	}
	if len(c.MAC) != 6 {
		return c, fmt.Errorf("netstack: MAC must be 6 bytes, got %v", c.MAC)
	}
	// A group address as a source is not one host's own: switches may drop
	// such frames and no peer will answer them.
	if c.MAC[0]&1 != 0 {
		return c, fmt.Errorf("netstack: MAC %v is a group address, not a host's own", c.MAC)
	}
	if (c.MAC[0] | c.MAC[1] | c.MAC[2] | c.MAC[3] | c.MAC[4] | c.MAC[5]) == 0 {
		return c, errors.New("netstack: MAC is all zeros")
	}
	if c.VLAN > 4094 {
		return c, fmt.Errorf("netstack: VLAN %d is out of range", c.VLAN)
	}
	if c.MTU < 0 {
		return c, fmt.Errorf("netstack: MTU %d is negative", c.MTU)
	}
	if c.Gateway.IsValid() {
		if !c.Gateway.Is4() {
			return c, errors.New("netstack: Gateway must be IPv4")
		}
		if !c.Addr.Contains(c.Gateway) {
			return c, fmt.Errorf("netstack: gateway %v is not on %v", c.Gateway, c.Addr)
		}
	}
	for ip, mac := range c.Neighbors {
		if !ip.Is4() || len(mac) != 6 {
			return c, fmt.Errorf("netstack: neighbor %v -> %v is not an IPv4 address and a MAC", ip, mac)
		}
	}
	// The queue length is a slot per packet, per transmit queue, allocated
	// up front. Unbounded it is an out-of-memory the caller cannot recover
	// from rather than an error it can read.
	switch {
	case c.TxQueueLen < 0 || c.TxQueueLen > maxTxQueueLen:
		return c, fmt.Errorf("netstack: TxQueueLen %d is out of range; 0 for the default (%d), up to %d",
			c.TxQueueLen, defaultTxQueueLen, maxTxQueueLen)
	case c.TxQueueLen == 0:
		c.TxQueueLen = defaultTxQueueLen
	}
	for _, b := range []struct {
		name string
		v    *int
	}{{"SendBufferSize", &c.SendBufferSize}, {"ReceiveBufferSize", &c.ReceiveBufferSize}} {
		switch {
		case *b.v == 0:
			*b.v = defaultBuffer
		case *b.v < minBuffer || *b.v > maxBuffer:
			return c, fmt.Errorf("netstack: %s %d is out of range; 0 for the default (%d), or %d to %d",
				b.name, *b.v, defaultBuffer, minBuffer, maxBuffer)
		}
	}
	return c, nil
}

// broadcastOf is the all-ones address of a prefix.
func broadcastOf(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	for i, m := range net.CIDRMask(p.Bits(), 32) {
		a[i] |= ^m
	}
	return netip.AddrFrom4(a)
}
