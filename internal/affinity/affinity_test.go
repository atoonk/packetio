//go:build linux

package affinity

import (
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestParseCPUList(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []int
	}{
		{"0-3", []int{0, 1, 2, 3}},
		{"0,2,4", []int{0, 2, 4}},
		{"3-5,27-29", []int{3, 4, 5, 27, 28, 29}},
		{" 7 \n", []int{7}},
		{"", nil},
		{"nonsense", nil},
		{"5-3", nil},                          // backwards
		{"0-100000", nil},                     // absurd, must not allocate
		{"0-1,9999999999999999", []int{0, 1}}, // the impossible entry dropped, the good part kept
	} {
		got := parseCPUList(c.in)
		if len(got) != len(c.want) {
			t.Errorf("parseCPUList(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parseCPUList(%q) = %v, want %v", c.in, got, c.want)
				break
			}
		}
	}
	// The good half of a line with a bad range still comes through.
	if got := parseCPUList("0-1,5-3"); len(got) != 2 {
		t.Errorf("parseCPUList(\"0-1,5-3\") = %v, want the two valid entries", got)
	}
}

// An explicit list is used as given, and nonsense in it is refused at Open
// rather than on the first packet.
func TestExplicitPlacementIsChecked(t *testing.T) {
	if _, err := New("lo", []int{-1}); err == nil {
		t.Error("a negative processor was accepted")
	}
	if _, err := New("lo", []int{maxCPU}); err == nil {
		t.Errorf("processor %d was accepted", maxCPU)
	}
	// A processor this process may use must be accepted even when the affinity
	// mask makes runtime.NumCPU smaller than the processor's own number.
	var set unix.CPUSet
	if unix.SchedGetaffinity(0, &set) == nil {
		for c := runtime.NumCPU(); c < maxCPU; c++ {
			if !set.IsSet(c) {
				continue
			}
			if _, err := New("lo", []int{c}); err != nil {
				t.Errorf("usable processor %d rejected: %v", c, err)
			}
			break
		}
	}
	p, err := New("lo", []int{0})
	if err != nil {
		t.Fatalf("processor 0: %v", err)
	}
	if len(p.cpus) != 1 || p.cpus[0] != 0 {
		t.Errorf("explicit list became %v", p.cpus)
	}
}

// The chosen order must name each physical core once, and must group the cores
// of a cache complex together rather than interleaving them: that grouping is
// the whole point.
func TestPackedOrderGroupsComplexes(t *testing.T) {
	p, err := New("lo", nil)
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	if p == nil {
		t.Skip("no usable topology here")
	}

	seen := map[int]bool{}
	for _, c := range p.cpus {
		if seen[c] {
			t.Errorf("processor %d appears twice in %v", c, p.cpus)
		}
		seen[c] = true
	}

	// Walk the list and check a complex is never returned to once left.
	complexOf := func(cpu int) int {
		shared := sharedCacheCPUs(cpu)
		if len(shared) == 0 {
			return cpu
		}
		return shared[0]
	}
	var order []int
	for _, c := range p.cpus {
		k := complexOf(c)
		if len(order) == 0 || order[len(order)-1] != k {
			order = append(order, k)
		}
	}
	visited := map[int]bool{}
	for _, k := range order {
		if visited[k] {
			t.Errorf("cache complex %d is left and returned to: %v", k, p.cpus)
		}
		visited[k] = true
	}
}

// Placing must be idempotent per goroutine: a worker driving several queues
// asks once per queue and must not be moved by the later ones.
func TestPinIsIdempotentForOneWorker(t *testing.T) {
	p, err := New("lo", nil)
	if err != nil || p == nil {
		t.Skip("no usable topology here")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// This goroutine is locked to its thread by pin and never unlocked,
		// so it must not be returned to the runtime's pool: let it end.
		first, err := p.Pin()
		if err != nil {
			t.Errorf("first pin: %v", err)
			return
		}
		for i := 0; i < 4; i++ {
			again, err := p.Pin()
			if err != nil {
				t.Errorf("pin %d: %v", i, err)
				return
			}
			if again != first {
				t.Errorf("the same worker was moved from %d to %d", first, again)
			}
		}
		var set unix.CPUSet
		if err := unix.SchedGetaffinity(0, &set); err == nil {
			if set.Count() != 1 || !set.IsSet(first) {
				t.Errorf("thread runs on %d processors, want only %d", set.Count(), first)
			}
		}
	}()
	<-done
}

// Two workers must get different processors.
func TestPinGivesEachWorkerItsOwn(t *testing.T) {
	p, err := New("lo", nil)
	if err != nil || p == nil || len(p.cpus) < 2 {
		t.Skip("not enough processors here")
	}
	got := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			cpu, err := p.Pin()
			if err != nil {
				t.Errorf("pin: %v", err)
			}
			got <- cpu
		}()
	}
	a, b := <-got, <-got
	if a == b {
		t.Errorf("two workers were both placed on processor %d", a)
	}
}

// Running out of processors is an error the caller can see, not a silent
// stampede onto one core.
func TestPinRunsOut(t *testing.T) {
	p, err := New("lo", []int{0})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := p.Pin(); err != nil {
			t.Errorf("first worker: %v", err)
		}
	}()
	<-done
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		if _, err := p.Pin(); err == nil {
			t.Error("a second worker was placed although only one processor was given")
		}
	}()
	<-done2
}

// A nil plan is what WithoutAffinity leaves behind and must be harmless.
func TestNoPlacementIsHarmless(t *testing.T) {
	var p *Placement
	if cpu, err := p.Pin(); cpu != -1 || err != nil {
		t.Errorf("pin on no plan = %d, %v", cpu, err)
	}
	if p.String() == "" {
		t.Error("no plan should still describe itself")
	}
}

func TestSafeIfname(t *testing.T) {
	// The name is joined into a /sys path. filepath.Join cleans but does not
	// confine, so anything that could escape has to be refused before it.
	for _, bad := range []string{"", "..", ".", "../..", "eth0/../../etc", "a/b", "eth\x000"} {
		if safeIfname(bad) {
			t.Errorf("accepted %q as an interface name", bad)
		}
	}
	for _, ok := range []string{"eth0", "eno2.2053", "enp3s0f0np0", "veth-abc"} {
		if !safeIfname(ok) {
			t.Errorf("refused %q, which is a real interface name", ok)
		}
	}
	long := make([]byte, 200)
	for i := range long {
		long[i] = 'a'
	}
	if safeIfname(string(long)) {
		t.Error("accepted a 200-character interface name")
	}
}

func TestParseCPUListIsBounded(t *testing.T) {
	// Each range was already bounded; the number of parts was not, so a file
	// of a million commas could ask for a million-entry slice.
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		b.WriteString("0,")
	}
	if got := len(parseCPUList(b.String())); got > maxCPU {
		t.Errorf("parseCPUList returned %d entries, more than the %d cap", got, maxCPU)
	}
	// And the range bound still holds.
	if got := parseCPUList("0-99999999"); len(got) != 0 {
		t.Errorf("accepted an out-of-range span: %d entries", len(got))
	}
}
