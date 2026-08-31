//go:build linux && cgo && mlx5 && (amd64 || arm64)

package mlx5

import (
	"strings"
	"testing"

	"github.com/atoonk/packetio/mlx5/internal/dv"
)

// The startup line must describe every rule the card was given. Describing one
// and announcing "(2 rules)" reads as though the rest were dropped, which is
// exactly the doubt this line exists to remove.
func TestDescribeRulesNamesEveryPort(t *testing.T) {
	mac := [6]byte{0x02, 0, 0, 0, 0, 1}
	base := dv.Steering{
		MAC: mac, MACSet: true, VLAN: 2053,
		IPProto: 17, IPProtoSet: true, DstPortSet: true,
	}
	a, b := base, base
	a.DstPort, b.DstPort = 9002, 9004

	got := describeRules([]dv.Steering{a, b})
	for _, want := range []string{"9002", "9004", "vlan 2053", "udp"} {
		if !strings.Contains(got, want) {
			t.Errorf("describeRules omitted %q: %s", want, got)
		}
	}
	if strings.Contains(got, "rules)") {
		t.Errorf("still counting rules instead of naming them: %s", got)
	}
}

// Rules that differ in more than one field are listed in full rather than
// collapsed onto a field they do not share.
func TestDescribeRulesListsUnrelatedRules(t *testing.T) {
	one := dv.Steering{VLAN: 10, IPProto: 17, IPProtoSet: true, DstPort: 9002, DstPortSet: true}
	two := dv.Steering{VLAN: 20, IPProto: 6, IPProtoSet: true, DstPort: 80, DstPortSet: true}
	got := describeRules([]dv.Steering{one, two})
	for _, want := range []string{"vlan 10", "vlan 20", "9002", "80", " or "} {
		if !strings.Contains(got, want) {
			t.Errorf("describeRules omitted %q: %s", want, got)
		}
	}
}

func TestDescribeRulesSingleAndEmpty(t *testing.T) {
	if got := describeRules(nil); got != "nothing" {
		t.Errorf("no rules reads as %q", got)
	}
	one := dv.Steering{VLAN: -1, Promiscuous: true}
	if got := describeRules([]dv.Steering{one}); got != "every packet the port sees" {
		t.Errorf("one promiscuous rule reads as %q", got)
	}
}
