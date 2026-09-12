//go:build linux && cgo && dpdk && (amd64 || arm64)

package dpdk_test

import (
	"os"
	"testing"

	"github.com/atoonk/packetio/dpdk"
	"github.com/atoonk/packetio/dpdk/internal/eal"
)

// The named counters are the only way to read what a NIC reports beyond the
// eight generic ones -- on EC2 that means ENA's shaping counters, which are
// the difference between "the rate flattened here" and "the fabric shaped us
// here, and this is which allowance it was". Two things can go wrong and only
// one of them is obvious: the names can come back truncated or empty, and the
// values can land on the wrong names, because rte_eth_xstats_get returns each
// value tagged with the index of its own name rather than in name order. The
// second is what this test is really for.
func TestPortXStatsNamesAndValuesLineUp(t *testing.T) {
	needRoot(t)
	spec := os.Getenv("PACKETIO_DPDK_DEV")
	if spec == "" {
		t.Skip("set PACKETIO_DPDK_DEV to a device to read counters from")
	}

	d, err := dpdk.Open(spec)
	if err != nil {
		t.Fatalf("opening %s: %v", spec, err)
	}
	defer d.Close()

	before, err := d.PortXStats()
	if err != nil {
		t.Fatalf("PortXStats: %v", err)
	}
	if len(before) == 0 {
		t.Skip("this driver publishes no named counters")
	}
	for i, x := range before {
		if x.Name == "" {
			t.Fatalf("named counter %d came back without a name", i)
		}
	}

	const want = 8
	tx := d.Tx(0)
	if tx == nil {
		t.Skip("no transmit queue")
	}
	sent, err := tx.SendFunc(want, func(_ int, frame []byte) int {
		for i := range frame[:60] {
			frame[i] = 0xff
		}
		return 60
	})
	if err != nil {
		t.Fatalf("transmitting: %v", err)
	}
	tx.Complete(want)

	after, err := d.PortXStats()
	if err != nil {
		t.Fatalf("PortXStats after transmitting: %v", err)
	}
	st, err := d.PortStats()
	if err != nil {
		t.Fatalf("PortStats: %v", err)
	}
	got := xstatsByName(after)

	// The generic counter and the named one are two views of the same event,
	// so they have to agree. If the values had landed on the wrong names this
	// is where it shows: tx_good_packets would hold somebody else's number.
	if got["tx_good_packets"] != st.OutPackets {
		t.Errorf("tx_good_packets is %d but the port says %d packets went out",
			got["tx_good_packets"], st.OutPackets)
	}
	if st.OutPackets == 0 {
		t.Fatalf("nothing went out (%d frames accepted), so there is nothing to check", sent)
	}
	if got["tx_good_packets"] == 0 {
		t.Error("frames went out but tx_good_packets stayed at zero: values are not landing on their names")
	}
	if q, ok := got["tx_q0_packets"]; ok && q != st.OutPackets {
		t.Errorf("tx_q0_packets is %d but %d packets went out", q, st.OutPackets)
	}
}

func xstatsByName(xs []eal.XStat) map[string]uint64 {
	m := make(map[string]uint64, len(xs))
	for _, x := range xs {
		m[x.Name] = x.Value
	}
	return m
}
