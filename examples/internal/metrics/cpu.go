//go:build linux

// Package metrics measures what a run cost: how busy the processors were and
// what the NIC's own counters say.
//
// It is here because the interesting question about a packet path is not how
// fast it can go but what it spends to get there. A rate on its own says
// nothing without the processors that produced it, and the driver's own
// counters say nothing about what actually reached the wire.
package metrics

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// CPUTime is how a processor spent an interval, in seconds.
type CPUTime struct {
	User    float64
	System  float64
	SoftIRQ float64 // where a kernel network stack does its work
	IRQ     float64
	Idle    float64
	Steal   float64
	Total   float64
}

// Busy is everything that was not idle.
func (c CPUTime) Busy() float64 { return c.Total - c.Idle }

// Utilisation is the fraction of the interval that was not idle.
func (c CPUTime) Utilisation() float64 {
	if c.Total == 0 {
		return 0
	}
	return c.Busy() / c.Total
}

func (c CPUTime) sub(o CPUTime) CPUTime {
	return CPUTime{
		User: c.User - o.User, System: c.System - o.System,
		SoftIRQ: c.SoftIRQ - o.SoftIRQ, IRQ: c.IRQ - o.IRQ,
		Idle: c.Idle - o.Idle, Steal: c.Steal - o.Steal,
		Total: c.Total - o.Total,
	}
}

// CPUSample is one reading of the processors a run is using and of the process
// itself.
type CPUSample struct {
	When time.Time

	// PerCPU is indexed the same way as the CPU list the sampler was given.
	PerCPU []CPUTime

	// All is the sum over those processors.
	All CPUTime

	// Process is how much processor time this process itself had used, in
	// seconds, across all its threads.
	Process float64
}

// CPUSampler reads processor time for a fixed set of processors.
//
// Which processors matters. A packet path that bypasses the kernel spends its
// time in this process, and one that goes through the kernel spends much of it
// in soft interrupt handlers that belong to no process at all. Counting only
// the process would flatter the second; counting whole processors counts both.
type CPUSampler struct {
	cpus     []int
	tickSecs float64
}

// NewCPUSampler watches the given processors. An empty list watches all of them.
func NewCPUSampler(cpus []int) *CPUSampler {
	// The kernel reports these in clock ticks, which is a hundred a second on
	// every Linux configuration in ordinary use.
	return &CPUSampler{cpus: cpus, tickSecs: 1.0 / 100.0}
}

// Sample reads the counters now.
func (s *CPUSampler) Sample() (CPUSample, error) {
	all, err := readProcStat()
	if err != nil {
		return CPUSample{}, err
	}

	out := CPUSample{When: time.Now()}
	cpus := s.cpus
	if len(cpus) == 0 {
		cpus = make([]int, 0, len(all))
		for i := range all {
			cpus = append(cpus, i)
		}
	}
	for _, c := range cpus {
		if c < 0 || c >= len(all) {
			return CPUSample{}, fmt.Errorf("metrics: no such processor %d", c)
		}
		t := all[c]
		out.PerCPU = append(out.PerCPU, t)
		out.All = addCPU(out.All, t)
	}

	if p, err := readSelfCPU(s.tickSecs); err == nil {
		out.Process = p
	}
	return out, nil
}

// Delta is what happened between two samples.
func Delta(a, b CPUSample) CPUSample {
	out := CPUSample{
		When:    b.When,
		All:     b.All.sub(a.All),
		Process: b.Process - a.Process,
	}
	for i := range b.PerCPU {
		if i < len(a.PerCPU) {
			out.PerCPU = append(out.PerCPU, b.PerCPU[i].sub(a.PerCPU[i]))
		}
	}
	return out
}

// Cores is how many whole processors' worth of time was used: 2.0 means two
// processors were busy for the whole interval.
func (c CPUSample) Cores() float64 {
	if len(c.PerCPU) == 0 {
		return 0
	}
	// Total across the watched processors is the interval times their count,
	// so busy divided by interval is the number of cores' worth.
	interval := c.All.Total / float64(len(c.PerCPU))
	if interval == 0 {
		return 0
	}
	return c.All.Busy() / interval
}

// CyclesPerPacket estimates what each packet cost, given the processor speed in
// hertz. It is an estimate: without a performance counter to read, the only
// thing available is time multiplied by a nominal frequency, which ignores
// turbo and frequency scaling.
func (c CPUSample) CyclesPerPacket(packets uint64, hz float64) float64 {
	if packets == 0 {
		return 0
	}
	return c.All.Busy() * hz / float64(packets)
}

func addCPU(a, b CPUTime) CPUTime {
	return CPUTime{
		User: a.User + b.User, System: a.System + b.System,
		SoftIRQ: a.SoftIRQ + b.SoftIRQ, IRQ: a.IRQ + b.IRQ,
		Idle: a.Idle + b.Idle, Steal: a.Steal + b.Steal,
		Total: a.Total + b.Total,
	}
}

// readProcStat returns per-processor times, indexed by processor number.
func readProcStat() ([]CPUTime, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []CPUTime
	const tick = 1.0 / 100.0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 9 || !strings.HasPrefix(fields[0], "cpu") || fields[0] == "cpu" {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(fields[0], "cpu"))
		if err != nil {
			continue
		}
		v := make([]float64, 0, 10)
		for _, f := range fields[1:] {
			x, err := strconv.ParseFloat(f, 64)
			if err != nil {
				x = 0
			}
			v = append(v, x*tick)
		}
		for len(v) < 9 {
			v = append(v, 0)
		}
		t := CPUTime{
			User:    v[0] + v[1], // user and nice
			System:  v[2],
			Idle:    v[3] + v[4], // idle and waiting for io
			IRQ:     v[5],
			SoftIRQ: v[6],
			Steal:   v[7],
		}
		t.Total = t.User + t.System + t.Idle + t.IRQ + t.SoftIRQ + t.Steal
		for len(out) <= n {
			out = append(out, CPUTime{})
		}
		out[n] = t
	}
	return out, sc.Err()
}

// readSelfCPU returns the processor time this process has used, in seconds.
func readSelfCPU(tick float64) (float64, error) {
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, err
	}
	// The command name may contain spaces and is wrapped in brackets, so
	// everything is counted from the closing bracket.
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return 0, fmt.Errorf("metrics: cannot parse /proc/self/stat")
	}
	fields := strings.Fields(string(b[i+1:]))
	// After the command and state come the fields; user and system time are
	// the twelfth and thirteenth of them.
	if len(fields) < 13 {
		return 0, fmt.Errorf("metrics: /proc/self/stat is too short")
	}
	utime, _ := strconv.ParseFloat(fields[11], 64)
	stime, _ := strconv.ParseFloat(fields[12], 64)
	return (utime + stime) * tick, nil
}

// CPUHz is the processor's nominal speed in hertz, for turning busy time into
// an estimate of cycles per packet.
//
// It is nominal on purpose. What is wanted is a figure comparable between two
// runs on the same machine, and a measured frequency would move with turbo and
// with how busy the machine is, which is exactly what is being compared.
func CPUHz() float64 {
	// The maximum the driver will ask for, in kilohertz, is the most stable
	// number available without a performance counter.
	if b, err := readFileTrimmed("/sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_max_freq"); err == nil {
		if khz, err := strconv.ParseFloat(b, 64); err == nil && khz > 0 {
			return khz * 1000
		}
	}
	// Failing that, what the processor calls itself.
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			name, value, ok := strings.Cut(line, ":")
			if !ok || strings.TrimSpace(name) != "model name" {
				continue
			}
			// A name like "AMD EPYC 9275F 24-Core Processor" says nothing, but
			// an Intel one usually ends in "@ 3.00GHz".
			if _, ghz, ok := strings.Cut(value, "@"); ok {
				ghz = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(ghz), "GHz"))
				if v, err := strconv.ParseFloat(ghz, 64); err == nil && v > 0 {
					return v * 1e9
				}
			}
			break
		}
	}
	return 0
}

func readFileTrimmed(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
