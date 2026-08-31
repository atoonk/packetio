//go:build linux && cgo && mlx5 && (amd64 || arm64)

package mlx5

import (
	"testing"

	"github.com/atoonk/packetio"
)

// The backend has to satisfy the API it claims to implement, and the compiler
// is the only thing that can say so without a NIC to open.
func TestImplementsThePacketioAPI(t *testing.T) {
	var (
		_ packetio.Device  = (*Device)(nil)
		_ packetio.TxQueue = (*TxQueue)(nil)
		_ packetio.RxQueue = (*RxQueue)(nil)
		_ packetio.Region  = (*region)(nil)
	)
}

// The inline header must cover the whole Ethernet header on a device that
// requires one, and a tagged frame's header is longer than an untagged one's.
// Nothing may assume 18.
func TestInlineHeaderFor(t *testing.T) {
	for _, c := range []struct {
		name             string
		required, ethLen int
		want             int
	}{
		{"a device that parses nothing is shown nothing", 0, 22, 0},
		{"untagged, on a device asking for 18", 18, 14, 18},
		{"one tag, on a device asking for 18", 18, 18, 18},
		{"two tags need more than the 18 usually inlined", 18, 22, 22},
		{"three tags", 18, 26, 26},
		{"a device asking for more than the header", 32, 18, 32},
	} {
		if got := InlineHeaderFor(c.required, c.ethLen); got != c.want {
			t.Errorf("%s: InlineHeaderFor(%d, %d) = %d, want %d", c.name, c.required, c.ethLen, got, c.want)
		}
	}

	for _, c := range []struct{ tags, want int }{{0, 14}, {1, 18}, {2, 22}} {
		if got := EthernetHeaderLen(c.tags); got != c.want {
			t.Errorf("EthernetHeaderLen(%d) = %d, want %d", c.tags, got, c.want)
		}
	}
}

// A failed Open must report its error, not take the process down. Returning
// "nil, err" clears the named result before the deferred cleanup runs, so a
// cleanup that reached for it would dereference nothing; Close is also called
// on devices that never finished opening.
func TestFailedOpenReportsRatherThanPanics(t *testing.T) {
	var d *Device
	if err := d.Close(); err != nil {
		t.Errorf("closing nothing: %v", err)
	}
	// An interface that cannot exist fails early, before any device is made.
	if _, err := Open("definitely-not-an-interface"); err == nil {
		t.Error("opening a nonexistent interface succeeded")
	}
	// One that fails after the device exists must still come back as an error.
	if _, err := Open("definitely-not-an-interface", WithAffinity(1<<30)); err == nil {
		t.Error("an impossible processor was accepted")
	}
}

func TestOptionValidation(t *testing.T) {
	for _, c := range []struct {
		name string
		opts []Option
	}{
		{"queue depth is not a power of two", []Option{WithTxDepth(1000)}},
		{"queue depth of zero", []Option{WithTxDepth(0)}},
		{"frame size is not a power of two", []Option{WithFrameSize(1500)}},
		{"frame size too small", []Option{WithFrameSize(32)}},
		{"no frames", []Option{WithFrames(0)}},
		{"fewer frames than the queues need", []Option{WithTxQueues(4), WithTxDepth(1024), WithFrames(1024)}},
		{"negative completion interval", []Option{WithCompletionEvery(-1)}},
		{"no queues at all", []Option{WithTxQueues(0)}},
		{"receive depth is not a power of two", []Option{WithRxQueues(1), WithRxDepth(700)}},
		{"a vlan id out of range", []Option{WithRxQueues(1),
			WithSteering(packetio.SteeringFilter{Match: []packetio.Match{packetio.MatchVLAN(4096)}})}},
		{"promiscuous together with a match", []Option{WithRxQueues(1),
			WithSteering(packetio.SteeringFilter{
				Promiscuous: true,
				Match:       []packetio.Match{packetio.MatchVLAN(100)},
			})}},
		{"fewer frames than transmit and receive together need",
			[]Option{WithTxQueues(2), WithRxQueues(2), WithTxDepth(1024), WithRxDepth(1024), WithFrames(2048)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := defaults()
			for _, o := range c.opts {
				o(&cfg)
			}
			if err := cfg.validate("mlx5_0"); err == nil {
				t.Error("accepted")
			}
		})
	}

	t.Run("the defaults are usable", func(t *testing.T) {
		cfg := defaults()
		if err := cfg.validate("mlx5_0"); err != nil {
			t.Errorf("the defaults do not validate: %v", err)
		}
	})

	t.Run("a receiving device validates", func(t *testing.T) {
		cfg := defaults()
		for _, o := range []Option{WithTxQueues(1), WithRxQueues(1), WithFrames(4096)} {
			o(&cfg)
		}
		if err := cfg.validate("mlx5_0"); err != nil {
			t.Errorf("a device with one queue each way does not validate: %v", err)
		}
	})

	t.Run("no steering filter is the default", func(t *testing.T) {
		cfg := defaults()
		if cfg.steering.Promiscuous || len(cfg.steering.Match) > 0 {
			t.Errorf("the default filter is %v, want the zero one", cfg.steering)
		}
	})
}

// A region is ordinary memory until a device is opened, so the frame arithmetic
// can be checked without one.
func TestRegionFrames(t *testing.T) {
	r, err := newRegion(8, 2048, false)
	if err != nil {
		t.Skipf("cannot map frame memory here: %v", err)
	}
	defer r.close()

	if r.NumFrames() != 8 || r.FrameSize() != 2048 || len(r.Bytes()) != 8*2048 {
		t.Fatalf("region of %d frames of %d, %d bytes", r.NumFrames(), r.FrameSize(), len(r.Bytes()))
	}

	d := packetio.Desc{Addr: 2048, Len: 64}
	if got := len(r.Frame(d)); got != 64 {
		t.Errorf("Frame gave %d bytes, want 64", got)
	}
	if got := len(r.Writable(d)); got != 2048 {
		t.Errorf("Writable gave %d bytes, want the rest of the frame, 2048", got)
	}
	// A descriptor pointing into the middle of a frame, as one with headroom
	// would, must not be able to write into the next frame.
	d = packetio.Desc{Addr: 2048 + 128, Len: 0}
	if got := len(r.Writable(d)); got != 2048-128 {
		t.Errorf("Writable from an offset gave %d bytes, want %d", got, 2048-128)
	}

	// Writing a frame must not disturb its neighbours.
	copy(r.Writable(packetio.Desc{Addr: 2048}), make([]byte, 2048))
	for i := range r.Writable(packetio.Desc{Addr: 2048}) {
		r.Bytes()[2048+i] = 0xab
	}
	if r.Bytes()[2047] != 0 || r.Bytes()[4096] != 0 {
		t.Error("writing one frame reached into another")
	}
}
