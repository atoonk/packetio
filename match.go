package packetio

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// A SteeringFilter says which packets are steered to a device: taken away from
// the kernel and delivered to this program's receive queues. Packets it does
// not match are left to the kernel, so the interface keeps working -- your SSH
// session, ARP, and everything else carry on while you take the traffic you
// asked for.
//
// That second half is a property of the device, not of the filter:
// Capabilities.KernelCoexistence says whether the kernel still has the
// interface at all. Where it is false -- DPDK on a device bound to vfio-pci --
// the filter still selects what these queues see, but there is no kernel for
// the rest to carry on to.
//
// Steering is a property of the backend. mlx5 compiles it to hardware flow
// rules the card matches at no cost per packet; AF_XDP to an eBPF program.
// AF_PACKET has no steering at all: it is a tap, the kernel sees every packet
// whatever a socket takes, and so it offers no SteeringFilter rather than one
// that would mean something weaker under the same name. A backend that cannot
// express a match refuses it at Open with ErrUnsupported rather than installing
// a wider one -- a filter that silently delivers more than it was asked for is
// worse than none, because nothing downstream can tell.
//
// The zero SteeringFilter means "use the backend's default", which is packets
// addressed to this interface.
type SteeringFilter struct {
	// Match is the set of conditions a packet must satisfy.
	//
	// Matches of different kinds are ANDed, and repeated matches of one kind
	// are alternatives. Two MatchDstPort and one MatchVLAN means "either port,
	// on that VLAN" -- two rules. A filter that needs more than MaxRules of
	// them is refused rather than narrowed.
	Match []Match

	// Promiscuous takes every packet the port sees, whoever it is addressed
	// to, and ignores Match entirely.
	Promiscuous bool
}

// A Match is one condition on a packet. Use the Match* constructors; the
// fields are exported so backends can compile them, not so callers can build
// them by hand.
type Match struct {
	Kind MatchKind

	// MAC is set for MatchDstMAC.
	MAC [6]byte

	// VLAN is set for MatchVLAN, EtherType for MatchEtherType, IPProto for
	// MatchIPProto, and Port for the port matches.
	VLAN      uint16
	EtherType uint16
	IPProto   uint8
	Port      uint16

	// Prefix is set for MatchSrcIP and MatchDstIP. A single address is a
	// prefix with a full-length mask.
	Prefix netip.Prefix
}

// MatchKind names what a Match tests.
type MatchKind uint8

// The kinds of match, one per constructor.
const (
	MatchKindDstMAC MatchKind = iota + 1
	MatchKindVLAN
	MatchKindEtherType
	MatchKindIPProto
	MatchKindSrcIP
	MatchKindDstIP
	MatchKindSrcPort
	MatchKindDstPort
)

// IP protocol numbers, for MatchIPProto and the port matches.
const (
	IPProtoTCP uint8 = 6
	IPProtoUDP uint8 = 17
)

// MatchDstMAC matches packets addressed to one Ethernet address.
func MatchDstMAC(mac [6]byte) Match { return Match{Kind: MatchKindDstMAC, MAC: mac} }

// MatchVLAN matches one 802.1Q tag identifier. Only the identifier is matched,
// so a packet's priority bits do not decide whether it arrives.
//
// An identifier outside the twelve bits of a tag is refused by Validate rather
// than masked, because masking turns a typed digit into a filter for a VLAN
// the caller never named.
func MatchVLAN(id uint16) Match { return Match{Kind: MatchKindVLAN, VLAN: id} }

// MaxVLAN is the largest 802.1Q tag identifier: the field is twelve bits.
const MaxVLAN = 4095

// MatchEtherType matches one EtherType, for example 0x0800 for IPv4.
func MatchEtherType(t uint16) Match { return Match{Kind: MatchKindEtherType, EtherType: t} }

// MatchIPProto matches one IP protocol number, for example [IPProtoUDP].
func MatchIPProto(p uint8) Match { return Match{Kind: MatchKindIPProto, IPProto: p} }

// MatchSrcIP matches packets from an address or a prefix.
func MatchSrcIP(p netip.Prefix) Match { return Match{Kind: MatchKindSrcIP, Prefix: p} }

// MatchDstIP matches packets to an address or a prefix.
func MatchDstIP(p netip.Prefix) Match { return Match{Kind: MatchKindDstIP, Prefix: p} }

// MatchSrcPort and MatchDstPort match one L4 port of the given protocol.
//
// The protocol is part of the match because that is how the hardware works: a
// card looks for a port at a fixed offset inside a protocol it was told to
// expect, so a port without a protocol would match the same offset in
// something else. Both matches are emitted, and a SteeringFilter carrying a port match
// of one protocol and MatchIPProto of another is refused.
func MatchSrcPort(proto uint8, port uint16) []Match {
	return []Match{MatchIPProto(proto), {Kind: MatchKindSrcPort, IPProto: proto, Port: port}}
}

// MatchDstPort matches one destination port of the given protocol; see
// MatchSrcPort for why it emits the protocol match too.
func MatchDstPort(proto uint8, port uint16) []Match {
	return []Match{MatchIPProto(proto), {Kind: MatchKindDstPort, IPProto: proto, Port: port}}
}

// String renders a match the way a person would say it, for the line a program
// prints at startup.
func (m Match) String() string {
	switch m.Kind {
	case MatchKindDstMAC:
		return fmt.Sprintf("to %02x:%02x:%02x:%02x:%02x:%02x",
			m.MAC[0], m.MAC[1], m.MAC[2], m.MAC[3], m.MAC[4], m.MAC[5])
	case MatchKindVLAN:
		return fmt.Sprintf("vlan %d", m.VLAN)
	case MatchKindEtherType:
		return fmt.Sprintf("ethertype 0x%04x", m.EtherType)
	case MatchKindIPProto:
		return "proto " + protoName(m.IPProto)
	case MatchKindSrcIP:
		return "from " + m.Prefix.String()
	case MatchKindDstIP:
		return "to " + m.Prefix.String()
	case MatchKindSrcPort:
		return fmt.Sprintf("%s src port %d", protoName(m.IPProto), m.Port)
	case MatchKindDstPort:
		return fmt.Sprintf("%s port %d", protoName(m.IPProto), m.Port)
	}
	return "unknown match"
}

func protoName(p uint8) string {
	switch p {
	case IPProtoTCP:
		return "tcp"
	case IPProtoUDP:
		return "udp"
	}
	return fmt.Sprintf("ip/%d", p)
}

// String renders a filter for the startup line.
func (f SteeringFilter) String() string {
	if f.Promiscuous {
		return "every packet the port sees"
	}
	if len(f.Match) == 0 {
		return "packets addressed to this interface"
	}
	// Repeated matches of one kind are alternatives and different kinds are
	// conditions, so they read as "or" and "and" respectively -- the line has
	// to say what the filter means. Two ports on a VLAN is "vlan 2043 and
	// (udp port 9000 or udp port 9001)", not "and" three times.
	var order []MatchKind
	groups := map[MatchKind][]string{}
	for _, m := range f.Match {
		s := m.String()
		if slices.Contains(groups[m.Kind], s) {
			continue // MatchDstPort emits the protocol once per port
		}
		if len(groups[m.Kind]) == 0 {
			order = append(order, m.Kind)
		}
		groups[m.Kind] = append(groups[m.Kind], s)
	}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		alt := strings.Join(groups[k], " or ")
		if len(groups[k]) > 1 && len(order) > 1 {
			alt = "(" + alt + ")"
		}
		parts = append(parts, alt)
	}
	return strings.Join(parts, " and ")
}

// Validate reports whether a filter is self-consistent, independent of any
// backend. It catches the contradictions that would otherwise become a rule
// meaning something other than what was asked.
func (f SteeringFilter) Validate() error {
	if f.Promiscuous && len(f.Match) > 0 {
		return fmt.Errorf("%w: a promiscuous filter cannot also match on %s",
			ErrUnsupported, SteeringFilter{Match: f.Match})
	}
	var proto uint8
	var protoSet, wantPort bool
	for _, m := range f.Match {
		switch m.Kind {
		case MatchKindIPProto:
			if protoSet && proto != m.IPProto {
				return fmt.Errorf("%w: two different IP protocols, %s and %s",
					ErrUnsupported, protoName(proto), protoName(m.IPProto))
			}
			proto, protoSet = m.IPProto, true
		case MatchKindSrcPort, MatchKindDstPort:
			wantPort = true
			if m.IPProto != 0 {
				if protoSet && proto != m.IPProto {
					return fmt.Errorf("%w: a %s port match with an %s protocol match",
						ErrUnsupported, protoName(m.IPProto), protoName(proto))
				}
				proto, protoSet = m.IPProto, true
			}
		case MatchKindSrcIP, MatchKindDstIP:
			if !m.Prefix.IsValid() {
				return fmt.Errorf("%w: %s is not a valid prefix", ErrUnsupported, m.Prefix)
			}
		case MatchKindVLAN:
			if m.VLAN > MaxVLAN {
				return fmt.Errorf("%w: VLAN %d is outside the 0 to %d a tag can carry",
					ErrUnsupported, m.VLAN, MaxVLAN)
			}
		case MatchKindDstMAC:
			// Any 48-bit value is an address; nothing to refuse.
		case MatchKindEtherType:
			if m.EtherType == 0 {
				return fmt.Errorf("%w: EtherType 0 means \"not matched\" in a compiled rule, "+
					"so this condition would vanish and the filter would be wider than asked",
					ErrUnsupported)
			}
		default:
			return fmt.Errorf("%w: unknown match kind %d; a Match is built with the Match* constructors",
				ErrUnsupported, m.Kind)
		}
	}
	if wantPort && !protoSet {
		return fmt.Errorf("%w: a port can only be matched together with a protocol; "+
			"use MatchDstPort, which emits both", ErrUnsupported)
	}
	return nil
}

// A Rule is one conjunction a backend installs: every set field must hold for
// a packet to match. A SteeringFilter becomes one or more Rules through Rules, and a
// packet matching any of them is delivered.
//
// Backends compile Rules, not Filters, so the expansion from "these ports on
// this VLAN" into one rule per port is done once, here, and means the same
// thing on a card, in an XDP program, and in a classic BPF program.
type Rule struct {
	MAC    [6]byte
	MACSet bool

	VLAN    uint16
	VLANSet bool

	EtherType uint16 // 0 means not matched

	IPProto    uint8
	IPProtoSet bool

	SrcPrefix, DstPrefix netip.Prefix // invalid means not matched

	SrcPort, DstPort       uint16
	SrcPortSet, DstPortSet bool

	Promiscuous bool
}

// MaxRules bounds how many rules one SteeringFilter may expand to. A filter needing
// more is better expressed as a wider match than as a long list, and every
// backend has some limit on what it will install.
const MaxRules = 16

// Rules expands a filter into the conjunctions a backend installs.
//
// Within a SteeringFilter, repeated matches of one kind are alternatives and different
// kinds are ANDed: two MatchDstPort and one MatchVLAN is "either port, on that
// VLAN", and becomes two rules. It validates first, so a contradictory filter
// is refused before any backend sees it.
func (f SteeringFilter) Rules() ([]Rule, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	if f.Promiscuous {
		return []Rule{{Promiscuous: true}}, nil
	}

	// Every kind collects its alternatives. A kind that appears once expands
	// to nothing extra; a kind that appears twice doubles the rules. Keeping
	// only the last of a repeated kind -- which is what this did -- means a
	// filter asking for two VLANs silently receives one, and that is the
	// failure the whole design forbids: delivering something other than what
	// was asked for, with nothing downstream able to tell.
	var base Rule
	var macs [][6]byte
	var vlans, etherTypes, dstPorts, srcPorts []uint16
	var protos []uint8
	var srcNets, dstNets []netip.Prefix
	for _, m := range f.Match {
		switch m.Kind {
		case MatchKindDstMAC:
			macs = append(macs, m.MAC)
		case MatchKindVLAN:
			vlans = append(vlans, m.VLAN)
		case MatchKindEtherType:
			etherTypes = append(etherTypes, m.EtherType)
		case MatchKindIPProto:
			protos = append(protos, m.IPProto)
		case MatchKindSrcIP:
			srcNets = append(srcNets, m.Prefix)
		case MatchKindDstIP:
			dstNets = append(dstNets, m.Prefix)
		case MatchKindSrcPort:
			srcPorts = append(srcPorts, m.Port)
		case MatchKindDstPort:
			dstPorts = append(dstPorts, m.Port)
		default:
			// Validate refused unknown kinds just above. The refusal stays
			// here too, so a kind added to one switch and not the other
			// cannot become a condition that silently vanishes -- the
			// widening this package forbids.
			return nil, fmt.Errorf("%w: unknown match kind %d", ErrUnsupported, m.Kind)
		}
	}
	// Validate has already refused two different protocols, so a repeated
	// protocol match is the same value and expands to one rule, not several.
	protos = dedupe(protos)

	rules := []Rule{base}
	rules = expandRules(rules, len(macs), func(r *Rule, i int) { r.MAC, r.MACSet = macs[i], true })
	rules = expandRules(rules, len(vlans), func(r *Rule, i int) { r.VLAN, r.VLANSet = vlans[i], true })
	rules = expandRules(rules, len(etherTypes), func(r *Rule, i int) { r.EtherType = etherTypes[i] })
	rules = expandRules(rules, len(protos), func(r *Rule, i int) { r.IPProto, r.IPProtoSet = protos[i], true })
	rules = expandRules(rules, len(dstPorts), func(r *Rule, i int) { r.DstPort, r.DstPortSet = dstPorts[i], true })
	rules = expandRules(rules, len(srcPorts), func(r *Rule, i int) { r.SrcPort, r.SrcPortSet = srcPorts[i], true })
	rules = expandRules(rules, len(srcNets), func(r *Rule, i int) { r.SrcPrefix = srcNets[i] })
	rules = expandRules(rules, len(dstNets), func(r *Rule, i int) { r.DstPrefix = dstNets[i] })
	if len(rules) > MaxRules {
		return nil, fmt.Errorf("%w: this filter needs %d rules and %d is the most a backend "+
			"installs; use fewer alternatives or a wider match", ErrUnsupported, len(rules), MaxRules)
	}
	return rules, nil
}

// dedupe removes repeats, keeping the first of each. Order is kept so that the
// rules a filter expands to do not depend on map iteration.
func dedupe(in []uint8) []uint8 {
	if len(in) < 2 {
		return in
	}
	out := in[:0:0]
	seen := map[uint8]bool{}
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// expandRules multiplies the rules built so far by n alternatives, setting
// each with put. Zero alternatives leaves the set alone; one is applied to
// every rule rather than multiplying, since a single value is not a choice.
//
// It stops once the set is past MaxRules. Rules checks the length afterwards
// and refuses; the cap is here so that a filter naming hundreds of
// alternatives of several kinds does not build the whole cross product first.
func expandRules(in []Rule, n int, put func(*Rule, int)) []Rule {
	if n == 0 || len(in) > MaxRules {
		return in
	}
	out := make([]Rule, 0, len(in)*n)
	for _, r := range in {
		for i := 0; i < n; i++ {
			c := r
			put(&c, i)
			out = append(out, c)
		}
	}
	return out
}
