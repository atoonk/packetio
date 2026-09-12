//go:build linux && cgo && dpdk && (amd64 || arm64)

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/dpdk"
	"github.com/atoonk/packetio/examples/internal/metrics"
)

// totals is what the transmit queues have done, summed.
//
// The counters come from the queues rather than from the application, because
// what the application thinks it sent and what the driver took are different
// numbers whenever a ring is full -- and that difference is the interesting
// one.
type totals struct {
	packets, bytes   uint64
	batches          uint64
	completed        uint64
	ringFull, errors uint64
	inFlight         uint64
	poolEmpty        uint64
}

func sum(dev packetio.Device) (totals, error) {
	var t totals
	for i := 0; i < dev.NumTxQueues(); i++ {
		st, err := dev.TxQueue(i).Stats()
		if err != nil {
			return t, err
		}
		t.packets += st.Packets
		t.bytes += st.Bytes
		t.batches += st.Batches
		t.completed += st.Completed
		t.ringFull += st.RingFull
		t.poolEmpty += st.PoolEmpty
		t.errors += st.Errors
		t.inFlight += st.Backend["in_flight"]
	}
	return t, nil
}

func (t totals) sub(o totals) totals {
	return totals{
		packets: t.packets - o.packets, bytes: t.bytes - o.bytes,
		batches: t.batches - o.batches, completed: t.completed - o.completed,
		ringFull: t.ringFull - o.ringFull, poolEmpty: t.poolEmpty - o.poolEmpty,
		errors: t.errors - o.errors, inFlight: t.inFlight,
	}
}

// reportLoop prints a line every interval and a summary at the end.
//
// Two rates are printed where they can be: what the driver took, and what the
// port says it put on the wire. They differ when something is dropped below
// this program, and only the second one is the answer to "what did the wire
// see".
func reportLoop(ctx context.Context, dev packetio.Device, c config) error {
	cpuSampler := metrics.NewCPUSampler(c.cpus)
	hz := c.ghz * 1e9
	if hz == 0 {
		hz = metrics.CPUHz()
	}

	nicOK := c.iface != ""
	var nicStart metrics.NICCounters
	if nicOK {
		n, err := metrics.ReadNIC(c.iface)
		if err != nil || !n.HasPhysicalCounters() {
			nicOK = false
			fmt.Println("note: the port's own counters are not readable, so only what the " +
				"driver took is reported")
		} else {
			nicStart = n
		}
	}
	cpuStart, err := cpuSampler.Sample()
	if err != nil {
		return err
	}
	swStart, err := sum(dev)
	if err != nil {
		return err
	}

	begin := time.Now()
	prevCPU, prevSW, prevNIC, prevAt := cpuStart, swStart, nicStart, begin
	startAllow := readAllowances(dev)
	prevAllow := startAllow

	tick := time.NewTicker(c.report)
	defer tick.Stop()

	for done := false; !done; {
		select {
		case <-ctx.Done():
			done = true
		case <-tick.C:
		}
		now := time.Now()
		elapsed := now.Sub(prevAt)
		prevAt = now

		sw, err := sum(dev)
		if err != nil {
			return err
		}
		cpu, err := cpuSampler.Sample()
		if err != nil {
			return err
		}
		var nic metrics.NICCounters
		if nicOK {
			if n, err := metrics.ReadNIC(c.iface); err == nil {
				nic = n
			} else {
				nicOK = false
			}
		}
		fmt.Println(format(elapsed, sw.sub(prevSW), metrics.Delta(prevCPU, cpu),
			nic.Sub(prevNIC), nicOK, hz, c))
		allow := readAllowances(dev)
		if allow.WentBackwards(prevAllow) {
			fmt.Println("  INVALID: an allowance counter reset mid-run")
		} else if line := allow.Sub(prevAllow).String(); line != "" {
			fmt.Println("  " + line)
		}
		prevSW, prevCPU, prevNIC, prevAllow = sw, cpu, nic, allow
	}

	elapsed := time.Since(begin)
	sw, err := sum(dev)
	if err != nil {
		return err
	}
	cpu, err := cpuSampler.Sample()
	if err != nil {
		return err
	}
	var nic metrics.NICCounters
	if nicOK {
		if n, err := metrics.ReadNIC(c.iface); err == nil {
			nic = n
		}
	}
	fmt.Println()
	fmt.Println("total: " + format(elapsed, sw.sub(swStart), metrics.Delta(cpuStart, cpu),
		nic.Sub(nicStart), nicOK, hz, c))
	if line := readAllowances(dev).Sub(startAllow).String(); line != "" {
		fmt.Println("  over the whole run: " + line)
	}
	if sw.inFlight > 0 {
		// Not a leak: a driver signals completions in its own time, and mlx5
		// asks for one only every 32 packets.
		fmt.Printf("  %d frames still with the driver, which signals completions in its own time\n",
			sw.inFlight)
	}
	if sw.errors > 0 || sw.ringFull > 0 || sw.poolEmpty > 0 {
		fmt.Printf("  %d errors, %d times the ring was full, %d times the pool was empty\n",
			sw.errors, sw.ringFull, sw.poolEmpty)
	}
	return nil
}

func format(elapsed time.Duration, sw totals, cpu metrics.CPUSample,
	nic metrics.NICCounters, nicOK bool, hz float64, c config) string {
	s := elapsed.Seconds()
	if s <= 0 {
		return ""
	}
	mpps := float64(sw.packets) / s / 1e6
	// The wire carries the check sequence and the inter-frame gap as well, so
	// the honest bit rate counts them.
	wire := float64(sw.bytes) + float64(sw.packets)*(frameOverhead)
	gbps := wire * 8 / s / 1e9

	out := fmt.Sprintf("%6.2f Mpps  %6.2f Gbps", mpps, gbps)
	if nicOK && nic.TxPackets > 0 {
		out += fmt.Sprintf("   port %6.2f Mpps", float64(nic.TxPackets)/s/1e6)
	}
	out += fmt.Sprintf("   %5.2f cores", cpu.Cores())
	if hz > 0 && sw.packets > 0 {
		out += fmt.Sprintf("  %6.1f cyc/pkt", cpu.CyclesPerPacket(sw.packets, hz))
	}
	if sw.batches > 0 {
		out += fmt.Sprintf("  %5.1f pkt/batch", float64(sw.packets)/float64(sw.batches))
	}
	return out
}

// frameOverhead is the check sequence, preamble and inter-frame gap the wire
// carries beyond the bytes handed to the device.
const frameOverhead = 4 + 8 + 12

// readAllowances reads EC2's network allowance counters off the device itself.
//
// On a DPDK-owned ENI this is the only way to reach them: the kernel driver
// that answers ethtool -S is not attached to the device, so the -counters
// interface cannot see them. Anywhere else the driver publishes no such
// counters, this comes back empty, and nothing is printed.
func readAllowances(dev packetio.Device) metrics.ENAAllowances {
	d, ok := dev.(*dpdk.Device)
	if !ok {
		return metrics.ENAAllowances{}
	}
	m, err := d.PortXStatsMap()
	if err != nil {
		return metrics.ENAAllowances{}
	}
	return metrics.Allowances(m)
}
