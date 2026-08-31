//go:build linux && cgo && mlx5 && (amd64 || arm64)

package main

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/atoonk/packetio/examples/internal/metrics"
	"github.com/atoonk/packetio/mlx5"
)

// Every Ethernet frame carries 20 bytes on the wire that are not in the frame:
// seven of preamble, one start-of-frame delimiter, and twelve of gap before the
// next one. Line rate is measured including them, so a rate that ignores them
// understates how full the link is.
const wireOverhead = 20

// totals is what the queues have done, summed.
type totals struct {
	packets, bytes   uint64
	batches          uint64
	completions      uint64
	ringFull, errors uint64
	inFlight         uint64
}

func sum(dev *mlx5.Device) (totals, error) {
	var t totals
	for i := 0; i < dev.NumTxQueues(); i++ {
		st, err := dev.TxQueue(i).Stats()
		if err != nil {
			return t, err
		}
		t.packets += st.Packets
		t.bytes += st.Bytes
		t.batches += st.Batches
		t.completions += st.Completions
		t.ringFull += st.RingFull
		t.errors += st.Errors
		t.inFlight += st.Backend["in_flight"]
	}
	return t, nil
}

func (t totals) sub(o totals) totals {
	return totals{
		packets: t.packets - o.packets, bytes: t.bytes - o.bytes,
		batches: t.batches - o.batches, completions: t.completions - o.completions,
		ringFull: t.ringFull - o.ringFull, errors: t.errors - o.errors,
		inFlight: t.inFlight,
	}
}

// reportLoop prints a line every interval and a summary at the end.
func reportLoop(ctx context.Context, dev *mlx5.Device, cfg config, cpus []int) error {
	out, err := newCSV(cfg.csvPath)
	if err != nil {
		return err
	}
	defer out.close()

	cpuSampler := metrics.NewCPUSampler(cpus)
	hz := cfg.ghz * 1e9
	if hz == 0 {
		hz = metrics.CPUHz()
	}

	nicOK := true
	nicStart, err := metrics.ReadNIC(cfg.iface)
	if err != nil || !nicStart.HasPhysicalCounters() {
		nicOK = false
		fmt.Println("note: the port's own counters are not readable, so only what the driver queued is reported")
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
	prevCPU, prevSW, prevNIC := cpuStart, swStart, nicStart
	prevAt := begin

	tick := time.NewTicker(cfg.report)
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
			if n, err := metrics.ReadNIC(cfg.iface); err == nil {
				nic = n
			} else {
				nicOK = false
			}
		}

		dsw := sw.sub(prevSW)
		dcpu := metrics.Delta(prevCPU, cpu)
		dnic := nic.Sub(prevNIC)
		prevSW, prevCPU, prevNIC = sw, cpu, nic

		line, rec := format(now.Sub(begin), elapsed, dsw, dcpu, dnic, nicOK, cfg, hz, "sample")
		fmt.Println(line)
		out.write(rec)
	}

	// The summary covers the whole run, which is the number to quote: a single
	// second can catch a queue mid-stall.
	total := time.Since(begin)
	sw, _ := sum(dev)
	cpu, _ := cpuSampler.Sample()
	var nic metrics.NICCounters
	if nicOK {
		nic, _ = metrics.ReadNIC(cfg.iface)
	}
	dsw := sw.sub(swStart)
	dcpu := metrics.Delta(cpuStart, cpu)
	dnic := nic.Sub(nicStart)

	fmt.Println()
	line, rec := format(total, total, dsw, dcpu, dnic, nicOK, cfg, hz, "total")
	fmt.Println(line)
	out.write(rec)

	fmt.Printf("           %d batches of %.1f packets, %d completions (%.1f packets each), %d stalls, %d errors\n",
		dsw.batches, per(dsw.packets, dsw.batches), dsw.completions, per(dsw.packets, dsw.completions),
		dsw.ringFull, dsw.errors)
	if nicOK {
		fmt.Printf("           the port sent %d frames, %d errors, %d discards\n",
			dnic.TxPackets, dnic.TxErrors, dnic.TxDiscards)
		if dnic.TxPackets < dsw.packets {
			fmt.Printf("           %d packets were queued but did not reach the wire\n", dsw.packets-dnic.TxPackets)
		}
	}
	if dsw.errors > 0 {
		return fmt.Errorf("the hardware reported %d errors", dsw.errors)
	}
	return nil
}

// format turns one interval into a line for a person and a row for a machine.
func format(since, elapsed time.Duration, sw totals, cpu metrics.CPUSample, nic metrics.NICCounters, nicOK bool, cfg config, hz float64, kind string) (string, []string) {
	secs := elapsed.Seconds()
	if secs <= 0 {
		secs = 1
	}

	// Prefer the port's own count: it is what actually left.
	packets := sw.packets
	if nicOK && nic.TxPackets > 0 {
		packets = nic.TxPackets
	}
	pps := float64(packets) / secs
	l2 := (float64(sw.bytes) + float64(sw.packets)*4) * 8 / secs                // frames plus their check sequence
	l1 := (float64(sw.bytes) + float64(sw.packets)*(4+wireOverhead)) * 8 / secs // and the gap between them

	cores := cpu.Cores()
	cyclesPer := cpu.CyclesPerPacket(packets, hz)

	line := fmt.Sprintf("[%5.1fs] %8s pps  %6.2f Gbit/s L1  %6.2f Gbit/s L2  %5.2f cores  %6.0f cycles/pkt",
		since.Seconds(), si(pps), l1/1e9, l2/1e9, cores, cyclesPer)
	if cores > 0 {
		line += fmt.Sprintf("  %s pps/core", si(pps/cores))
	}
	if sw.ringFull > 0 {
		line += fmt.Sprintf("  %d stalls", sw.ringFull)
	}
	if sw.errors > 0 {
		line += fmt.Sprintf("  %d ERRORS", sw.errors)
	}
	if nicOK && nic.TxPackets != sw.packets {
		line += fmt.Sprintf("  (driver queued %s)", si(float64(sw.packets)/secs))
	}

	rec := []string{
		strconv.FormatFloat(since.Seconds(), 'f', 3, 64), kind,
		strconv.FormatUint(packets, 10), strconv.FormatUint(sw.bytes, 10),
		strconv.FormatFloat(pps, 'f', 0, 64),
		strconv.FormatFloat(l1, 'f', 0, 64), strconv.FormatFloat(l2, 'f', 0, 64),
		strconv.FormatUint(nic.TxPackets, 10), strconv.FormatUint(nic.TxErrors, 10),
		strconv.FormatUint(nic.TxDiscards, 10),
		strconv.FormatFloat(cores, 'f', 3, 64),
		strconv.FormatFloat(cpu.Process, 'f', 3, 64),
		strconv.FormatFloat(cpu.All.SoftIRQ, 'f', 3, 64),
		strconv.FormatFloat(cyclesPer, 'f', 1, 64),
		strconv.FormatUint(sw.batches, 10), strconv.FormatUint(sw.completions, 10),
		strconv.FormatUint(sw.ringFull, 10), strconv.FormatUint(sw.errors, 10),
		strconv.FormatUint(sw.inFlight, 10),
	}
	return line, rec
}

func per(a, b uint64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}
