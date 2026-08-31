//go:build linux

package affinity

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []int
	}{
		{"", nil},
		{"  ", nil},
		{"4", []int{4}},
		{"4,6", []int{4, 6}},
		{"8-11", []int{8, 9, 10, 11}},
		{"4, 6, 8-10", []int{4, 6, 8, 9, 10}},
		{"0-0", []int{0}},
	} {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("Parse(%q) = %v, want %v", c.in, got, c.want)
		}
	}

	for _, bad := range []string{"a", "4-", "-4", "6-4", "4,,6", "1-x"} {
		if got, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) = %v, want an error", bad, got)
		}
	}
}

// Pinning to a CPU that exists must work, and to one that does not must fail
// rather than silently leave the goroutine wherever it was.
func TestPin(t *testing.T) {
	if err := Pin(0); err != nil {
		t.Fatalf("pinning to cpu 0: %v", err)
	}
	if err := Pin(1 << 20); err == nil {
		t.Error("pinning to a cpu that does not exist was accepted")
	}
	if err := Pin(-1); err != nil {
		t.Errorf("a negative cpu means do not pin, got %v", err)
	}
}
