//go:build linux

package afxdp

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/atoonk/packetio"
)

// SteeringOption needs no socket to decide what it can express.
func TestSteeringOptionExpressible(t *testing.T) {
	p := netip.MustParsePrefix("10.0.0.0/8")
	for _, tc := range []struct {
		name string
		f    packetio.SteeringFilter
		ok   bool
	}{
		{"promiscuous", packetio.SteeringFilter{Promiscuous: true}, true},
		{"udp ports", packetio.SteeringFilter{Match: append(
			packetio.MatchDstPort(packetio.IPProtoUDP, 1), packetio.MatchDstPort(packetio.IPProtoUDP, 2)...)}, true},
		{"a source prefix", packetio.SteeringFilter{Match: []packetio.Match{packetio.MatchSrcIP(p)}}, true},
		{"a prefix pair", packetio.SteeringFilter{Match: []packetio.Match{
			packetio.MatchSrcIP(p), packetio.MatchDstIP(netip.MustParsePrefix("192.168.0.0/16"))}}, true},
		{"an ethertype", packetio.SteeringFilter{Match: []packetio.Match{packetio.MatchEtherType(0x86dd)}}, true},

		// What go-afxdp cannot AND, or cannot see.
		{"a vlan", packetio.SteeringFilter{Match: []packetio.Match{packetio.MatchVLAN(2053)}}, false},
		{"a mac", packetio.SteeringFilter{Match: []packetio.Match{packetio.MatchDstMAC([6]byte{1})}}, false},
		{"a port and a prefix", packetio.SteeringFilter{Match: append(
			[]packetio.Match{packetio.MatchSrcIP(p)}, packetio.MatchDstPort(packetio.IPProtoUDP, 1)...)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SteeringOption(tc.f)
			if tc.ok && err != nil {
				t.Errorf("refused: %v", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatal("accepted a filter go-afxdp cannot express; it would have been widened")
				}
				if !errors.Is(err, packetio.ErrUnsupported) {
					t.Errorf("got %v, want ErrUnsupported", err)
				}
			}
		})
	}
}
