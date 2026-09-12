//go:build linux

package metrics

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// NICCounters are the NIC's own counts of what crossed the wire.
//
// They are the only honest answer to "did the packets actually leave". A driver
// counts what it handed to the hardware, which is not the same thing: a frame
// the NIC rejects, or drops for want of buffer, is counted as sent by everyone
// except the NIC.
type NICCounters struct {
	// TxPackets and TxBytes are what the port put on the wire, and RxPackets
	// and RxBytes what it took off.
	TxPackets uint64
	TxBytes   uint64
	RxPackets uint64
	RxBytes   uint64

	// TxErrors and TxDiscards are frames the port would not send, and
	// RxDiscards frames it could not take. On the receiving side of a fast
	// test this is usually where the missing packets are.
	TxErrors   uint64
	TxDiscards uint64
	RxDiscards uint64

	// RxOutOfBuffer is receive work the NIC had nowhere to put, which means
	// the receive queue was not refilled fast enough.
	RxOutOfBuffer uint64

	// Raw is every counter that was read, for the ones not named above.
	Raw map[string]uint64
}

// Sub is the difference between two readings.
func (c NICCounters) Sub(o NICCounters) NICCounters {
	d := NICCounters{
		TxPackets: c.TxPackets - o.TxPackets, TxBytes: c.TxBytes - o.TxBytes,
		RxPackets: c.RxPackets - o.RxPackets, RxBytes: c.RxBytes - o.RxBytes,
		TxErrors: c.TxErrors - o.TxErrors, TxDiscards: c.TxDiscards - o.TxDiscards,
		RxDiscards: c.RxDiscards - o.RxDiscards, RxOutOfBuffer: c.RxOutOfBuffer - o.RxOutOfBuffer,
		Raw: map[string]uint64{},
	}
	for k, v := range c.Raw {
		d.Raw[k] = v - o.Raw[k]
	}
	return d
}

// ReadNIC reads a port's counters.
//
// The physical counters this wants are not in sysfs, because sysfs reports what
// the kernel's own driver saw and this packet path does not go through it. They
// come from ethtool, which asks the hardware.
func ReadNIC(iface string) (NICCounters, error) {
	out, err := exec.Command("ethtool", "-S", iface).Output()
	if err != nil {
		return NICCounters{}, fmt.Errorf("metrics: reading %s counters with ethtool: %w "+
			"(the physical counters are not in sysfs, so ethtool must be installed)", iface, err)
	}
	return parseEthtool(out), nil
}

func parseEthtool(out []byte) NICCounters {
	c := NICCounters{Raw: map[string]uint64{}}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		name, value, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		v, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if err != nil {
			continue
		}
		c.Raw[name] = v

		// The _phy counters are the port's own, which is what matters here.
		// The others are the kernel driver's view and will not see traffic
		// that bypassed it.
		switch name {
		case "tx_packets_phy":
			c.TxPackets = v
		case "tx_bytes_phy":
			c.TxBytes = v
		case "rx_packets_phy":
			c.RxPackets = v
		case "rx_bytes_phy":
			c.RxBytes = v
		case "tx_errors_phy":
			c.TxErrors = v
		case "tx_discards_phy":
			c.TxDiscards = v
		case "rx_discards_phy":
			c.RxDiscards = v
		case "rx_out_of_buffer":
			c.RxOutOfBuffer = v
		}
	}
	return c
}

// HasPhysicalCounters reports whether the port exposed the counters that
// describe the wire rather than the kernel's view of it.
func (c NICCounters) HasPhysicalCounters() bool {
	_, ok := c.Raw["tx_packets_phy"]
	return ok
}

// ENAAllowances are the counters EC2's Nitro card publishes to say which of an
// instance's network allowances it is shaping traffic against. They are the
// difference between "the rate flattened here" and "the fabric shaped us here,
// and this is which quota did it", which is the only way to tell an instance's
// real limit from the generator running out of steam.
//
// Every one is cumulative since the driver last reset, so only deltas mean
// anything, and a counter that goes backwards means the driver reset
// underneath the measurement and the window is void.
//
// PPSExceeded is the one the packet-per-second work is about, and AWS
// documents it as bidirectional and per instance: receive and transmit draw on
// one budget. Note that it counts packets "queued or dropped", so a nonzero
// value alone only proves a microburst grazed the shaper -- it takes a
// shortfall of delivered against offered to show a real limit.
type ENAAllowances struct {
	PPSExceeded        uint64
	BwInExceeded       uint64
	BwOutExceeded      uint64
	ConntrackExceeded  uint64
	LinklocalExceeded  uint64
	ConntrackAvailable uint64

	// Present is false on anything that is not an ENA, which is every NIC
	// outside EC2 and an ENA behind a driver too old to publish them.
	Present bool
}

// enaAllowanceNames maps each counter to where it lands. The names are the
// same whether they came from the kernel driver through ethtool -S or from the
// poll-mode driver through rte_eth_xstats, which is what lets one instance be
// measured either way.
var enaAllowanceNames = map[string]func(*ENAAllowances) *uint64{
	"pps_allowance_exceeded":        func(a *ENAAllowances) *uint64 { return &a.PPSExceeded },
	"bw_in_allowance_exceeded":      func(a *ENAAllowances) *uint64 { return &a.BwInExceeded },
	"bw_out_allowance_exceeded":     func(a *ENAAllowances) *uint64 { return &a.BwOutExceeded },
	"conntrack_allowance_exceeded":  func(a *ENAAllowances) *uint64 { return &a.ConntrackExceeded },
	"linklocal_allowance_exceeded":  func(a *ENAAllowances) *uint64 { return &a.LinklocalExceeded },
	"conntrack_allowance_available": func(a *ENAAllowances) *uint64 { return &a.ConntrackAvailable },
}

// Allowances picks the ENA allowance counters out of any set of named
// counters: NICCounters.Raw from ethtool, or the driver's own xstats read
// straight from a device this process owns.
func Allowances(raw map[string]uint64) ENAAllowances {
	var a ENAAllowances
	for name, field := range enaAllowanceNames {
		if v, ok := raw[name]; ok {
			*field(&a) = v
			a.Present = true
		}
	}
	return a
}

// Sub returns the change between two readings. ConntrackAvailable is a level
// rather than a running total, so it is carried across as the later reading
// instead of a difference.
func (a ENAAllowances) Sub(o ENAAllowances) ENAAllowances {
	return ENAAllowances{
		PPSExceeded:        a.PPSExceeded - o.PPSExceeded,
		BwInExceeded:       a.BwInExceeded - o.BwInExceeded,
		BwOutExceeded:      a.BwOutExceeded - o.BwOutExceeded,
		ConntrackExceeded:  a.ConntrackExceeded - o.ConntrackExceeded,
		LinklocalExceeded:  a.LinklocalExceeded - o.LinklocalExceeded,
		ConntrackAvailable: a.ConntrackAvailable,
		Present:            a.Present && o.Present,
	}
}

// WentBackwards reports whether any cumulative counter fell, which means the
// driver reset mid-measurement and the deltas are meaningless.
func (a ENAAllowances) WentBackwards(o ENAAllowances) bool {
	return a.PPSExceeded < o.PPSExceeded ||
		a.BwInExceeded < o.BwInExceeded ||
		a.BwOutExceeded < o.BwOutExceeded ||
		a.ConntrackExceeded < o.ConntrackExceeded ||
		a.LinklocalExceeded < o.LinklocalExceeded
}

// String renders a delta as one line for a periodic report, naming only the
// allowances that actually fired so that a quiet run stays quiet.
func (a ENAAllowances) String() string {
	if !a.Present {
		return ""
	}
	var b strings.Builder
	add := func(name string, v uint64) {
		if v == 0 {
			return
		}
		if b.Len() > 0 {
			b.WriteString("  ")
		}
		fmt.Fprintf(&b, "%s %d", name, v)
	}
	add("pps", a.PPSExceeded)
	add("bw_in", a.BwInExceeded)
	add("bw_out", a.BwOutExceeded)
	add("conntrack", a.ConntrackExceeded)
	add("linklocal", a.LinklocalExceeded)
	if b.Len() == 0 {
		return "allowances: none exceeded"
	}
	return "allowances exceeded: " + b.String()
}
