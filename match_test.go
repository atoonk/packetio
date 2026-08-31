package packetio

import (
	"errors"
	"net/netip"
	"testing"
)

func TestSteeringFilterValidate(t *testing.T) {
	p := netip.MustParsePrefix("10.0.0.0/8")
	for _, tc := range []struct {
		name string
		f    SteeringFilter
		ok   bool
	}{
		{"empty is the default", SteeringFilter{}, true},
		{"promiscuous alone", SteeringFilter{Promiscuous: true}, true},
		{"a vlan", SteeringFilter{Match: []Match{MatchVLAN(2053)}}, true},
		{"a prefix", SteeringFilter{Match: []Match{MatchSrcIP(p)}}, true},
		{"a udp port, built properly", SteeringFilter{Match: MatchDstPort(IPProtoUDP, 9000)}, true},
		{"vlan and port together", SteeringFilter{Match: append(
			[]Match{MatchVLAN(2053)}, MatchDstPort(IPProtoUDP, 9000)...)}, true},

		// The contradictions. Each of these would otherwise become a rule
		// meaning something other than what was asked for.
		{"promiscuous with a match", SteeringFilter{
			Promiscuous: true, Match: []Match{MatchVLAN(1)}}, false},
		{"a bare port with no protocol", SteeringFilter{
			Match: []Match{{Kind: MatchKindDstPort, Port: 9000}}}, false},
		{"two different protocols", SteeringFilter{Match: []Match{
			MatchIPProto(IPProtoUDP), MatchIPProto(IPProtoTCP)}}, false},
		{"a tcp port under a udp protocol", SteeringFilter{Match: append(
			[]Match{MatchIPProto(IPProtoUDP)}, MatchDstPort(IPProtoTCP, 80)...)}, false},
		{"an invalid prefix", SteeringFilter{
			Match: []Match{MatchSrcIP(netip.Prefix{})}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.f.Validate()
			if tc.ok && err != nil {
				t.Errorf("refused a valid filter: %v", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatal("accepted a contradictory filter")
				}
				if !errors.Is(err, ErrUnsupported) {
					t.Errorf("got %v, want ErrUnsupported", err)
				}
			}
		})
	}
}

func TestMatchDstPortEmitsItsProtocol(t *testing.T) {
	// A port match without its protocol would match the same offset inside
	// whatever else the packet happened to carry, so the constructor emits
	// both and the pair has to survive Validate.
	ms := MatchDstPort(IPProtoUDP, 9000)
	if len(ms) != 2 {
		t.Fatalf("got %d matches, want the port and its protocol", len(ms))
	}
	var sawProto, sawPort bool
	for _, m := range ms {
		switch m.Kind {
		case MatchKindIPProto:
			sawProto = m.IPProto == IPProtoUDP
		case MatchKindDstPort:
			sawPort = m.Port == 9000 && m.IPProto == IPProtoUDP
		}
	}
	if !sawProto || !sawPort {
		t.Errorf("MatchDstPort produced %v", ms)
	}
	if err := (SteeringFilter{Match: ms}).Validate(); err != nil {
		t.Errorf("its own output does not validate: %v", err)
	}
}

func TestSteeringFilterString(t *testing.T) {
	// The startup line is how a user checks the filter is what they meant, so
	// it has to read like the thing they asked for.
	for _, tc := range []struct {
		f    SteeringFilter
		want string
	}{
		{SteeringFilter{}, "packets addressed to this interface"},
		{SteeringFilter{Promiscuous: true}, "every packet the port sees"},
		{SteeringFilter{Match: append([]Match{MatchVLAN(2053)},
			MatchDstPort(IPProtoUDP, 9000)...)}, "vlan 2053 and proto udp and udp port 9000"},
		// Alternatives read as "or", grouped, because that is what they mean.
		// MatchDstPort emits the protocol once per port; it prints once.
		{SteeringFilter{Match: append(append([]Match{MatchVLAN(2043)},
			MatchDstPort(IPProtoUDP, 9000)...),
			MatchDstPort(IPProtoUDP, 9001)...)},
			"vlan 2043 and proto udp and (udp port 9000 or udp port 9001)"},
		{SteeringFilter{Match: []Match{MatchVLAN(10), MatchVLAN(20)}}, "vlan 10 or vlan 20"},
		{SteeringFilter{Match: []Match{MatchSrcIP(netip.MustParsePrefix("10.0.0.0/8"))}},
			"from 10.0.0.0/8"},
	} {
		if got := tc.f.String(); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

func TestRulesExpandAlternatives(t *testing.T) {
	f := SteeringFilter{Match: append(
		[]Match{MatchVLAN(2053)},
		append(MatchDstPort(IPProtoUDP, 9001), MatchDstPort(IPProtoUDP, 9005)...)...)}
	rules, err := f.Rules()
	if err != nil {
		t.Fatalf("Rules: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2 (one per port)", len(rules))
	}
	ports := map[uint16]bool{}
	for _, r := range rules {
		if !r.VLANSet || r.VLAN != 2053 {
			t.Errorf("a rule lost the VLAN: %+v", r)
		}
		if !r.IPProtoSet || r.IPProto != IPProtoUDP {
			t.Errorf("a rule lost the protocol: %+v", r)
		}
		if !r.DstPortSet {
			t.Errorf("a rule has no port: %+v", r)
		}
		ports[r.DstPort] = true
	}
	if !ports[9001] || !ports[9005] {
		t.Errorf("ports covered: %v, want 9001 and 9005", ports)
	}

	// A single value does not multiply, and two kinds do.
	one, _ := SteeringFilter{Match: MatchDstPort(IPProtoUDP, 1)}.Rules()
	if len(one) != 1 {
		t.Errorf("one port gave %d rules", len(one))
	}
	p1, p2 := netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("192.168.0.0/16")
	four, _ := SteeringFilter{Match: append(
		append(MatchDstPort(IPProtoUDP, 1), MatchDstPort(IPProtoUDP, 2)...),
		MatchSrcIP(p1), MatchSrcIP(p2))}.Rules()
	if len(four) != 4 {
		t.Errorf("two ports and two prefixes gave %d rules, want 4", len(four))
	}

	// Past the limit is refused, not truncated.
	var many []Match
	for p := uint16(1); p <= MaxRules+1; p++ {
		many = append(many, MatchDstPort(IPProtoUDP, p)...)
	}
	if _, err := (SteeringFilter{Match: many}).Rules(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("%d alternatives: got %v, want ErrUnsupported", MaxRules+1, err)
	}

	// Promiscuous is exactly one rule that says so.
	if r, _ := (SteeringFilter{Promiscuous: true}).Rules(); len(r) != 1 || !r[0].Promiscuous {
		t.Errorf("promiscuous gave %+v", r)
	}
	// And a contradiction never reaches a backend.
	if _, err := (SteeringFilter{Match: []Match{{Kind: MatchKindDstPort, Port: 1}}}).Rules(); err == nil {
		t.Error("a bare port with no protocol was expanded instead of refused")
	}
}

// Repeated matches of one kind are alternatives, for every kind and not just
// ports and prefixes. Keeping only the last of them means a filter asking for
// two VLANs silently receives one, which is the failure the design forbids.
func TestRulesExpandsEveryRepeatedKind(t *testing.T) {
	mac1 := [6]byte{0x02, 0, 0, 0, 0, 1}
	mac2 := [6]byte{0x02, 0, 0, 0, 0, 2}

	for _, tc := range []struct {
		name  string
		f     SteeringFilter
		rules int
		check func(t *testing.T, rs []Rule)
	}{
		{
			name:  "two VLANs",
			f:     SteeringFilter{Match: []Match{MatchVLAN(10), MatchVLAN(20)}},
			rules: 2,
			check: func(t *testing.T, rs []Rule) {
				want := map[uint16]bool{10: true, 20: true}
				for _, r := range rs {
					if !r.VLANSet || !want[r.VLAN] {
						t.Errorf("rule has VLAN %d (set=%v), want one of 10, 20", r.VLAN, r.VLANSet)
					}
					delete(want, r.VLAN)
				}
				if len(want) != 0 {
					t.Errorf("these VLANs got no rule: %v", want)
				}
			},
		},
		{
			name:  "two MACs",
			f:     SteeringFilter{Match: []Match{MatchDstMAC(mac1), MatchDstMAC(mac2)}},
			rules: 2,
			check: func(t *testing.T, rs []Rule) {
				if rs[0].MAC == rs[1].MAC {
					t.Errorf("both rules got the same MAC %v", rs[0].MAC)
				}
				for _, r := range rs {
					if !r.MACSet {
						t.Error("a rule has no MAC set")
					}
				}
			},
		},
		{
			name:  "two ethertypes",
			f:     SteeringFilter{Match: []Match{MatchEtherType(0x0800), MatchEtherType(0x86dd)}},
			rules: 2,
		},
		{
			// The cross product: two VLANs and two ports is four rules.
			name: "two VLANs and two ports",
			f: SteeringFilter{Match: append(
				[]Match{MatchVLAN(10), MatchVLAN(20)},
				append(MatchDstPort(IPProtoUDP, 9001), MatchDstPort(IPProtoUDP, 9002)...)...)},
			rules: 4,
			check: func(t *testing.T, rs []Rule) {
				seen := map[[2]uint16]bool{}
				for _, r := range rs {
					seen[[2]uint16{r.VLAN, r.DstPort}] = true
					if r.IPProto != IPProtoUDP {
						t.Errorf("rule lost its protocol: %+v", r)
					}
				}
				if len(seen) != 4 {
					t.Errorf("got %d distinct VLAN/port pairs, want 4: %v", len(seen), seen)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs, err := tc.f.Rules()
			if err != nil {
				t.Fatalf("Rules: %v", err)
			}
			if len(rs) != tc.rules {
				t.Fatalf("got %d rules, want %d: %+v", len(rs), tc.rules, rs)
			}
			if tc.check != nil {
				tc.check(t, rs)
			}
		})
	}
}

// A repeated protocol match is the same protocol -- Validate refuses two
// different ones -- so it must not double the rules.
func TestRulesDoesNotDoubleARepeatedProtocol(t *testing.T) {
	f := SteeringFilter{Match: append(
		MatchDstPort(IPProtoUDP, 9001),
		MatchSrcPort(IPProtoUDP, 5000)...)}
	rs, err := f.Rules()
	if err != nil {
		t.Fatalf("Rules: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("got %d rules, want 1: %+v", len(rs), rs)
	}
	if rs[0].DstPort != 9001 || rs[0].SrcPort != 5000 || rs[0].IPProto != IPProtoUDP {
		t.Errorf("rule = %+v, want udp src 5000 dst 9001", rs[0])
	}
}

// A filter needing more rules than any backend installs is refused, and does
// not build the whole cross product on the way to finding out.
func TestRulesRefusesTooManyAlternatives(t *testing.T) {
	var ms []Match
	for i := 0; i < 300; i++ {
		ms = append(ms, MatchVLAN(uint16(i%MaxVLAN)))
		ms = append(ms, MatchEtherType(uint16(0x0800+i)))
	}
	if _, err := (SteeringFilter{Match: ms}).Rules(); err == nil {
		t.Fatal("accepted a filter needing far more than MaxRules")
	} else if !errors.Is(err, ErrUnsupported) {
		t.Errorf("error is %v, want ErrUnsupported", err)
	}
}

// A VLAN outside the twelve bits of a tag is refused rather than masked down
// to some other VLAN the caller never named.
func TestValidateRefusesAVLANOutOfRange(t *testing.T) {
	err := SteeringFilter{Match: []Match{MatchVLAN(5000)}}.Validate()
	if err == nil {
		t.Fatal("accepted VLAN 5000")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("error is %v, want ErrUnsupported", err)
	}
}

// An unknown MatchKind is refused, not compiled into nothing. A Match built
// by hand with a kind this package never defined would collect no condition
// in Rules, leaving the bare base rule -- a filter that matches everything,
// which is the widening the package doc forbids. Both switches refuse it.
func TestUnknownMatchKindRefused(t *testing.T) {
	f := SteeringFilter{Match: []Match{{Kind: MatchKind(99)}}}
	if err := f.Validate(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Validate: got %v, want ErrUnsupported", err)
	}
	if _, err := f.Rules(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Rules: got %v, want ErrUnsupported", err)
	}
}

// EtherType 0 is refused for the same reason: a compiled Rule carries 0 as
// "not matched", so the condition would vanish rather than match nothing.
func TestEtherTypeZeroRefused(t *testing.T) {
	f := SteeringFilter{Match: []Match{MatchEtherType(0)}}
	if err := f.Validate(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Validate: got %v, want ErrUnsupported", err)
	}
}
