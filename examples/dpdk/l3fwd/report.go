//go:build linux && cgo && dpdk && amd64

package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/examples/internal/forward"
	"github.com/atoonk/packetio/examples/internal/metrics"
)

// totals is what the workers and their queues have done, summed.
type totals struct {
	rx, fwd  uint64
	drops    [forward.NumErrors]uint64
	dropped  uint64
	inFlight uint64
}

func sumWorkers(cnts []counters) totals {
	var t totals
	for i := range cnts {
		t.rx += cnts[i].rx.Load()
		t.fwd += cnts[i].fwd.Load()
		for j := range cnts[i].drops {
			n := cnts[i].drops[j].Load()
			t.drops[j] += n
			if forward.ErrorCode(j) != forward.ErrNone {
				t.dropped += n
			}
		}
	}
	return t
}

func (t totals) sub(o totals) totals {
	d := totals{rx: t.rx - o.rx, fwd: t.fwd - o.fwd, dropped: t.dropped - o.dropped}
	for i := range t.drops {
		d.drops[i] = t.drops[i] - o.drops[i]
	}
	return d
}

// inFlight is how many frames the transmit queues still hold. It is reported
// because it is the number that looks like a leak and is not: a driver signals
// completions when it chooses.
func inFlight(dev packetio.Device) uint64 {
	var n uint64
	for i := 0; i < dev.NumTxQueues(); i++ {
		if st, err := dev.TxQueue(i).Stats(); err == nil {
			n += st.Backend["in_flight"]
		}
	}
	return n
}

func reportLoop(ctx context.Context, dev packetio.Device, c config, cnts []counters) error {
	cpuSampler := metrics.NewCPUSampler(c.cpus)
	hz := metrics.CPUHz()

	nicOK := c.iface != ""
	var nicStart metrics.NICCounters
	if nicOK {
		n, err := metrics.ReadNIC(c.iface)
		if err != nil || !n.HasPhysicalCounters() {
			nicOK = false
		} else {
			nicStart = n
		}
	}
	cpuStart, err := cpuSampler.Sample()
	if err != nil {
		return err
	}
	swStart := sumWorkers(cnts)

	begin := time.Now()
	prevCPU, prevSW, prevNIC, prevAt := cpuStart, swStart, nicStart, begin

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

		sw := sumWorkers(cnts)
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
			nic.Sub(prevNIC), nicOK, hz))
		prevSW, prevCPU, prevNIC = sw, cpu, nic
	}

	elapsed := time.Since(begin)
	sw := sumWorkers(cnts)
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
	total := sw.sub(swStart)
	fmt.Println()
	fmt.Println("total: " + format(elapsed, total, metrics.Delta(cpuStart, cpu),
		nic.Sub(nicStart), nicOK, hz))

	// Every packet is accounted for: forwarded, or dropped for a named reason.
	if why := reasons(total); why != "" {
		fmt.Println("  dropped: " + why)
	}
	if got, want := total.fwd+total.dropped, total.rx; got != want {
		fmt.Printf("  %d received but %d accounted for: %d unexplained\n", want, got, want-got)
	} else {
		fmt.Printf("  %d received, %d forwarded, %d dropped, every packet accounted for\n",
			total.rx, total.fwd, total.dropped)
	}
	if n := inFlight(dev); n > 0 {
		fmt.Printf("  %d frames still with the driver, which signals completions in its own time\n", n)
	}
	return nil
}

func reasons(t totals) string {
	var parts []string
	for i, n := range t.drops {
		if n == 0 || forward.ErrorCode(i) == forward.ErrNone {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %d", forward.ErrorCode(i), n))
	}
	return strings.Join(parts, ", ")
}

func format(elapsed time.Duration, sw totals, cpu metrics.CPUSample,
	nic metrics.NICCounters, nicOK bool, hz float64) string {
	s := elapsed.Seconds()
	if s <= 0 {
		return ""
	}
	out := fmt.Sprintf("in %6.2f Mpps  out %6.2f Mpps", float64(sw.rx)/s/1e6, float64(sw.fwd)/s/1e6)
	if sw.dropped > 0 {
		out += fmt.Sprintf("  dropped %d", sw.dropped)
	}
	if nicOK {
		if missed := nic.RxDiscards + nic.RxOutOfBuffer; missed > 0 {
			out += fmt.Sprintf("  port missed %d", missed)
		}
	}
	out += fmt.Sprintf("   %5.2f cores", cpu.Cores())
	if hz > 0 && sw.rx > 0 {
		out += fmt.Sprintf("  %6.1f cyc/pkt", cpu.CyclesPerPacket(sw.rx, hz))
	}
	return out
}
