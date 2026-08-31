//go:build linux && cgo && mlx5 && (amd64 || arm64)

package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/atoonk/packetio/examples/internal/forward"
	"github.com/atoonk/packetio/examples/internal/metrics"
	"github.com/atoonk/packetio/mlx5"
)

// totals is what the workers and their queues have done, summed.
type totals struct {
	rx, fwd uint64
	drops   [forward.NumErrors]uint64
	dropped uint64 // all of the above

	rxErrors, txErrors uint64
	poolEmpty          uint64

	perWorker []uint64 // forwarded, by worker
}

func sum(dev *mlx5.Device, counts []counters) (totals, error) {
	var t totals
	t.perWorker = make([]uint64, len(counts))
	for i := range counts {
		t.rx += counts[i].rx.Load()
		t.fwd += counts[i].fwd.Load()
		t.perWorker[i] = counts[i].fwd.Load()
		for j := range counts[i].drops {
			n := counts[i].drops[j].Load()
			t.drops[j] += n
			t.dropped += n
		}
	}
	for i := 0; i < dev.NumRxQueues(); i++ {
		st, err := dev.RxQueue(i).Stats()
		if err != nil {
			return t, err
		}
		t.rxErrors += st.Errors
		t.poolEmpty += st.PoolEmpty
	}
	for i := 0; i < dev.NumTxQueues(); i++ {
		st, err := dev.TxQueue(i).Stats()
		if err != nil {
			return t, err
		}
		t.txErrors += st.Errors
	}
	return t, nil
}

func (t totals) sub(o totals) totals {
	d := totals{
		rx: t.rx - o.rx, fwd: t.fwd - o.fwd, dropped: t.dropped - o.dropped,
		rxErrors: t.rxErrors - o.rxErrors, txErrors: t.txErrors - o.txErrors,
		poolEmpty: t.poolEmpty - o.poolEmpty,
	}
	for i := range t.drops {
		d.drops[i] = t.drops[i] - o.drops[i]
	}
	return d
}

// reportLoop prints a line every interval and a summary at the end.
func reportLoop(ctx context.Context, dev *mlx5.Device, c config, cpus []int, counts []counters) error {
	out, err := newCSV(c.csvPath)
	if err != nil {
		return err
	}
	defer out.close()

	cpuSampler := metrics.NewCPUSampler(cpus)
	hz := c.ghz * 1e9
	if hz == 0 {
		hz = metrics.CPUHz()
	}

	nicOK := true
	nicStart, err := metrics.ReadNIC(c.iface)
	if err != nil || !nicStart.HasPhysicalCounters() {
		nicOK = false
		fmt.Println("note: the port's own counters are not readable, so only what the workers saw is reported")
	}
	cpuStart, err := cpuSampler.Sample()
	if err != nil {
		return err
	}
	swStart, err := sum(dev, counts)
	if err != nil {
		return err
	}

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

		sw, err := sum(dev, counts)
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
		line, rec := format(now.Sub(begin), elapsed, sw.sub(prevSW), metrics.Delta(prevCPU, cpu), nic.Sub(prevNIC), nicOK, hz, "sample")
		fmt.Println(line)
		out.write(rec)
		prevSW, prevCPU, prevNIC = sw, cpu, nic
	}

	// The summary covers the whole run, which is the number to quote.
	total := time.Since(begin)
	sw, _ := sum(dev, counts)
	cpu, _ := cpuSampler.Sample()
	var nic metrics.NICCounters
	if nicOK {
		nic, _ = metrics.ReadNIC(c.iface)
	}
	dsw := sw.sub(swStart)
	dnic := nic.Sub(nicStart)

	fmt.Println()
	line, rec := format(total, total, dsw, metrics.Delta(cpuStart, cpu), dnic, nicOK, hz, "total")
	fmt.Println(line)
	out.write(rec)

	fmt.Printf("           received %d, forwarded %d, dropped %d%s\n", dsw.rx, dsw.fwd, dsw.dropped, dropNote(dsw))
	if len(counts) > 1 {
		// How evenly the hash spread the traffic, which is what decides
		// whether adding a worker adds its share.
		var parts []string
		for i := range counts {
			parts = append(parts, si(float64(counts[i].fwd.Load()-swStart.perWorker[i])/total.Seconds()))
		}
		fmt.Printf("           per worker: %s pps\n", strings.Join(parts, " "))
	}
	if dsw.poolEmpty > 0 {
		fmt.Printf("           %d refills found no free frame\n", dsw.poolEmpty)
	}
	if nicOK {
		fmt.Printf("           the port took %d frames and sent %d; %d arrived with no buffer to go in, %d were discarded, %d transmit errors\n",
			dnic.RxPackets, dnic.TxPackets, dnic.RxOutOfBuffer, dnic.RxDiscards, dnic.TxErrors)
		if dnic.TxPackets < dsw.fwd {
			fmt.Printf("           %d packets were queued but did not reach the wire\n", dsw.fwd-dnic.TxPackets)
		}
	}
	if dsw.rxErrors+dsw.txErrors > 0 {
		return fmt.Errorf("the hardware reported %d receive and %d transmit errors", dsw.rxErrors, dsw.txErrors)
	}
	return nil
}

// format turns one interval into a line for a person and a row for a machine.
func format(since, elapsed time.Duration, sw totals, cpu metrics.CPUSample, nic metrics.NICCounters, nicOK bool, hz float64, kind string) (string, []string) {
	secs := elapsed.Seconds()
	if secs <= 0 {
		secs = 1
	}
	// Prefer the port's own count of what left: it is what a router is for.
	forwarded := sw.fwd
	if nicOK && nic.TxPackets > 0 {
		forwarded = nic.TxPackets
	}
	cores := cpu.Cores()
	cyclesPer := cpu.CyclesPerPacket(forwarded, hz)

	line := fmt.Sprintf("[%5.1fs] in %8s pps  out %8s pps  %5.2f cores  %6.0f cycles/pkt",
		since.Seconds(), si(float64(sw.rx)/secs), si(float64(forwarded)/secs), cores, cyclesPer)
	if cores > 0 {
		line += fmt.Sprintf("  %s pps/core", si(float64(forwarded)/secs/cores))
	}
	if sw.dropped > 0 {
		line += fmt.Sprintf("  %s dropped%s", si(float64(sw.dropped)/secs), dropNote(sw))
	}
	if nicOK {
		if nic.RxOutOfBuffer > 0 {
			line += fmt.Sprintf("  %s with no buffer", si(float64(nic.RxOutOfBuffer)/secs))
		}
		if nic.RxDiscards > 0 {
			line += fmt.Sprintf("  %s discarded by the port", si(float64(nic.RxDiscards)/secs))
		}
		if offered := nic.RxPackets - nic.RxOutOfBuffer; offered > 0 && sw.rx < offered {
			line += fmt.Sprintf("  (port took %s)", si(float64(offered)/secs))
		}
	}
	if sw.rxErrors+sw.txErrors > 0 {
		line += fmt.Sprintf("  %d ERRORS", sw.rxErrors+sw.txErrors)
	}

	rec := []string{
		strconv.FormatFloat(since.Seconds(), 'f', 3, 64), kind,
		strconv.FormatUint(sw.rx, 10), strconv.FormatUint(sw.fwd, 10),
		strconv.FormatUint(sw.dropped, 10), strconv.FormatUint(sw.drops[forward.ErrTxFull], 10),
		strconv.FormatUint(nic.RxPackets, 10), strconv.FormatUint(nic.TxPackets, 10),
		strconv.FormatUint(nic.RxOutOfBuffer, 10), strconv.FormatUint(nic.RxDiscards, 10),
		strconv.FormatUint(nic.TxErrors, 10),
		strconv.FormatFloat(cores, 'f', 3, 64),
		strconv.FormatFloat(cpu.Process, 'f', 3, 64),
		strconv.FormatFloat(cyclesPer, 'f', 1, 64),
	}
	return line, rec
}

// dropNote says why packets were dropped, when any were.
func dropNote(t totals) string {
	if t.dropped == 0 {
		return ""
	}
	var parts []string
	for i := forward.ErrorCode(1); i < forward.NumErrors; i++ {
		if n := t.drops[i]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, i))
		}
	}
	return " (" + strings.Join(parts, ", ") + ")"
}
