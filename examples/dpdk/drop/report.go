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

// totals is what the receive queues have done, summed.
type totals struct {
	packets, bytes uint64
	filled         uint64
	batches        uint64
	polls          uint64
	poolEmpty      uint64
	errors         uint64
	dropped        uint64
	outstanding    uint64
}

func sum(dev packetio.Device) (totals, error) {
	var t totals
	for i := 0; i < dev.NumRxQueues(); i++ {
		st, err := dev.RxQueue(i).Stats()
		if err != nil {
			return t, err
		}
		t.packets += st.Packets
		t.bytes += st.Bytes
		t.filled += st.Filled
		t.batches += st.Batches
		t.polls += st.Polls
		t.poolEmpty += st.PoolEmpty
		t.errors += st.Errors
		t.dropped += st.Dropped
		t.outstanding += st.Backend["outstanding"]
	}
	return t, nil
}

func (t totals) sub(o totals) totals {
	return totals{
		packets: t.packets - o.packets, bytes: t.bytes - o.bytes,
		filled: t.filled - o.filled, batches: t.batches - o.batches,
		polls:     t.polls - o.polls,
		poolEmpty: t.poolEmpty - o.poolEmpty,
		errors:    t.errors - o.errors, dropped: t.dropped - o.dropped,
		outstanding: t.outstanding,
	}
}

// reportLoop prints a line every interval and a summary at the end.
//
// The port's own counters matter more here than on the transmit side: a packet
// the NIC dropped for want of a buffer never reaches this program at all, and
// only imissed says so.
func reportLoop(ctx context.Context, dev packetio.Device, c config, good []counter) error {
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
			fmt.Println("note: the port's own counters are not readable, so packets the NIC " +
				"dropped will not be visible here")
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
	startPort := readPort(dev)
	prevPort := startPort

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
		curPort := readPort(dev)
		fmt.Println(format(elapsed, sw.sub(prevSW), metrics.Delta(prevCPU, cpu),
			nic.Sub(prevNIC), nicOK, hz, curPort.sub(prevPort)))
		prevPort = curPort
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
		nic.Sub(nicStart), nicOK, hz, readPort(dev).sub(startPort)))
	if line := readAllowances(dev).Sub(startAllow).String(); line != "" {
		fmt.Println("  over the whole run: " + line)
	}
	if sw.batches > 0 {
		// Packets per receive call: the number that says whether the loop is
		// taking packets in useful sizes. A backend once lost half its
		// receive rate to bursts that had collapsed to a handful of packets,
		// and no other counter here would have shown it.
		fmt.Printf("  %.1f packets per receive call over %d calls\n",
			float64(sw.packets)/float64(sw.batches), sw.batches)
	}
	if c.verify {
		var ok uint64
		for i := range good {
			ok += good[i].n.Load()
		}
		fmt.Printf("  %d of %d packets carried a checksum the NIC verified\n", ok, sw.packets-swStart.packets)
	}
	if sw.errors > 0 || sw.poolEmpty > 0 {
		fmt.Printf("  %d receive errors, %d refills that found no free frame\n",
			sw.errors, sw.poolEmpty)
	}
	return nil
}

func format(elapsed time.Duration, sw totals, cpu metrics.CPUSample,
	nic metrics.NICCounters, nicOK bool, hz float64, port portCounters) string {
	s := elapsed.Seconds()
	if s <= 0 {
		return ""
	}
	mpps := float64(sw.packets) / s / 1e6
	wire := float64(sw.bytes) + float64(sw.packets)*frameOverhead
	out := fmt.Sprintf("%6.2f Mpps  %6.2f Gbps", mpps, wire*8/s/1e9)
	if nicOK && nic.RxPackets > 0 {
		out += fmt.Sprintf("   port %6.2f Mpps", float64(nic.RxPackets)/s/1e6)
		// Frames the NIC had nowhere to put. It is the number that says the
		// receiver could not keep up, and it is invisible from inside this
		// program: those packets never reached it.
		if missed := nic.RxDiscards + nic.RxOutOfBuffer; missed > 0 {
			out += fmt.Sprintf("  missed %d", missed)
		}
	} else if port.ok {
		// The same question, asked of the device rather than of the kernel.
		// A device this process owns has no kernel interface to read, which is
		// exactly the case where "did we drop these, or did the fabric?" is
		// hardest to answer and matters most: missed is the receiver failing to
		// keep up, and it is a different fact from an allowance being exceeded.
		out += fmt.Sprintf("   port %6.2f Mpps", float64(port.rx)/s/1e6)
		if port.missed > 0 {
			out += fmt.Sprintf("  missed %d", port.missed)
		}
		if port.noBuf > 0 {
			out += fmt.Sprintf("  nobuf %d", port.noBuf)
		}
	}
	out += fmt.Sprintf("   %5.2f cores", cpu.Cores())
	if hz > 0 && sw.packets > 0 {
		out += fmt.Sprintf("  %6.1f cyc/pkt", cpu.CyclesPerPacket(sw.packets, hz))
	}
	return out
}

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

// portCounters is what the device itself says it received and could not place.
// Read from the port rather than from a kernel interface, because a device
// bound to a userspace driver has no kernel interface to ask.
type portCounters struct {
	rx, missed, noBuf uint64
	ok                bool
}

func readPort(dev packetio.Device) portCounters {
	d, ok := dev.(*dpdk.Device)
	if !ok {
		return portCounters{}
	}
	st, err := d.PortStats()
	if err != nil {
		return portCounters{}
	}
	return portCounters{rx: st.InPackets, missed: st.Missed, noBuf: st.NoBuffer, ok: true}
}

func (p portCounters) sub(o portCounters) portCounters {
	if !p.ok || !o.ok {
		return portCounters{}
	}
	return portCounters{rx: p.rx - o.rx, missed: p.missed - o.missed, noBuf: p.noBuf - o.noBuf, ok: true}
}
