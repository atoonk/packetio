//go:build linux && cgo && mlx5 && (amd64 || arm64)

package mlx5

import "testing"

func TestQueueAndFrameCeilings(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []Option
	}{
		{"999 receive queues", []Option{WithTxQueues(0), WithRxQueues(999)}},
		{"999 transmit queues", []Option{WithTxQueues(999)}},
		{"a mistyped frame count", []Option{WithTxQueues(1), WithFrames(1 << 25)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaults()
			for _, o := range tc.opts {
				o(&cfg)
			}
			err := cfg.validate("mlx5_0")
			if err == nil {
				t.Fatal("accepted")
			}
			t.Logf("%v", err)
		})
	}
}
