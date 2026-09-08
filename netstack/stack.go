//go:build linux

package netstack

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/refs"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/adapters/gonet"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/header"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/link/qdisc/fifo"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/network/arp"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/network/ipv4"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/stack"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/transport/tcp"
)

// nicID is the one NIC a Stack has.
const nicID tcpip.NICID = 1

// Stack is a TCP/IP stack -- gVisor's netstack -- over one packetio.Device.
// Listen and Dial give net.Listener and net.Conn, so what runs on top is
// ordinary Go.
type Stack struct {
	dev  packetio.Device
	cfg  Config
	ep   *endpoint
	s    *stack.Stack
	addr tcpip.Address

	closeOnce sync.Once
	closed    atomic.Bool
}

// initOnce does the process-wide setup the first New needs: gVisor's
// reference leak checker off (a global registry of every packet buffer, a
// locked map insert and delete per packet, visible in a receive profile; a
// debugging aid) and the fork's settings applied.
var initOnce sync.Once

// inUse is every device a stack is up on. A device's queues take one driver
// each; a second stack on the same device would race the first on them.
var inUse = struct {
	sync.Mutex
	devs map[packetio.Device]bool
}{devs: map[packetio.Device]bool{}}

func claim(dev packetio.Device) error {
	inUse.Lock()
	defer inUse.Unlock()
	if inUse.devs[dev] {
		return errors.New("netstack: the device already has a stack on it")
	}
	inUse.devs[dev] = true
	return nil
}

func release(dev packetio.Device) {
	inUse.Lock()
	delete(inUse.devs, dev)
	inUse.Unlock()
}

// New builds a stack over dev and brings it up: the device's queues are
// driven from here on, and the address is answering ARP. The device is not
// closed by Close; it belongs to whoever opened it, and its queues must not
// be used by anything else while the stack is up. One stack per device.
func New(dev packetio.Device, cfg Config) (*Stack, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	if err := claim(dev); err != nil {
		return nil, err
	}
	ep, err := newEndpoint(dev, cfg)
	if err != nil {
		release(dev)
		return nil, err
	}
	cfg = ep.cfg // the MTU now set
	initOnce.Do(func() {
		refs.SetLeakMode(refs.NoLeakChecking)
		applyFork()
	})

	clock := tcpip.NewStdClock()
	if cfg.FastClock {
		clock = newFastClock()
	}
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocolCUBIC},
		Clock:              clock,
	})
	st := &Stack{dev: dev, cfg: cfg, ep: ep, s: s, addr: tcpip.AddrFrom4(cfg.Addr.Addr().As4())}

	// What the endpoint hears from goes into the stack's own ARP cache, so
	// a reply to a peer whose ARP the kernel answered (see the package doc)
	// does not wait for a resolution that would never complete.
	ep.learned = func(ip [4]byte, mac [6]byte, evicted [4]byte, didEvict bool) {
		if didEvict {
			_ = s.RemoveNeighbor(nicID, ipv4.ProtocolNumber, tcpip.AddrFrom4(evicted))
		}
		_ = s.AddStaticNeighbor(nicID, ipv4.ProtocolNumber, tcpip.AddrFrom4(ip), tcpip.LinkAddress(mac[:]))
	}

	ep.afterBatch = func() { drainDeferred(s) }

	// The TCP options first: they must be in place before any endpoint
	// exists. Then the NIC, disabled, so nothing the receive goroutines
	// deliver meanwhile reaches a stack that has no address or routes yet;
	// creating the NIC attaches the endpoint, which starts them. Address,
	// routes and neighbors, then the NIC goes live.
	if err := st.configureTCP(); err != nil {
		s.Destroy()
		release(dev)
		return nil, err
	}
	// One goroutine per transmit queue, gVisor's fifo discipline: each
	// queue is written by exactly one goroutine, in batches. Or, direct,
	// every writer takes the queue's lock itself.
	nicOpts := stack.NICOptions{Name: "pio0", Disabled: true}
	if !cfg.DirectTx {
		nicOpts.QDisc = fifo.New(ep, ep.NumTxQueues(), cfg.TxQueueLen)
	}
	if terr := s.CreateNICWithOptions(nicID, ep, nicOpts); terr != nil {
		s.Destroy()
		release(dev)
		return nil, fmt.Errorf("netstack: create NIC: %v", terr)
	}
	if err := st.configureNIC(); err != nil {
		st.Close()
		return nil, err
	}
	if terr := s.EnableNIC(nicID); terr != nil {
		st.Close()
		return nil, fmt.Errorf("netstack: enable NIC: %v", terr)
	}
	return st, nil
}

// configureTCP sets the stack's TCP options.
func (st *Stack) configureTCP() error {
	cfg, s := &st.cfg, st.s
	// A server that churns connections must be allowed to reuse ports held
	// in TIME-WAIT.
	tw := tcpip.TCPTimeWaitReuseOption(tcpip.TCPTimeWaitReuseGlobal)
	if terr := s.SetTransportProtocolOption(tcp.ProtocolNumber, &tw); terr != nil {
		return fmt.Errorf("netstack: time-wait reuse: %v", terr)
	}
	sb := tcpip.TCPSendBufferSizeRangeOption{Min: minBuffer, Default: cfg.SendBufferSize, Max: max(maxBuffer, cfg.SendBufferSize)}
	if terr := s.SetTransportProtocolOption(tcp.ProtocolNumber, &sb); terr != nil {
		return fmt.Errorf("netstack: send buffer: %v", terr)
	}
	rb := tcpip.TCPReceiveBufferSizeRangeOption{Min: minBuffer, Default: cfg.ReceiveBufferSize, Max: max(maxBuffer, cfg.ReceiveBufferSize)}
	if terr := s.SetTransportProtocolOption(tcp.ProtocolNumber, &rb); terr != nil {
		return fmt.Errorf("netstack: receive buffer: %v", terr)
	}
	return nil
}

// configureNIC gives the fresh NIC its address, routes and pinned neighbors.
func (st *Stack) configureNIC() error {
	cfg, s := &st.cfg, st.s
	if terr := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: st.addr, PrefixLen: cfg.Addr.Bits()},
	}, stack.AddressProperties{}); terr != nil {
		return fmt.Errorf("netstack: add address: %v", terr)
	}

	// The prefix is on-link and resolved by ARP; beyond it, the gateway if
	// there is one, else everything is treated as on-link and the endpoint
	// asks for it directly.
	subnet, err := tcpip.NewSubnet(tcpip.AddrFrom4(cfg.Addr.Masked().Addr().As4()),
		tcpip.MaskFromBytes(net.CIDRMask(cfg.Addr.Bits(), 32)))
	if err != nil {
		return fmt.Errorf("netstack: subnet: %v", err)
	}
	routes := []tcpip.Route{{Destination: subnet, NIC: nicID}}
	if cfg.Gateway.IsValid() {
		routes = append(routes, tcpip.Route{Destination: header.IPv4EmptySubnet, Gateway: tcpip.AddrFrom4(cfg.Gateway.As4()), NIC: nicID})
	} else {
		routes = append(routes, tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: nicID})
	}
	s.SetRouteTable(routes)

	for ip, mac := range cfg.Neighbors {
		if err := st.AddNeighbor(ip, mac); err != nil {
			return err
		}
	}
	return nil
}

// AddNeighbor tells the stack the MAC for ip, for a peer it must send to
// before it has heard from it. The entry is pinned, as Config.Neighbors are.
func (st *Stack) AddNeighbor(ip netip.Addr, mac net.HardwareAddr) error {
	if !ip.Is4() || len(mac) != 6 {
		return fmt.Errorf("netstack: neighbor %v -> %v is not an IPv4 address and a MAC", ip, mac)
	}
	st.ep.AddNeighbor(ip.As4(), [6]byte(mac))
	return nil
}

// Listen announces on the local network address, as net.Listen does. The
// network must be "tcp" or "tcp4"; the address is host:port, where an empty
// host means the stack's own address. Accept on the listener reports
// net.ErrClosed once the listener or the stack is closed, as net.Listen's
// does, so an ordinary accept loop ends rather than reporting a failure.
func (st *Stack) Listen(network, address string) (net.Listener, error) {
	if st.closed.Load() {
		return nil, fmt.Errorf("netstack: listen %s: %w", address, net.ErrClosed)
	}
	fa, err := st.fullAddress(network, address, true)
	if err != nil {
		return nil, err
	}
	ln, err := gonet.ListenTCP(st.s, fa, ipv4.ProtocolNumber)
	if err != nil {
		return nil, fmt.Errorf("netstack: listen %s: %w", address, err)
	}
	return &listener{Listener: ln, st: st}, nil
}

// Dial connects to the address on the named network, as net.Dialer.DialContext
// does. The peer's MAC is resolved by ARP unless the stack already knows it.
func (st *Stack) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	if st.closed.Load() {
		return nil, fmt.Errorf("netstack: dial %s: %w", address, net.ErrClosed)
	}
	fa, err := st.fullAddress(network, address, false)
	if err != nil {
		return nil, err
	}
	c, err := gonet.DialContextTCP(ctx, st.s, fa, ipv4.ProtocolNumber)
	if err != nil {
		return nil, fmt.Errorf("netstack: dial %s: %w", address, err)
	}
	return &conn{Conn: c}, nil
}

func (st *Stack) fullAddress(network, address string, local bool) (tcpip.FullAddress, error) {
	switch network {
	case "tcp", "tcp4":
	default:
		return tcpip.FullAddress{}, fmt.Errorf("netstack: network %q is not supported; use tcp", network)
	}
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return tcpip.FullAddress{}, fmt.Errorf("netstack: %w", err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return tcpip.FullAddress{}, fmt.Errorf("netstack: port %q: %w", portStr, err)
	}
	fa := tcpip.FullAddress{Port: uint16(port)}
	if host == "" {
		if !local {
			return tcpip.FullAddress{}, errors.New("netstack: dial needs a host")
		}
		fa.NIC, fa.Addr = nicID, st.addr
		return fa, nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.Is4() {
		return tcpip.FullAddress{}, fmt.Errorf("netstack: %q is not an IPv4 address (names are not resolved here)", host)
	}
	fa.Addr = tcpip.AddrFrom4(ip.As4())
	if local {
		fa.NIC = nicID
	}
	return fa, nil
}

// Stats returns the endpoint's counters, the device's own, and what the
// transmit queueing discipline dropped. TCPIP gives the stack's own.
func (st *Stack) Stats() Stats {
	s := st.ep.Stats()
	// The queueing discipline is inside the NIC, so its drops are the
	// stack's to report. They are the ones that fill first when the wire
	// cannot keep up, and the endpoint never sees the packets.
	if info, ok := st.s.NICInfo()[nicID]; ok {
		s.QDiscDropped = uint64(info.Stats.TxPacketsDroppedNoBufferSpace.Value())
	}
	return s
}

// TCPIP is the gVisor stack underneath, for what this package does not name.
func (st *Stack) TCPIP() *stack.Stack { return st.s }

// Err returns the first device failure the stack hit, or nil. A queue that
// fails (the interface went away, say) is logged and not driven again; the
// stack goes on, with connections timing out. Done is the same news as a
// channel.
func (st *Stack) Err() error { return st.ep.Err() }

// Done is closed when a queue of the device fails.
func (st *Stack) Done() <-chan struct{} { return st.ep.Done() }

// Close stops the stack. Connections still open are reset, the peers told;
// then the receive goroutines, the stack's own goroutines and transmit are
// stopped, in that order, so that when Close returns nothing is inside a
// queue of the device and the device may be closed. The device is left open.
func (st *Stack) Close() error {
	st.closed.Store(true)
	st.closeOnce.Do(func() {
		// The receive loops stop first: Destroy holds the stack's lock
		// while it detaches the endpoint, and a loop that is inside the
		// stack delivering a packet needs that lock to finish. Transmit
		// stays open through Destroy, which is when the resets go out;
		// Destroy ends by closing the endpoint, which closes transmit.
		st.ep.stopReceive()
		st.s.Destroy()
		st.ep.Close()
		release(st.dev)
	})
	return nil
}
