//go:build linux && cgo && mlx5 && (amd64 || arm64)

package mlx5

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/mlx5/internal/dv"
)

// Device is an open NIC: a registered region of frame memory and the queues
// that move packets through it.
type Device struct {
	dev    *dv.Device
	region *region

	// clock converts a completion's timestamp to nanoseconds. Mult is zero on
	// a device that does not report a clock, and then the queues do not offer
	// timestamps at all.
	clock dv.ClockInfo
	tx    []*TxQueue
	rx    []*RxQueue
	rxg   *dv.RxGroup
	info  Info
	cfg   config
	place *placement
	// closeMu makes Close idempotent against a concurrent second call, which
	// would otherwise both pass the flag and destroy the queues twice.
	closeMu sync.Mutex
	closed  bool
}

// Info describes an open device.
type Info struct {
	// Interface is the network interface asked for, IBDev the verbs device
	// that carries it and Port its port.
	Interface string
	IBDev     string
	Port      uint32

	// Firmware is the NIC's firmware version, and PortActive whether the link
	// is up. A device with the link down opens and transmits nothing.
	Firmware   string
	PortActive bool

	// InlineHeader is how many bytes of each packet go inside its work queue
	// entry, and RequiredInline the least the device would accept.
	InlineHeader   int
	RequiredInline int

	// EnhancedMPW reports whether the NIC can carry several packets in one
	// descriptor, and MultiPacket whether this device is doing so.
	EnhancedMPW bool
	MultiPacket bool

	// MultiPacketPointer says those descriptors point at their packets rather
	// than carrying copies, which is the default and the fast form: the card
	// drains pointer entries about three times faster.
	MultiPacketPointer bool

	// MultiPacketMaxLen is the longest packet carried that way; longer ones go
	// in ordinary descriptors.
	MultiPacketMaxLen int

	// FrameSize, Frames and the queue counts and depths are what was opened.
	FrameSize int
	Frames    int
	TxQueues  int
	TxDepth   int
	RxQueues  int
	RxDepth   int

	// RingsPerQueue is how many hardware send queues sit behind each
	// TxQueue. It is more than one in copied multi-packet mode, where the
	// card's per-send-queue drain rate is the limit and Transmit deals
	// batches over the rings round-robin.
	RingsPerQueue int

	// Steering describes what the receive queues were told to take.
	Steering string

	// MAC is the interface's own Ethernet address.
	MAC [6]byte

	// HugePages reports whether the frame region got the huge pages it asked
	// for.
	HugePages bool

	// Placement describes where this device's workers will be put.
	Placement string

	// LKey is the memory key of the registered region, and RegionVA the
	// address the NIC knows it by. They are here because they are the first
	// things to check against a packet capture when a frame does not arrive.
	LKey     uint32
	RegionVA uint64
}

// Open opens the NIC behind a network interface and creates its queues.
//
// The interface is the physical one, such as ens1f0np0, not a VLAN or bond on
// top of it: those are kernel constructs and this bypasses the kernel. Tagged
// traffic is a matter of what goes in the frame, not of which interface is
// opened.
func Open(ifname string, opts ...Option) (d *Device, err error) {
	cfg := defaults()
	for _, o := range opts {
		o(&cfg)
	}

	// Options are checked before the hardware is looked for, so that a bad
	// option says so. The other way round, every option error on a machine
	// without the card came back as "no verbs device", which is true and
	// useless: it hides the mistake the caller can actually fix, and makes
	// every option error untestable without a ConnectX.
	//
	// validate takes the device name only for its error messages, and is
	// called again below once that is known.
	if err := cfg.validate(ifname); err != nil {
		return nil, err
	}
	ibdev, port, err := dv.Resolve(ifname)
	if err != nil {
		return nil, err
	}
	if err := cfg.validate(ibdev); err != nil {
		return nil, err
	}

	dev, err := dv.Open(ibdev, port)
	if err != nil {
		return nil, withPrivilegeHint(err)
	}
	d = &Device{dev: dev, cfg: cfg}
	// Anything that fails from here leaves nothing behind: a half-open device
	// holds pinned memory and a queue pair the NIC may still be reading.
	//
	// The cleanup holds its own reference. Returning "nil, err" assigns the
	// named result before the deferred function runs, so a defer that reached
	// for d would find nothing to close and would take the process down
	// instead of reporting the error.
	half := d
	defer func() {
		if err != nil {
			half.Close()
			d = nil
		}
	}()

	caps := d.dev.Caps()

	// Whether transmit uses copied multi-packet entries, and how many rings
	// back each queue, decide how big the frame region must be. The last
	// input to that choice -- the port's required inline length -- can only
	// be probed once a region is registered, so the choice is made
	// provisionally from the device capabilities, the region is sized from
	// it, and the probe below can only downgrade it, to pointer entries and
	// one ring, on the unusual port that demands inline header bytes. Such a
	// port gets a region a little larger than it needs, which is the
	// harmless direction.
	//
	// The default is decided by what measured fastest per core, because most
	// callers are not descriptor-format experts and should not have to be.
	// One transmit queue: pointer entries, which one core drives at 66-67
	// Mpps -- this card's best single-core number. Two queues or more, on a
	// transmit-only device: the caller has said throughput matters, and
	// pointer entries share one ~76 Mpps device-wide ceiling however many
	// queues there are, so the device switches to copied entries, whose
	// ceiling is per send queue and multiplies. WithMultiPacket and
	// WithoutMultiPacket still override.
	//
	// A device with receive queues is usually a forwarder, and forwarding
	// used to be receive-cost-bound -- but that cost was the cache-cold read
	// of each arriving frame, and the receive ring now prefetches those (see
	// Rx.Receive). What limits forwarding since is the same ~76 Mpps pointer
	// wall as transmit, so the same escape applies from four queues up:
	// copied entries, measured 80.0 against pointer's 69.8 Mpps at four
	// workers and 139.3 against 68.5 at eight. Below four the wall is out of
	// reach and pointer entries win or tie (29.7/58.4/65 Mpps on 1-3 cores
	// against copied 23.7/42/64), so they stay.
	autoCopy := !cfg.mpwSet && caps.EnhancedMPW() &&
		(cfg.rxQueues == 0 && cfg.txQueues >= 2 ||
			cfg.rxQueues > 0 && cfg.txQueues >= 4)
	copied := autoCopy || (cfg.mpwSet && cfg.multiPacket)
	rings := cfg.ringsPerQueue
	if rings == 0 {
		rings = 1
		if copied {
			rings = ringsPerCopiedQueue
		}
	}
	if cfg.txQueues*rings > maxQueues {
		return nil, fmt.Errorf("mlx5: %d transmit queues of %d rings each is %d send queues, "+
			"and %d is the most this backend opens", cfg.txQueues, rings, cfg.txQueues*rings, maxQueues)
	}

	// The frames have to cover every ring twice over, or a queue can never
	// keep its rings full. A caller who set the count is refused if it cannot
	// work; everyone else gets it sized here.
	need := cfg.txQueues*rings*cfg.txDepth + cfg.rxQueues*cfg.rxDepth
	if cfg.framesSet {
		if cfg.frames < need {
			return nil, fmt.Errorf("mlx5: %d frames is not enough for %d transmit rings of %d "+
				"and %d receive queues of %d; at least %d are needed",
				cfg.frames, cfg.txQueues*rings, cfg.txDepth, cfg.rxQueues, cfg.rxDepth, need)
		}
	} else if cfg.frames < 2*need {
		f := nextPow2(2 * need)
		if f > maxFrames {
			return nil, fmt.Errorf("mlx5: %d transmit rings of %d and %d receive queues of %d "+
				"want %d frames, more than this backend registers; open fewer queues or size "+
				"the region yourself with WithFrames", cfg.txQueues*rings, cfg.txDepth,
				cfg.rxQueues, cfg.rxDepth, 2*need)
		}
		cfg.frames = f
	}
	d.cfg.frames = cfg.frames

	if d.region, err = newRegion(cfg.frames, cfg.frameSize, cfg.hugePages); err != nil {
		return nil, err
	}
	lkey, err := d.dev.RegisterRegion(d.region.b)
	if err != nil {
		return nil, fmt.Errorf("%w; is the memory lock limit high enough for %d bytes?",
			err, len(d.region.b))
	}

	// How much of each packet the device insists on seeing inside the work
	// queue entry. It is a property of the port, not of the packet, and the
	// only way to learn it is to let the provider lay out a send and look --
	// which needs the region registered, and is why the sizing above was
	// provisional.
	// How the NIC's completion timestamps convert to nanoseconds. A device
	// that reports no clock cannot timestamp, which is a capability rather
	// than a failure to open, so there is nothing to check here.
	d.clock = d.dev.ClockInfo()

	required, err := d.dev.MinInline()
	if err != nil {
		// This is the first thing that creates a queue pair, so on a machine
		// where the caller lacks the capability it is where the refusal
		// surfaces -- as a bare "Operation not permitted" from inside a probe
		// nobody asked for.
		return nil, withPrivilegeHint(err)
	}

	// Carrying several packets in one descriptor is worth it wherever the
	// device allows it, so it is on by default where it can be. It cannot be
	// used on a device that parses an Ethernet header out of the descriptor,
	// because the descriptor has no room for one.
	// Pointer entries are the default wherever multi-packet is on and nobody
	// asked for copying: sixteen bytes of descriptor per packet, nothing
	// copied, and the card drains them three times faster than copied ones --
	// 57 against 18 million packets a second on one queue of a ConnectX-6 Dx.
	multiPacket := cfg.multiPacket
	mpwPointer := !cfg.mpwSet
	if !cfg.mpwSet {
		multiPacket = caps.EnhancedMPW() && required == 0
	} else if multiPacket {
		switch {
		case !caps.EnhancedMPW():
			return nil, fmt.Errorf("mlx5: %s cannot carry several packets in one descriptor", ibdev)
		case required != 0:
			return nil, fmt.Errorf("mlx5: %s requires %d inline header bytes, which leaves no room for several packets in one descriptor",
				ibdev, required)
		}
	}

	// The provisional choice above, finalized now that required is known.
	if autoCopy && required == 0 {
		multiPacket, mpwPointer = true, false
	} else if autoCopy && cfg.ringsPerQueue == 0 {
		rings = 1 // the port demands inline headers, so copied entries are off the table
	}

	inline := InlineHeaderFor(required, cfg.ethHdrLen)
	if multiPacket {
		inline = 0 // the packets carry their own headers
	}
	if cfg.inlineSet {
		if cfg.inlineLen < required {
			return nil, fmt.Errorf("mlx5: %s requires %d inline header bytes, but %d were asked for",
				ibdev, required, cfg.inlineLen)
		}
		inline = cfg.inlineLen
	}

	d.info = Info{
		Interface:          ifname,
		IBDev:              caps.IBDev,
		Port:               caps.Port,
		Firmware:           caps.FW,
		PortActive:         caps.PortActive,
		InlineHeader:       inline,
		RequiredInline:     required,
		EnhancedMPW:        caps.EnhancedMPW(),
		MultiPacket:        multiPacket,
		MultiPacketPointer: multiPacket && mpwPointer,
		MultiPacketMaxLen:  cfg.mpwMaxLen,
		FrameSize:          cfg.frameSize,
		Frames:             cfg.frames,
		TxQueues:           cfg.txQueues,
		TxDepth:            cfg.txDepth,
		RingsPerQueue:      rings,
		RxQueues:           cfg.rxQueues,
		RxDepth:            cfg.rxDepth,
		HugePages:          d.region.hugePages,
		LKey:               lkey,
		RegionVA:           d.region.va(),
	}

	// Where the workers will run. Worked out here so that Open fails on an
	// impossible request rather than the first packet.
	if !cfg.noAffinity {
		if d.place, err = newPlacement(ifname, cfg.affinity); err != nil {
			return nil, err
		}
	}
	d.info.Placement = d.place.String()

	// Each queue owns a slice of the region, so that the free lists never
	// share a frame and need no lock. The shares are weighted by what each
	// queue can have outstanding -- a transmit queue of four rings holds four
	// rings' worth -- and the remainder goes to the first queues, so every
	// frame has exactly one owner.
	txW, rxW := rings*cfg.txDepth, cfg.rxDepth
	totalW := max(cfg.txQueues*txW+cfg.rxQueues*rxW, 1)
	txFrames := cfg.frames / totalW * txW
	rxFrames := cfg.frames / totalW * rxW
	if txFrames == 0 || (cfg.rxQueues > 0 && rxFrames == 0) {
		// Integer division starved somebody; fall back to an even split.
		even := cfg.frames / max(cfg.txQueues+cfg.rxQueues, 1)
		txFrames, rxFrames = even, even
	}
	rem := cfg.frames - cfg.txQueues*txFrames - cfg.rxQueues*rxFrames
	take := func(base int) int {
		if rem > 0 {
			rem--
			return base + 1
		}
		return base
	}
	first := 0
	for i := 0; i < cfg.txQueues; i++ {
		n := take(txFrames)
		q, err := d.newTxQueue(i, first, n, rings, inline, multiPacket, mpwPointer)
		if err != nil {
			return nil, fmt.Errorf("mlx5: transmit queue %d: %w", i, err)
		}
		d.tx = append(d.tx, q)
		first += n
	}

	if cfg.rxQueues > 0 {
		steer, err := d.steeringRules(ifname, cfg)
		if err != nil {
			return nil, err
		}
		dvqs := make([]*dv.RxQueue, 0, cfg.rxQueues)
		for i := 0; i < cfg.rxQueues; i++ {
			n := take(rxFrames)
			q, err := d.newRxQueue(i, first, n)
			if err != nil {
				return nil, fmt.Errorf("mlx5: receive queue %d: %w", i, err)
			}
			d.rx = append(d.rx, q)
			dvqs = append(dvqs, q.dvq)
			first += n
		}

		// One rule over all the queues, with the NIC dividing the traffic
		// between them. The rule goes in last, so that nothing arrives before
		// there is somewhere to put it.
		if d.rxg, err = d.dev.NewRxGroup(dvqs); err != nil {
			return nil, err
		}
		if err := d.dev.Steer(d.rxg, steer...); err != nil {
			return nil, err
		}
		d.info.Steering = describeRules(steer)
	}
	return d, nil
}

// steeringRules turns a filter into the rules the card installs: one per
// [packetio.Rule], each a conjunction, a packet matching any of them arriving.
func (d *Device) steeringRules(ifname string, cfg config) ([]dv.Steering, error) {
	base, err := d.steering(ifname, cfg)
	if err != nil {
		return nil, err
	}
	if base.Promiscuous {
		return []dv.Steering{base}, nil
	}
	rules, err := cfg.steering.Rules()
	if err != nil {
		return nil, fmt.Errorf("mlx5: %w", err)
	}
	out := make([]dv.Steering, 0, len(rules))
	for _, r := range rules {
		s := base
		if r.MACSet {
			s.MAC, s.MACSet = r.MAC, true
		}
		if r.VLANSet {
			s.VLAN = int(r.VLAN)
		}
		if r.EtherType != 0 {
			s.EtherType = r.EtherType
		}
		if r.IPProtoSet {
			s.IPProto, s.IPProtoSet = r.IPProto, true
		}
		for _, pre := range []struct {
			p        netip.Prefix
			ip, mask *[4]byte
		}{{r.SrcPrefix, &s.SrcIP, &s.SrcMask}, {r.DstPrefix, &s.DstIP, &s.DstMask}} {
			if !pre.p.IsValid() {
				continue
			}
			if !pre.p.Addr().Is4() {
				return nil, fmt.Errorf("%w: mlx5 steering matches IPv4 addresses, not %s",
					packetio.ErrUnsupported, pre.p)
			}
			*pre.ip, *pre.mask = prefixBytes(pre.p)
		}
		if r.SrcPortSet {
			s.SrcPort, s.SrcPortSet = r.SrcPort, true
		}
		if r.DstPortSet {
			s.DstPort, s.DstPortSet = r.DstPort, true
		}
		out = append(out, s)
	}
	return out, nil
}

// steering works out what the receive queues should be told to take, and
// refuses a filter this card cannot express rather than installing a wider one.
func (d *Device) steering(ifname string, cfg config) (dv.Steering, error) {
	// VLAN -1 means "any": the rules a filter expands to set it where the
	// filter names one.
	s := dv.Steering{VLAN: -1, Promiscuous: cfg.steering.Promiscuous}
	if err := cfg.steering.Validate(); err != nil {
		return s, fmt.Errorf("mlx5: %w", err)
	}

	switch {
	case s.Promiscuous:
	case !filterNamesDstMAC(cfg.steering):
		// Packets addressed to this interface, which is what the rest of the
		// network already sends here. A filter that names its own destination
		// address replaces this rather than adding to it.
		ifi, err := net.InterfaceByName(ifname)
		if err != nil {
			return s, fmt.Errorf("mlx5: looking up %s: %w", ifname, err)
		}
		if len(ifi.HardwareAddr) != 6 {
			return s, fmt.Errorf("mlx5: %s has no Ethernet address to receive on", ifname)
		}
		copy(s.MAC[:], ifi.HardwareAddr)
		s.MACSet = true
		d.info.MAC = s.MAC
	}

	return s, nil
}

func filterNamesDstMAC(f packetio.SteeringFilter) bool {
	for _, m := range f.Match {
		if m.Kind == packetio.MatchKindDstMAC {
			return true
		}
	}
	return false
}

// prefixBytes splits a prefix into the address and the mask the card wants,
// both in wire order.
func prefixBytes(p netip.Prefix) (addr, mask [4]byte) {
	a := p.Masked().Addr().As4()
	addr = a
	bits := p.Bits()
	for i := 0; i < 4; i++ {
		n := bits - i*8
		switch {
		case n >= 8:
			mask[i] = 0xff
		case n > 0:
			mask[i] = ^byte(0) << (8 - n)
		}
	}
	return addr, mask
}

// describeRules renders every rule the card was given, not just the first.
//
// This line is how a user checks the card got what they meant, and describing
// one rule while announcing "(2 rules)" is worse than saying nothing: asking
// for two ports printed only the first, so the honest reading was that the
// second had been dropped -- when in fact both were installed and working.
//
// Rules that differ in one field are collapsed onto that field, because "vlan
// 2053 proto udp port 9002 or 9004" is what the user asked for and reads like
// it, where two near-identical lines do not.
func describeRules(rules []dv.Steering) string {
	if len(rules) == 0 {
		return "nothing"
	}
	if len(rules) == 1 {
		return describeSteering(rules[0])
	}
	// If every rule is the first with only its destination port changed, say so
	// on one line. Same for source port. Anything else is listed in full.
	if ports, ok := varyingDstPorts(rules); ok {
		base := rules[0]
		base.DstPortSet = false
		return describeSteering(base) + " port " + joinPorts(ports)
	}
	parts := make([]string, 0, len(rules))
	for _, r := range rules {
		parts = append(parts, describeSteering(r))
	}
	return strings.Join(parts, ", or ")
}

// varyingDstPorts reports the destination ports of a rule set that is identical
// but for them.
func varyingDstPorts(rules []dv.Steering) ([]uint16, bool) {
	ports := make([]uint16, 0, len(rules))
	first := rules[0]
	if !first.DstPortSet {
		return nil, false
	}
	for _, r := range rules {
		if !r.DstPortSet {
			return nil, false
		}
		same := r
		same.DstPort = first.DstPort
		if same != first {
			return nil, false
		}
		ports = append(ports, r.DstPort)
	}
	return ports, true
}

func joinPorts(ports []uint16) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, fmt.Sprint(p))
	}
	if len(parts) == 2 {
		return parts[0] + " or " + parts[1]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + ", or " + parts[len(parts)-1]
}

func describeSteering(s dv.Steering) string {
	if s.Promiscuous {
		return "every packet the port sees"
	}
	var parts []string
	if s.MACSet {
		parts = append(parts, "to "+net.HardwareAddr(s.MAC[:]).String())
	}
	if s.VLAN >= 0 {
		parts = append(parts, fmt.Sprintf("vlan %d", s.VLAN))
	}
	if s.EtherType != 0 {
		parts = append(parts, fmt.Sprintf("ethertype 0x%04x", s.EtherType))
	}
	if s.SrcMask != ([4]byte{}) {
		parts = append(parts, "from "+ipMaskString(s.SrcIP, s.SrcMask))
	}
	if s.DstMask != ([4]byte{}) {
		parts = append(parts, "to "+ipMaskString(s.DstIP, s.DstMask))
	}
	if s.IPProtoSet {
		parts = append(parts, "proto "+protoName(s.IPProto))
	}
	if s.SrcPortSet {
		parts = append(parts, fmt.Sprintf("src port %d", s.SrcPort))
	}
	if s.DstPortSet {
		parts = append(parts, fmt.Sprintf("port %d", s.DstPort))
	}
	if len(parts) == 0 {
		return "every packet the port sees"
	}
	return "packets " + strings.Join(parts, " ")
}

func ipMaskString(ip, mask [4]byte) string {
	bits := 0
	for _, b := range mask {
		for ; b&0x80 != 0; b <<= 1 {
			bits++
		}
	}
	return fmt.Sprintf("%d.%d.%d.%d/%d", ip[0], ip[1], ip[2], ip[3], bits)
}

func protoName(p uint8) string {
	switch p {
	case 6:
		return "tcp"
	case 17:
		return "udp"
	}
	return fmt.Sprintf("ip/%d", p)
}

// Info describes the device.
func (d *Device) Info() Info { return d.info }

// maxQueues is how many queues of either direction this backend will open. The
// card allows far more; this is the point past which more queues have only
// ever cost throughput on the hardware measured, and it is the number
// Capabilities reports as the ceiling.
const maxQueues = 64

// ringsPerCopiedQueue is how many hardware send queues back one TxQueue in
// copied multi-packet mode. Measured on a ConnectX-6 Dx: one ring drains
// ~16.8 Mpps of copied entries, one core produces ~52, and the fifth ring
// adds nothing a core can use.
const ringsPerCopiedQueue = 4

func nextPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// maxFrames bounds the region a device will register, so that a mistyped frame
// count is refused rather than turning into a multi-gigabyte pinned
// allocation. It is far above anything a real queue configuration needs: 64
// queues of 4096 buffers is 262144 frames.
const maxFrames = 1 << 21

// Capabilities reports what this backend and this NIC can do.
func (d *Device) Capabilities() packetio.Capabilities {
	return packetio.Capabilities{
		Backend:           "mlx5",
		ZeroCopy:          true,
		KernelCoexistence: true,          // flow steering: the kernel keeps the netdev
		MultiBuffer:       false,         // one frame per packet for now
		RSS:               len(d.rx) > 1, // Toeplitz over IPv4 addresses and UDP ports
		TxChecksumOffload: true,
		RxChecksumFlags:   true,
		RxTimestamps:      d.clock.Mult != 0,
		BlockingPoll:      false, // polling only, for now
		SharedRegion:      true,
		HandsBackFrames:   true,
		// What a packet can actually be on receive, not what a frame
		// measures: the ring posts buffers rxHeadroom into the frame so a
		// forwarder has room to prepend, and those bytes are not available
		// to an arriving packet.
		MaxFrameSize: d.info.FrameSize - rxHeadroom,
		MaxQueues:    maxQueues,
	}
}

// Region is the frame memory every queue of this device draws on.
func (d *Device) Region() packetio.Region { return d.region }

// NumTxQueues is how many transmit queues are open.
func (d *Device) NumTxQueues() int { return len(d.tx) }

// NumRxQueues is how many receive queues are open.
func (d *Device) NumRxQueues() int { return len(d.rx) }

// TxQueue returns transmit queue i, or nil if there is no such queue.
func (d *Device) TxQueue(i int) packetio.TxQueue {
	if i < 0 || i >= len(d.tx) {
		return nil
	}
	return d.tx[i]
}

// Tx returns transmit queue i as its concrete type, for the options that are
// specific to this backend.
func (d *Device) Tx(i int) *TxQueue {
	if i < 0 || i >= len(d.tx) {
		return nil
	}
	return d.tx[i]
}

// RxQueue returns receive queue i, or nil if there is no such queue.
func (d *Device) RxQueue(i int) packetio.RxQueue {
	if i < 0 || i >= len(d.rx) {
		return nil
	}
	return d.rx[i]
}

// Rx returns receive queue i as its concrete type.
func (d *Device) Rx(i int) *RxQueue {
	if i < 0 || i >= len(d.rx) {
		return nil
	}
	return d.rx[i]
}

// Close shuts down every queue and releases the device.
//
// The order matters and is not negotiable: the queues go first, then the
// registered region, then the device. Unregistering memory a queue still points
// at leaves the NIC reading pages that no longer belong to this process.
//
// No queue may be in use by another goroutine, and nothing should still be in
// flight; a queue with packets outstanding is destroyed anyway, which drops
// them.
func (d *Device) Close() error {
	if d == nil {
		return nil
	}
	d.closeMu.Lock()
	defer d.closeMu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true

	var errs []error
	// The steering rule goes first, so nothing new arrives while the rest is
	// being taken apart, and the queues it names go after it.
	if d.rxg != nil {
		if err := d.rxg.Close(); err != nil {
			errs = append(errs, err)
		}
		d.rxg = nil
	}
	for _, q := range d.rx {
		if err := q.close(); err != nil {
			errs = append(errs, err)
		}
	}
	d.rx = nil
	for _, q := range d.tx {
		if err := q.close(); err != nil {
			errs = append(errs, err)
		}
	}
	d.tx = nil
	if d.dev != nil {
		if err := d.dev.Close(); err != nil {
			errs = append(errs, err)
		}
		d.dev = nil
	}
	if d.region != nil {
		if err := d.region.close(); err != nil {
			errs = append(errs, err)
		}
		d.region = nil
	}
	return errors.Join(errs...)
}

// HeaderLayout reports where the installed rdma-core headers put the fields of
// a work queue entry. It is here for the info tool and for bug reports; nothing
// on the packet path consults it.
func HeaderLayout() dv.HeaderLayout { return dv.Headers() }

// InlineHeaderFor is how many bytes of each packet a device must be shown,
// given what it requires and how long the frames' Ethernet headers are.
//
// A device that requires nothing is shown nothing, whatever the frames look
// like: it does not parse the entry, so there is no reason to copy into it. A
// device that requires something must be shown its whole Ethernet header, which
// is 14 bytes untagged, 18 with a VLAN tag and 22 with two. The 18 that
// rdma-core inlines unconditionally is the singly tagged case, and is
// not enough for a QinQ frame.
func InlineHeaderFor(required, ethHdrLen int) int {
	if required == 0 {
		return 0
	}
	if ethHdrLen > required {
		return ethHdrLen
	}
	return required
}

// EthernetHeaderLen is how long an Ethernet header with the given number of
// VLAN tags is.
func EthernetHeaderLen(vlanTags int) int {
	return 14 + vlanTags*4
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
