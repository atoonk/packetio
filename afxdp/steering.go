//go:build linux

package afxdp

import (
	"fmt"

	xdp "github.com/atoonk/go-afxdp"
	"github.com/atoonk/packetio"
)

// SteeringOption compiles a backend-neutral filter into a go-afxdp option.
//
// go-afxdp's matches are ORed, and its only AND is a source-and-destination
// prefix pair, so not every SteeringFilter has an XDP form. What is expressible:
// promiscuous; any number of ports of one protocol; any number of source or
// destination prefixes; exactly one source and one destination prefix
// together; one EtherType; one IP protocol. A filter that needs an AND go-afxdp
// cannot express is refused with [packetio.ErrUnsupported] rather than
// installed as something wider.
//
// Two different things about VLANs, and it is worth keeping them apart.
//
// A VLAN cannot be matched at all: go-afxdp skips an 802.1Q tag transparently
// so the same program works whether or not the NIC strips it, and there is no
// match on the identifier. A filter naming one is refused; on a tagged
// interface the traffic is usually distinguished by port or address anyway.
// For anything beyond this vocabulary, pass go-afxdp's own matches to Open
// directly.
//
// Separately, and more likely to waste an afternoon: on a tagged interface the
// packets may never reach the program at all. See the note on the package doc
// about rx-vlan-filter.
func SteeringOption(f packetio.SteeringFilter) (xdp.Option, error) {
	rules, err := f.Rules()
	if err != nil {
		return nil, err
	}
	if len(rules) == 1 && rules[0].Promiscuous {
		return xdp.WithFilter(xdp.MatchAll()), nil
	}

	// Every rule must be of the same shape for this to be an OR of one kind.
	var matches []xdp.Match
	for _, r := range rules {
		m, err := ruleToMatch(r)
		if err != nil {
			return nil, err
		}
		matches = append(matches, m)
	}
	return xdp.WithFilter(matches...), nil
}

func ruleToMatch(r packetio.Rule) (xdp.Match, error) {
	if r.VLANSet {
		return xdp.Match{}, fmt.Errorf("%w: AF_XDP filters skip the VLAN tag and cannot match "+
			"its identifier; match on ports or addresses instead", packetio.ErrUnsupported)
	}
	if r.MACSet {
		return xdp.Match{}, fmt.Errorf("%w: AF_XDP filters have no destination-MAC match",
			packetio.ErrUnsupported)
	}
	// Count what the rule asks for, since go-afxdp can AND at most a prefix
	// pair, and a port cannot be combined with anything but its protocol.
	var kinds int
	var m xdp.Match
	set := func(x xdp.Match) {
		kinds++
		m = x
	}
	switch {
	case r.SrcPrefix.IsValid() && r.DstPrefix.IsValid():
		set(xdp.MatchFlow(r.SrcPrefix.String(), r.DstPrefix.String()))
	case r.SrcPrefix.IsValid():
		set(xdp.MatchSrcIP(r.SrcPrefix.String()))
	case r.DstPrefix.IsValid():
		set(xdp.MatchDstIP(r.DstPrefix.String()))
	}
	if r.DstPortSet {
		switch r.IPProto {
		case packetio.IPProtoUDP:
			set(xdp.MatchUDPPort(r.DstPort))
		case packetio.IPProtoTCP:
			set(xdp.MatchTCPPort(r.DstPort))
		default:
			return xdp.Match{}, fmt.Errorf("%w: a port match needs TCP or UDP", packetio.ErrUnsupported)
		}
	}
	if r.SrcPortSet {
		switch r.IPProto {
		case packetio.IPProtoUDP:
			set(xdp.MatchUDPSrcPort(r.SrcPort))
		case packetio.IPProtoTCP:
			set(xdp.MatchTCPSrcPort(r.SrcPort))
		default:
			return xdp.Match{}, fmt.Errorf("%w: a port match needs TCP or UDP", packetio.ErrUnsupported)
		}
	}
	if r.IPProtoSet && !r.SrcPortSet && !r.DstPortSet {
		set(xdp.MatchIPv4Proto(r.IPProto))
	}
	if r.EtherType != 0 {
		set(xdp.MatchEtherType(r.EtherType))
	}
	switch kinds {
	case 0:
		// The zero SteeringFilter means "the backend's default", which the
		// common API defines as packets addressed to this interface. AF_XDP
		// has no destination-MAC match, so that default has no form here and
		// there is nothing to quietly fall back to: a filter matching nothing
		// receives nothing, and one matching everything takes the kernel's
		// traffic with it. Say which you meant.
		return xdp.Match{}, fmt.Errorf("%w: AF_XDP cannot express \"packets addressed to this "+
			"interface\", so it has no default filter -- name the traffic you want "+
			"(MatchDstPort, MatchDstIP, MatchEtherType...), or set Promiscuous to take "+
			"everything and keep it from the kernel", packetio.ErrUnsupported)
	case 1:
		return m, nil
	}
	return xdp.Match{}, fmt.Errorf("%w: AF_XDP filters cannot AND %s; only a source and "+
		"destination prefix pair combine", packetio.ErrUnsupported, describeRule(r))
}

func describeRule(r packetio.Rule) string {
	var parts []string
	if r.SrcPrefix.IsValid() {
		parts = append(parts, "from "+r.SrcPrefix.String())
	}
	if r.DstPrefix.IsValid() {
		parts = append(parts, "to "+r.DstPrefix.String())
	}
	if r.IPProtoSet {
		parts = append(parts, fmt.Sprintf("proto %d", r.IPProto))
	}
	if r.DstPortSet {
		parts = append(parts, fmt.Sprintf("port %d", r.DstPort))
	}
	if r.SrcPortSet {
		parts = append(parts, fmt.Sprintf("src port %d", r.SrcPort))
	}
	if r.EtherType != 0 {
		parts = append(parts, fmt.Sprintf("ethertype 0x%04x", r.EtherType))
	}
	s := ""
	for i, p := range parts {
		if i > 0 {
			s += " and "
		}
		s += p
	}
	return s
}
