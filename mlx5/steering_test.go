//go:build linux && cgo && mlx5 && (amd64 || arm64)

package mlx5

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/mlx5/internal/dv"
)

// These need no card: expand and prefixBytes are the fiddly parts, and both
// are pure.

func TestPrefixBytes(t *testing.T) {
	for _, tc := range []struct {
		prefix string
		addr   [4]byte
		mask   [4]byte
	}{
		{"10.0.0.0/8", [4]byte{10, 0, 0, 0}, [4]byte{0xff, 0, 0, 0}},
		{"198.51.100.1/32", [4]byte{198, 51, 100, 1}, [4]byte{0xff, 0xff, 0xff, 0xff}},
		{"172.16.0.0/12", [4]byte{172, 16, 0, 0}, [4]byte{0xff, 0xf0, 0, 0}},
		{"0.0.0.0/0", [4]byte{0, 0, 0, 0}, [4]byte{0, 0, 0, 0}},
		// The address is masked, so a prefix written with host bits set does
		// not install a rule matching an address nobody has.
		{"10.1.2.3/8", [4]byte{10, 0, 0, 0}, [4]byte{0xff, 0, 0, 0}},
	} {
		t.Run(tc.prefix, func(t *testing.T) {
			addr, mask := prefixBytes(netip.MustParsePrefix(tc.prefix))
			if addr != tc.addr {
				t.Errorf("addr = %v, want %v", addr, tc.addr)
			}
			if mask != tc.mask {
				t.Errorf("mask = %v, want %v", mask, tc.mask)
			}
		})
	}
}

func TestDescribeSteeringReadsLikeTheFilter(t *testing.T) {
	// The startup line is how a user checks the card got what they meant.
	s := dv.Steering{
		MAC: [6]byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x01}, MACSet: true,
		VLAN: 2053, IPProto: 17, IPProtoSet: true, DstPort: 9003, DstPortSet: true,
	}
	want := "packets to 02:00:00:00:00:01 vlan 2053 proto udp port 9003"
	if got := describeSteering(s); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := describeSteering(dv.Steering{Promiscuous: true, VLAN: -1}); got != "every packet the port sees" {
		t.Errorf("promiscuous reads as %q", got)
	}
}

func TestSteeringRefusesWhatTheCardCannotDo(t *testing.T) {
	// IPv6 steering is not wired up, and a filter asking for it must be
	// refused at Open rather than installed as a rule that ignores it.
	//
	// This goes through steeringRules, not steering: the IPv6 check is there.
	// Asking steering() instead passed for the wrong reason -- "lo has no
	// Ethernet address to receive on" -- and would have gone on passing if the
	// IPv6 check were deleted.
	d := &Device{}
	cfg := config{steering: packetio.SteeringFilter{
		Match: []packetio.Match{
			// A destination MAC, so the interface lookup that would otherwise
			// fail on lo is not reached.
			packetio.MatchDstMAC([6]byte{0x02, 0, 0, 0, 0, 1}),
			packetio.MatchSrcIP(netip.MustParsePrefix("2001:db8::/32")),
		},
	}}
	_, err := d.steeringRules("lo", cfg)
	if err == nil {
		t.Fatal("accepted an IPv6 prefix, which this backend does not install")
	}
	if !errors.Is(err, packetio.ErrUnsupported) {
		t.Errorf("error is %v, want ErrUnsupported", err)
	}
	if !strings.Contains(err.Error(), "IPv4") {
		t.Errorf("error %q does not say the card matches IPv4", err)
	}
}
