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
// preamble, start-of-frame delimiter and the gap before the next one.
const wireOverhead = 20

type totals struct {
	packets, bytes uint64
	filled         uint64
	batches        uint64
	completions    uint64
	poolEmpty      uint64
	errors         uint64
	outstanding    uint64
}

func sum(dev *mlx5.Device) (totals, error) {
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
		t.completions += st.Completions
		t.poolEmpty += st.PoolEmpty
		t.errors += st.Errors
		t.outstanding += st.Backend["outstanding"]
	}
	return t, nil
}

func (t totals) sub(o totals) totals {
	return totals{
		packets: t.packets - o.packets, bytes: t.bytes - o.bytes,
		filled: t.filled - o.filled, batches: t.batches - o.batches,
		completions: t.completions - o.completions,
		poolEmpty:   t.poolEmpty - o.poolEmpty, errors: t.errors - o.errors,
		outstanding: t.outstanding,
	}
}

func reportLoop(ctx context.Context, dev *mlx5.Device, c cfg, cpus []int, wrong *[16]counter) error {
	cpuSampler := metrics.NewCPUSampler(cpus)
	hz := c.ghz * 1e9
	if hz == 0 {
		hz = metrics.CPUHz()
	}

	nicOK := true
	nicStart, err := metrics.ReadNIC(c.iface)
	if err != nil || !nicStart.HasPhysicalCounters() {
		nicOK = false
		fmt.Println("note: the port's own counters are not readable, so what it dropped cannot be reported")
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

		fmt.Println(format(now.Sub(begin), elapsed, sw.sub(prevSW), metrics.Delta(prevCPU, cpu), nic.Sub(prevNIC), nicOK, hz))
		prevSW, prevCPU, prevNIC = sw, cpu, nic
	}

	total := time.Since(begin)
	sw, _ := sum(dev)
	cpu, _ := cpuSampler.Sample()
	var nic metrics.NICCounters
	if nicOK {
		nic, _ = metrics.ReadNIC(c.iface)
	}
	dsw := sw.sub(swStart)
	dnic := nic.Sub(nicStart)

	fmt.Println()
	fmt.Println(format(total, total, dsw, metrics.Delta(cpuStart, cpu), dnic, nicOK, hz))
	fmt.Printf("           %d buffers posted, %d completions, %d packets, %d errors\n",
		dsw.filled, dsw.completions, dsw.packets, dsw.errors)
	if dsw.batches > 0 {
		// Packets per receive call. A loop taking the offered load a handful
		// of packets at a time pays a call's fixed cost over too few of them,
		// and nothing else printed here would show it.
		fmt.Printf("           %.1f packets per receive call over %d calls\n",
			float64(dsw.packets)/float64(dsw.batches), dsw.batches)
	}
	if dsw.poolEmpty > 0 {
		fmt.Printf("           %d refills found no free frame: the application was holding them all\n", dsw.poolEmpty)
	}
	if nicOK {
		fmt.Printf("           the port saw %d frames and dropped %d, %d of them for want of a buffer\n",
			dnic.RxPackets, dnic.RxDiscards, dnic.RxOutOfBuffer)
		if missing := int64(dnic.RxPackets) - int64(dnic.RxDiscards) - int64(dsw.packets); missing > 0 {
			fmt.Printf("           %d frames the port took are not accounted for here; some of them went to the kernel\n", missing)
		}
	}
	if c.check && c.vlan >= 0 {
		var bad uint64
		for i := range wrong {
			bad += wrong[i].n
		}
		if bad > 0 {
			fmt.Printf("           %d packets did not carry vlan %d\n", bad, c.vlan)
		} else {
			fmt.Printf("           every packet carried vlan %d\n", c.vlan)
		}
	}
	if c.sample {
		for i := range wrong {
			if len(wrong[i].seq) > 0 {
				fmt.Printf("           first packet on a queue: % x\n", wrong[i].seq[:min(32, len(wrong[i].seq))])
				break
			}
		}
	}
	if dsw.errors > 0 {
		return fmt.Errorf("the hardware reported %d errors", dsw.errors)
	}
	return nil
}

func format(since, elapsed time.Duration, sw totals, cpu metrics.CPUSample, nic metrics.NICCounters, nicOK bool, hz float64) string {
	secs := elapsed.Seconds()
	if secs <= 0 {
		secs = 1
	}
	pps := float64(sw.packets) / secs
	l1 := (float64(sw.bytes) + float64(sw.packets)*(4+wireOverhead)) * 8 / secs
	cores := cpu.Cores()

	line := fmt.Sprintf("[%5.1fs] %8s pps  %6.2f Gbit/s L1  %5.2f cores  %6.0f cycles/pkt",
		since.Seconds(), si(pps), l1/1e9, cores, cpu.CyclesPerPacket(sw.packets, hz))
	if cores > 0 {
		line += fmt.Sprintf("  %s pps/core", si(pps/cores))
	}
	if nicOK {
		drop := float64(nic.RxDiscards) / secs
		if drop > 0 {
			line += fmt.Sprintf("  %s dropped by the port", si(drop))
		}
		if nic.RxOutOfBuffer > 0 {
			line += fmt.Sprintf("  %s with no buffer", si(float64(nic.RxOutOfBuffer)/secs))
		}
	}
	if sw.errors > 0 {
		line += fmt.Sprintf("  %d ERRORS", sw.errors)
	}
	return line
}

func si(v float64) string {
	switch {
	case v >= 1e9:
		return strconv.FormatFloat(v/1e9, 'f', 2, 64) + "G"
	case v >= 1e6:
		return strconv.FormatFloat(v/1e6, 'f', 2, 64) + "M"
	case v >= 1e3:
		return strconv.FormatFloat(v/1e3, 'f', 2, 64) + "k"
	}
	return strconv.FormatFloat(v, 'f', 0, 64)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
