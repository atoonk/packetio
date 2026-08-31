//go:build linux

package metrics

import (
	"testing"
	"time"
)

func TestParseEthtool(t *testing.T) {
	out := []byte(`NIC statistics:
     rx_packets: 12
     tx_packets_phy: 1000
     tx_bytes_phy: 68000
     rx_packets_phy: 990
     rx_discards_phy: 10
     tx_errors_phy: 0
     rx_out_of_buffer: 3
     some_string_counter: n/a
`)
	c := parseEthtool(out)
	if !c.HasPhysicalCounters() {
		t.Fatal("the physical counters were not recognised")
	}
	if c.TxPackets != 1000 || c.TxBytes != 68000 {
		t.Errorf("transmitted %d packets, %d bytes", c.TxPackets, c.TxBytes)
	}
	if c.RxPackets != 990 || c.RxDiscards != 10 {
		t.Errorf("received %d packets, %d discarded", c.RxPackets, c.RxDiscards)
	}
	if c.RxOutOfBuffer != 3 {
		t.Errorf("out of buffer %d, want 3", c.RxOutOfBuffer)
	}
	if _, ok := c.Raw["some_string_counter"]; ok {
		t.Error("a counter that is not a number was parsed anyway")
	}

	// The rate of a run is the difference between two readings.
	later := parseEthtool([]byte("     tx_packets_phy: 3000\n     rx_discards_phy: 25\n"))
	d := later.Sub(c)
	if d.TxPackets != 2000 {
		t.Errorf("delta transmitted %d, want 2000", d.TxPackets)
	}
	if d.RxDiscards != 15 {
		t.Errorf("delta discarded %d, want 15", d.RxDiscards)
	}
}

// A port that reports no physical counters must say so rather than silently
// reporting zero, because zero looks like "nothing was sent".
func TestNoPhysicalCounters(t *testing.T) {
	c := parseEthtool([]byte("     rx_packets: 12\n     tx_packets: 13\n"))
	if c.HasPhysicalCounters() {
		t.Error("claimed physical counters it does not have")
	}
	if c.TxPackets != 0 {
		t.Errorf("transmitted %d, want 0: the kernel's counters are not the port's", c.TxPackets)
	}
}

// The processor sampler has to work on the machine it runs on, whatever that
// is, because every number a benchmark prints leans on it.
func TestCPUSampler(t *testing.T) {
	s := NewCPUSampler([]int{0})
	a, err := s.Sample()
	if err != nil {
		t.Fatalf("sampling: %v", err)
	}
	if len(a.PerCPU) != 1 {
		t.Fatalf("read %d processors, want 1", len(a.PerCPU))
	}
	if a.All.Total <= 0 {
		t.Fatalf("processor 0 has been up for %v seconds", a.All.Total)
	}

	// Burn a little time so the difference is not zero.
	spin := time.Now()
	for time.Since(spin) < 60*time.Millisecond {
	}

	b, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	d := Delta(a, b)
	if d.All.Total <= 0 {
		t.Errorf("no time passed between samples: %v", d.All.Total)
	}
	if d.Process <= 0 {
		t.Errorf("the process used %v seconds while spinning", d.Process)
	}
	if u := d.All.Utilisation(); u < 0 || u > 1.01 {
		t.Errorf("utilisation %v, which is not a fraction", u)
	}
	if c := d.Cores(); c < 0 || c > 1.01 {
		t.Errorf("%v cores' worth out of one processor", c)
	}
	// The interval's busy time is not asserted: this goroutine is not pinned,
	// so the processor it spun on is whichever one the scheduler chose, and
	// the kernel reports these in hundredths of a second.
}

// The arithmetic a benchmark leans on, on numbers chosen rather than measured.
func TestDerivedFigures(t *testing.T) {
	// Two processors watched for one second; one busy throughout, one idle.
	d := CPUSample{
		PerCPU: []CPUTime{
			{User: 0.9, System: 0.1, Total: 1.0},
			{Idle: 1.0, Total: 1.0},
		},
	}
	d.All = CPUTime{User: 0.9, System: 0.1, Idle: 1.0, Total: 2.0}

	if got := d.Cores(); got != 1.0 {
		t.Errorf("Cores = %v, want 1", got)
	}
	if got := d.All.Utilisation(); got != 0.5 {
		t.Errorf("utilisation of two processors, one busy = %v, want 0.5", got)
	}
	// One busy second at three gigahertz over three million packets is a
	// thousand cycles each.
	if got := d.CyclesPerPacket(3_000_000, 3e9); got != 1000 {
		t.Errorf("CyclesPerPacket = %v, want 1000", got)
	}
	if got := d.CyclesPerPacket(0, 3e9); got != 0 {
		t.Errorf("CyclesPerPacket with no packets = %v, want 0", got)
	}
	if got := (CPUTime{}).Utilisation(); got != 0 {
		t.Errorf("utilisation of nothing = %v, want 0", got)
	}
	if got := (CPUSample{}).Cores(); got != 0 {
		t.Errorf("cores of nothing = %v, want 0", got)
	}
}

func TestSamplerRejectsAProcessorThatDoesNotExist(t *testing.T) {
	if _, err := NewCPUSampler([]int{1 << 20}).Sample(); err == nil {
		t.Error("sampled a processor that does not exist")
	}
}
