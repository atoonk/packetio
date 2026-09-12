//go:build linux

// Package affinity decides where a backend's workers run.
//
// It is shared rather than per-backend because the two things that decide the
// answer are not specific to one: the memory node the card is attached to, and
// the cache complex a worker's doorbell writes travel from. The measurement
// below was taken on mlx5 Direct Verbs and applies unchanged to any backend
// whose workers ring doorbells through the same write-combining pages, which
// includes DPDK's mlx5 poll-mode driver.
//
// A backend names itself in any error it passes on; nothing here does, so the
// same message reads correctly whichever backend produced it.
package affinity

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// Locking the goroutine to its thread is what makes a processor choice stick:
// without it the scheduler may move the goroutine to another thread, and the
// affinity was set on the thread.
func runtimeLockOSThread() { runtime.LockOSThread() }

// runtimeUnlockOSThread is the other half, for the paths that lock a thread
// and then find they have nothing to place it on. Leaving a goroutine locked
// to a thread it was never placed on costs the caller a thread for nothing.
func runtimeUnlockOSThread() { runtime.UnlockOSThread() }

func numCPU() int { return runtime.NumCPU() }

// maxCPU bounds what a processor list may name. unix.CPUSet holds 1024 bits,
// and nothing this code does is useful beyond that.
const maxCPU = 1024

// Where a worker runs decides how fast it goes, and the reason is not the one
// an AF_XDP driver has.
//
// A polling backend takes no interrupt for its queues, so there is nothing to
// sit beside. What its workers do share is the device register a doorbell is
// written to. The provider hands out those registers from a small
// number of write-combining pages, so several send queues ring their doorbells
// through the same page -- and a page written from two last-level caches costs
// far more than one written from a single cache.
//
// Measured on a ConnectX-6 Dx in an EPYC 9275F, two workers of four queues
// each, 64-byte frames:
//
//	both workers in one cache complex   92.7 Mpps
//	one worker in each of two           75.6 Mpps
//
// The same run with the doorbell carrying sixteen times as many packets closes
// the gap to 3%, which is what says it is the doorbell and not the packets.
//
// So the rule here is the opposite of go-afxdp's "put the worker beside its
// interrupt": put the workers together, filling one cache complex before
// starting the next, and prefer the complexes on the memory node the card is
// attached to. A caller who knows better passes its own list to New; a caller
// who wants nothing done never calls New at all.

// placement is the order in which processors are handed to workers, and the
// record of which ones have been handed out.
type Placement struct {
	cpus []int // physical cores, one cache complex at a time, best first
	note string

	mu     sync.Mutex
	next   int
	byTID  map[int]int // thread -> the processor it was given
	failed int
}

// New works out where a device's workers should run. want, when not empty, is
// the caller's own list and is used in the order given.
//
// It returns nil, nil when the machine's topology yields nothing usable, which
// means "leave the workers where the scheduler puts them" rather than an error.
func New(ifname string, want []int) (*Placement, error) {
	allowed, haveAllowed := allowedCPUs()

	if len(want) > 0 {
		for _, c := range want {
			// Bounds only: runtime.NumCPU reports how many processors this
			// process may use, not how many the machine has, so under taskset
			// it would reject the very processors it was confined to. What the
			// caller may actually use is the affinity mask, checked next.
			if c < 0 || c >= maxCPU {
				return nil, fmt.Errorf("%d is not a processor number", c)
			}
			if haveAllowed && !allowed[c] {
				return nil, fmt.Errorf("processor %d is outside what this process may run on", c)
			}
		}
		return &Placement{cpus: append([]int(nil), want...), note: "as asked", byTID: map[int]int{}}, nil
	}

	cpus, note := packedCPUs(ifname, allowed, haveAllowed)
	if len(cpus) == 0 {
		return nil, nil // nothing usable; leave placement alone
	}
	return &Placement{cpus: cpus, note: note, byTID: map[int]int{}}, nil
}

// packedCPUs lists one processor per physical core, grouped so that a whole
// cache complex is used before the next is started, with the card's own memory
// node first.
func packedCPUs(ifname string, allowed map[int]bool, haveAllowed bool) ([]int, string) {
	node := deviceNUMANode(ifname)

	type complex struct {
		lead  int   // lowest cpu of the complex, for ordering
		node  int   // memory node it belongs to
		cores []int // one processor per physical core
		sibs  []int // the other SMT threads of those cores, as a fallback
	}
	var (
		complexes   []*complex
		byLead      = map[int]*complex{}
		seenCore    = map[int]bool{}
		leadComplex = map[int]int{} // physical core lead -> its complex key
		strayS      []int
	)
	for _, c := range onlineCPUs() {
		if haveAllowed && !allowed[c] {
			continue
		}
		// One processor per physical core: the first sibling wins, because a
		// second worker on the other half of a core is not a second core.
		sibs := readCPUList(fmt.Sprintf("/sys/devices/system/cpu/cpu%d/topology/thread_siblings_list", c))
		lead := c
		if len(sibs) > 0 {
			lead = sibs[0]
		}
		if lead != c {
			// The other half of a core already counted. Keep it aside: it is
			// not a second core, but it is a second place to run, and running
			// there beats not running at all.
			if cx := byLead[leadComplex[lead]]; cx != nil {
				cx.sibs = append(cx.sibs, c)
			} else {
				strayS = append(strayS, c)
			}
			continue
		}
		if seenCore[lead] {
			continue
		}
		seenCore[lead] = true

		shared := sharedCacheCPUs(c)
		key := c
		if len(shared) > 0 {
			key = shared[0]
		}
		cx := byLead[key]
		if cx == nil {
			cx = &complex{lead: key, node: cpuNUMANode(c)}
			byLead[key] = cx
			complexes = append(complexes, cx)
		}
		cx.cores = append(cx.cores, c)
		leadComplex[lead] = key
	}
	// Siblings seen before their core's complex existed.
	for _, c := range strayS {
		sibs := readCPUList(fmt.Sprintf("/sys/devices/system/cpu/cpu%d/topology/thread_siblings_list", c))
		if len(sibs) == 0 {
			continue
		}
		if cx := byLead[leadComplex[sibs[0]]]; cx != nil {
			cx.sibs = append(cx.sibs, c)
		}
	}
	if len(complexes) == 0 {
		return nil, ""
	}

	// The card's memory node first, then the rest; within that, the complex
	// with the most free cores first so a fleet fits in as few as possible.
	sort.SliceStable(complexes, func(i, j int) bool {
		a, b := complexes[i], complexes[j]
		if node >= 0 && (a.node == node) != (b.node == node) {
			return a.node == node
		}
		if len(a.cores) != len(b.cores) {
			return len(a.cores) > len(b.cores)
		}
		return a.lead < b.lead
	})

	// Physical cores first, in the measured order. Then, and only then, the
	// SMT siblings.
	//
	// Running out of physical cores used to be a hard failure: Pin returned
	// "no processor left to place a worker on" and the worker ran unplaced. On
	// anything with SMT that is easy to hit -- a 4-vCPU EC2 instance is two
	// cores, so one queue per vCPU asks for twice what the first tier holds,
	// and the measured cost was exactly half the throughput (0.20 Mpps of
	// 0.40, 2.01 cores of 4.00). A sibling is a worse place to run than a core
	// of one's own, and a far better one than nowhere.
	//
	// This is not a rule about EC2, or about virtual machines, or about any
	// architecture: it is what to do when a caller asks for more workers than
	// there are cores, which a bare-metal box hits too as soon as the queue
	// count passes its core count.
	var out []int
	for _, cx := range complexes {
		sort.Ints(cx.cores)
		out = append(out, cx.cores...)
	}
	phys := len(out)
	for _, cx := range complexes {
		sort.Ints(cx.sibs)
		out = append(out, cx.sibs...)
	}
	note := fmt.Sprintf("%d cores over %d cache complexes", phys, len(complexes))
	if len(out) > phys {
		note += fmt.Sprintf(", then %d sibling threads", len(out)-phys)
	}
	if node >= 0 {
		note += fmt.Sprintf(", card on memory node %d", node)
	}
	return out, note
}

// Pin puts the calling goroutine on a processor of its own and reports which.
//
// A goroutine that drives several queues asks once per queue; the second and
// later calls find the thread already placed and leave it where it is, which
// is what makes "one worker, four queues" work without the queues fighting
// over where the worker runs.
func (p *Placement) Pin() (int, error) {
	if p == nil {
		return -1, nil
	}
	runtimeLockOSThread()
	tid := unix.Gettid()

	p.mu.Lock()
	cpu, known := p.byTID[tid]
	if !known {
		if p.next >= len(p.cpus) {
			p.mu.Unlock()
			// Nothing was placed, so do not leave the caller pinned to a
			// thread it did not ask to be pinned to.
			runtimeUnlockOSThread()
			return -1, fmt.Errorf("no processor left to place a worker on; %d were available",
				len(p.cpus))
		}
		cpu = p.cpus[p.next]
		p.next++
		p.byTID[tid] = cpu
	}
	p.mu.Unlock()

	// The affinity call is made every time, including for a thread already in
	// the map. Linux recycles thread ids: a goroutine that locked a thread and
	// then exited takes the thread with it, and a later worker landing on the
	// same id would otherwise be told it is placed on a processor it was never
	// moved to. One cheap idempotent syscall is a better answer than a map
	// entry that may be about a thread that no longer exists.
	var set unix.CPUSet
	set.Zero()
	set.Set(cpu)
	if err := unix.SchedSetaffinity(0, &set); err != nil {
		p.mu.Lock()
		delete(p.byTID, tid)
		if !known {
			p.next-- // give the processor back
		}
		p.failed++
		p.mu.Unlock()
		runtimeUnlockOSThread()
		return -1, fmt.Errorf("placing a worker on processor %d: %w", cpu, err)
	}
	return cpu, nil
}

// String describes what was decided, for a program that wants to print it.
func (p *Placement) String() string {
	if p == nil {
		return "left to the scheduler"
	}
	return fmt.Sprintf("%s (%v)", p.note, p.cpus[:min(len(p.cpus), 8)])
}

func allowedCPUs() (map[int]bool, bool) {
	var set unix.CPUSet
	if unix.SchedGetaffinity(0, &set) != nil {
		return nil, false
	}
	out := map[int]bool{}
	for c := 0; c < len(set)*64; c++ {
		if set.IsSet(c) {
			out[c] = true
		}
	}
	return out, true
}

// sharedCacheCPUs is the processors sharing the deepest cache this one has.
func sharedCacheCPUs(cpu int) []int {
	for level := 4; level >= 0; level-- {
		list := readCPUList(fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cache/index%d/shared_cpu_list", cpu, level))
		if len(list) > 1 {
			return list
		}
	}
	return nil
}

func deviceNUMANode(ifname string) int {
	if !safeIfname(ifname) {
		return -1
	}
	b, err := readSmall(filepath.Join("/sys/class/net", ifname, "device", "numa_node"))
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return -1
	}
	return n
}

func cpuNUMANode(cpu int) int {
	matches, err := filepath.Glob(fmt.Sprintf("/sys/devices/system/cpu/cpu%d/node*", cpu))
	if err != nil {
		return -1
	}
	for _, m := range matches {
		if n, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(m), "node")); err == nil {
			return n
		}
	}
	return -1
}

func onlineCPUs() []int {
	if cpus := readCPUList("/sys/devices/system/cpu/online"); len(cpus) > 0 {
		return cpus
	}
	out := make([]int, numCPU())
	for i := range out {
		out[i] = i
	}
	return out
}

func readCPUList(path string) []int {
	b, err := readSmall(path)
	if err != nil {
		return nil
	}
	return parseCPUList(string(b))
}

// safeIfname reports whether name can be joined into a /sys path without
// escaping it. filepath.Join cleans but does not confine, so "../.." would
// read somewhere else entirely.
func safeIfname(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	return !strings.ContainsAny(name, "/\x00") && name != "." && name != ".."
}

// readSmall reads a sysfs file, refusing to allocate more than a sysfs file
// should ever be. Under a real /sys this never triggers; under one a container
// supplied it is the difference between a bounded read and an allocation the
// caller chose.
func readSmall(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 64<<10))
}

// parseCPUList reads the "0-3,8,12-15" form the kernel uses.
// parseCPUList's output is capped at maxCPU entries: each range is already
// bounded, but a file of a million comma-separated numbers is not.
func parseCPUList(s string) []int {
	var out []int
	for _, part := range strings.Split(strings.TrimSpace(s), ",") {
		if part == "" {
			continue
		}
		lo, hi, ok := strings.Cut(part, "-")
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			continue
		}
		b := a
		if ok {
			if b, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil {
				continue
			}
		}
		// A malformed or hostile entry must not be able to ask for a huge
		// slice, or to name a processor that cannot exist: no machine has
		// this many, and unix.CPUSet cannot hold them.
		if a < 0 || b < a || b >= maxCPU || b-a > maxCPU {
			continue
		}
		for c := a; c <= b && len(out) < maxCPU; c++ {
			out = append(out, c)
		}
	}
	return out
}
