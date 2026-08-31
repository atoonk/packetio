//go:build amd64 || arm64

package arch

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unsafe"
)

// The values these functions publish are what the NIC reads, so the first thing
// to check is that the right bytes land in the right place. Here they are
// pointed at ordinary memory; on a device they point at a doorbell record and a
// mapped register.
func TestPublishDoorbellStoresBothWords(t *testing.T) {
	var dbrec [2]uint32
	var uar [2]uint64

	PublishDoorbell(&dbrec[1], 0x0000cafe, unsafe.Pointer(&uar[0]), 0x1122334455667788)

	if dbrec[0] != 0 {
		t.Errorf("the receive half of the doorbell record was written: %#x", dbrec[0])
	}
	if dbrec[1] != 0x0000cafe {
		t.Errorf("doorbell record = %#x, want 0xcafe", dbrec[1])
	}
	if uar[0] != 0x1122334455667788 {
		t.Errorf("register = %#x, want 0x1122334455667788", uar[0])
	}
	if uar[1] != 0 {
		t.Errorf("wrote past the eight bytes of the register: %#x", uar[1])
	}
}

func TestPublishDbrec(t *testing.T) {
	var dbrec [2]uint32
	PublishDbrec(&dbrec[0], 0xdeadbeef)
	if dbrec[0] != 0xdeadbeef || dbrec[1] != 0 {
		t.Fatalf("doorbell record = %#x %#x, want 0xdeadbeef and 0", dbrec[0], dbrec[1])
	}
}

func TestReleaseCQ(t *testing.T) {
	var dbrec [2]uint32
	ReleaseCQ(&dbrec[0], 0x00ffffff)
	if dbrec[0] != 0x00ffffff || dbrec[1] != 0 {
		t.Fatalf("doorbell record = %#x %#x, want 0xffffff and 0", dbrec[0], dbrec[1])
	}
}

// The completion header is big-endian in memory and wanted host-endian in a
// register, so the load byte-swaps. Getting this backwards would put the
// opcode where the work queue counter belongs and still look plausible, which
// is why it is checked against bytes written out by hand.
func TestLoadCQEHeader(t *testing.T) {
	buf := make([]byte, 8)
	// wqe_counter 0xbeef, signature 0x00, op_own 0x21: opcode 2, owner 1.
	buf[0], buf[1], buf[2], buf[3] = 0xbe, 0xef, 0x00, 0x21

	got := LoadCQEHeader(unsafe.Pointer(&buf[0]))
	if want := uint32(0xbeef0021); got != want {
		t.Fatalf("LoadCQEHeader = %#08x, want %#08x", got, want)
	}
	if counter := uint16(got >> 16); counter != 0xbeef {
		t.Errorf("work queue counter = %#x, want 0xbeef", counter)
	}
	if opOwn := byte(got); opOwn != 0x21 {
		t.Errorf("opcode-and-owner byte = %#x, want 0x21", opOwn)
	}
}

// Publishing is on the packet path.
func TestPublishDoesNotAllocate(t *testing.T) {
	var dbrec [2]uint32
	var uar uint64
	buf := make([]byte, 8)

	allocs := testing.AllocsPerRun(1000, func() {
		PublishDoorbell(&dbrec[1], 1, unsafe.Pointer(&uar), 2)
		PublishDbrec(&dbrec[0], 3)
		ReleaseCQ(&dbrec[0], 4)
		_ = LoadCQEHeader(unsafe.Pointer(&buf[0]))
	})
	if allocs != 0 {
		t.Fatalf("publishing allocated %v times per run, want 0", allocs)
	}
}

// The fences are the whole reason these functions are assembly, and one that
// quietly went missing would leave a race with the NIC that no test against
// ordinary memory could catch: the values would still be correct, just
// occasionally visible in the wrong order. So look at the instructions.
func TestFencesArePresent(t *testing.T) {
	if testing.Short() {
		t.Skip("building a binary to disassemble takes a few seconds")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go tool to build and disassemble with")
	}
	// The running test binary is linked without a symbol table, so build one
	// that has it. The test runs in its own package directory.
	self := filepath.Join(t.TempDir(), "arch.test")
	if out, err := exec.Command("go", "test", "-c", "-o", self, ".").CombinedOutput(); err != nil {
		t.Skipf("cannot build a binary to disassemble: %v\n%s", err, out)
	}

	type want struct {
		fn    string
		count map[string]int
	}
	var cases []want
	switch runtime.GOARCH {
	case "amd64":
		cases = []want{
			// Order the producer index ahead of the register write, then push
			// the register write out of the write-combining buffer.
			{"PublishDoorbell", map[string]int{"SFENCE": 2}},
			// Keep later reads of the completion behind the ownership check.
			// Removing this on the x86-64 TSO argument was measured and bought
			// nothing, so it follows rdma-core rather than DPDK here.
			{"LoadCQEHeader", map[string]int{"LFENCE": 1}},
			// Nothing needed: x86-64 stores reach the device in order.
			{"PublishDbrec", map[string]int{"SFENCE": 0, "MFENCE": 0}},
		}
	case "arm64":
		cases = []want{
			{"PublishDoorbell", map[string]int{"DMB": 1, "DSB": 2}},
			{"LoadCQEHeader", map[string]int{"DMB": 1}},
			{"PublishDbrec", map[string]int{"DMB": 1}},
			{"ReleaseCQ", map[string]int{"DMB": 1}},
		}
	default:
		t.Skipf("no expectations for %s", runtime.GOARCH)
	}

	for _, c := range cases {
		// Assembly functions are called through an ABI wrapper, so the symbol
		// carries an .abi0 suffix.
		out, err := exec.Command("go", "tool", "objdump",
			"-s", `arch\.`+c.fn+`(\.abi0)?$`, self).CombinedOutput()
		if err != nil {
			t.Fatalf("disassembling %s: %v\n%s", c.fn, err, out)
		}
		text := string(out)
		if !strings.Contains(text, c.fn) {
			t.Fatalf("%s: nothing disassembled:\n%s", c.fn, text)
		}
		for ins, n := range c.count {
			if got := strings.Count(text, "\t"+ins); got != n {
				t.Errorf("%s has %d %s, want %d:\n%s", c.fn, got, ins, n, text)
			}
		}
	}
}
