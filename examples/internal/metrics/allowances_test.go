package metrics

import "testing"

// The fixture is the output AWS documents for ethtool -S on an ENA. The same
// names come back from the poll-mode driver's xstats, which is the point: one
// instance can be measured through the kernel or through DPDK and the counters
// line up either way.
func TestAllowancesReadTheENACounters(t *testing.T) {
	raw := map[string]uint64{
		"bw_in_allowance_exceeded":      11,
		"bw_out_allowance_exceeded":     22,
		"pps_allowance_exceeded":        33,
		"conntrack_allowance_exceeded":  44,
		"linklocal_allowance_exceeded":  55,
		"conntrack_allowance_available": 136812,
		"tx_packets":                    9999, // not an allowance; must be ignored
	}
	a := Allowances(raw)
	if !a.Present {
		t.Fatal("the counters were there but Present is false")
	}
	for _, c := range []struct {
		name string
		got  uint64
		want uint64
	}{
		{"pps", a.PPSExceeded, 33},
		{"bw_in", a.BwInExceeded, 11},
		{"bw_out", a.BwOutExceeded, 22},
		{"conntrack", a.ConntrackExceeded, 44},
		{"linklocal", a.LinklocalExceeded, 55},
		{"conntrack_available", a.ConntrackAvailable, 136812},
	} {
		if c.got != c.want {
			t.Errorf("%s is %d, want %d", c.name, c.got, c.want)
		}
	}
}

func TestAllowancesAreAbsentOffEC2(t *testing.T) {
	a := Allowances(map[string]uint64{"tx_packets_phy": 5, "rx_packets_phy": 7})
	if a.Present {
		t.Error("a NIC with no allowance counters reports Present")
	}
	if s := a.String(); s != "" {
		t.Errorf("a NIC with no allowance counters renders %q", s)
	}
}

// ConntrackAvailable is a level, not a running total: a delta of it would be
// meaningless, and what a reader wants is how many entries are left now.
func TestSubKeepsConntrackAvailableAsALevel(t *testing.T) {
	before := Allowances(map[string]uint64{
		"pps_allowance_exceeded": 100, "conntrack_allowance_available": 500,
	})
	after := Allowances(map[string]uint64{
		"pps_allowance_exceeded": 175, "conntrack_allowance_available": 420,
	})
	d := after.Sub(before)
	if d.PPSExceeded != 75 {
		t.Errorf("pps delta is %d, want 75", d.PPSExceeded)
	}
	if d.ConntrackAvailable != 420 {
		t.Errorf("conntrack available is %d, want the later level 420", d.ConntrackAvailable)
	}
}

// A driver reset zeroes every cumulative counter. Subtracting across one
// underflows into a vast number, so the window has to be thrown away instead.
func TestWentBackwardsCatchesADriverReset(t *testing.T) {
	before := Allowances(map[string]uint64{"pps_allowance_exceeded": 1000})
	after := Allowances(map[string]uint64{"pps_allowance_exceeded": 3})
	if !after.WentBackwards(before) {
		t.Error("a counter that fell from 1000 to 3 was not reported as a reset")
	}
	if before.WentBackwards(before) {
		t.Error("an unchanged reading was reported as a reset")
	}
}

func TestStringNamesOnlyWhatFired(t *testing.T) {
	quiet := Allowances(map[string]uint64{"pps_allowance_exceeded": 0})
	if got, want := quiet.String(), "allowances: none exceeded"; got != want {
		t.Errorf("quiet run renders %q, want %q", got, want)
	}
	loud := Allowances(map[string]uint64{
		"pps_allowance_exceeded": 7, "bw_in_allowance_exceeded": 0,
	})
	if got, want := loud.String(), "allowances exceeded: pps 7"; got != want {
		t.Errorf("renders %q, want %q", got, want)
	}
}

// The report path is Sub then String, not String on a raw reading, so that is
// what has to be covered: a tick where the shaper fired has to name it.
func TestTheDeltaBetweenTwoReadingsRenders(t *testing.T) {
	before := Allowances(map[string]uint64{
		"pps_allowance_exceeded":    1000,
		"bw_out_allowance_exceeded": 5,
	})
	after := Allowances(map[string]uint64{
		"pps_allowance_exceeded":    1042,
		"bw_out_allowance_exceeded": 5,
	})
	if got, want := after.Sub(before).String(), "allowances exceeded: pps 42"; got != want {
		t.Errorf("a tick that shaped 42 packets renders %q, want %q", got, want)
	}
	if got, want := before.Sub(before).String(), "allowances: none exceeded"; got != want {
		t.Errorf("a quiet tick renders %q, want %q", got, want)
	}
}

// If one of the two readings never happened, the "delta" would really be the
// whole count since boot. Reporting nothing is right; reporting a made-up
// spike is not.
func TestADeltaAgainstAMissingReadingStaysSilent(t *testing.T) {
	var missing ENAAllowances
	got := Allowances(map[string]uint64{"pps_allowance_exceeded": 9999})
	if line := got.Sub(missing).String(); line != "" {
		t.Errorf("a delta against a reading that never happened renders %q", line)
	}
}
