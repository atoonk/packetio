//go:build linux

// Package affinity pins a goroutine to a CPU.
//
// A packet loop that migrates between cores loses its caches and, on a machine
// with more than one memory node, may end up a long way from the NIC. Pinning
// is also what makes a measurement mean anything: "four workers" is only a
// number if each one has a core to itself.
package affinity

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Pin locks the calling goroutine to its operating system thread and moves that
// thread to the given CPU. It stays there until the goroutine ends.
//
// The lock is what makes the pinning stick: without it the Go scheduler is free
// to run the goroutine on another thread, which is on another CPU.
func Pin(cpu int) error {
	if cpu < 0 {
		return nil
	}
	runtime.LockOSThread()

	var set unix.CPUSet
	set.Zero()
	set.Set(cpu)
	if err := unix.SchedSetaffinity(0, &set); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("pinning to cpu %d: %w", cpu, err)
	}
	return nil
}

// Parse turns a list like "4,6,8-11" into CPU numbers.
func Parse(s string) ([]int, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var cpus []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		lo, hi, isRange := strings.Cut(part, "-")
		first, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return nil, fmt.Errorf("cpu list %q: %w", s, err)
		}
		last := first
		if isRange {
			if last, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil {
				return nil, fmt.Errorf("cpu list %q: %w", s, err)
			}
		}
		if last < first {
			return nil, fmt.Errorf("cpu list %q: %d-%d runs backwards", s, first, last)
		}
		for c := first; c <= last; c++ {
			if c < 0 {
				return nil, fmt.Errorf("cpu list %q: negative cpu %d", s, c)
			}
			cpus = append(cpus, c)
		}
	}
	return cpus, nil
}

// NUMANodeOf reports which memory node a CPU belongs to, or -1 if that cannot
// be told. It is for reporting: a run whose workers and NIC are on different
// nodes is worth knowing about before the numbers are believed.
func NUMANodeOf(cpu int) int {
	// /sys/devices/system/cpu/cpuN/nodeM is a symlink whose name carries the
	// answer, but reading the directory is simpler than resolving it.
	entries, err := readDirNames(fmt.Sprintf("/sys/devices/system/cpu/cpu%d", cpu))
	if err != nil {
		return -1
	}
	for _, name := range entries {
		if n, ok := strings.CutPrefix(name, "node"); ok {
			if v, err := strconv.Atoi(n); err == nil {
				return v
			}
		}
	}
	return -1
}

// NUMANodeOfInterface reports which memory node a network interface's card is
// attached to, or -1 if that cannot be told.
func NUMANodeOfInterface(ifname string) int {
	b, err := readFile(fmt.Sprintf("/sys/class/net/%s/device/numa_node", ifname))
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return -1
	}
	return n
}
