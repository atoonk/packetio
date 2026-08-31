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
