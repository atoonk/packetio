//go:build linux

package netstack

import (
	"time"

	"github.com/atoonk/packetio/netstack/gvisor/pkg/buffer"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/stack"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/tcpip/transport/tcp"
)

// The patched gVisor in ./gvisor (go-afxdp's examples/netstack/patches,
// applied to a pinned upstream) has knobs upstream does not. These are the
// settings that fork measured best on a 10G and a 100G LAN. Set once, before
// any stack is built.
func applyFork() {
	// Loss detection on a LAN whose ordinary reordering upstream mistook for
	// loss: a floor on the RACK reorder window, no zeroing it at three SACKed
	// segments, a floor under the tail loss probe timeout, no probe while
	// only the peer's window holds the sender back, and a zero-window probe
	// that is not counted as a timeout.
	tcp.MinReorderWindow = 5 * time.Millisecond
	tcp.ReorderWindowDupthreshOverride = true
	tcp.MinPTO = 10 * time.Millisecond
	tcp.TLPSkipWindowLimited = true
	tcp.SeparateZeroWindowProbe = true
	tcp.PreciseRTT = true
	// A reply carries the ACK instead of a bare ACK preceding it.
	tcp.DelayedACKTimeout = time.Millisecond
	// An established connection's segments are handled on the goroutine
	// that delivered them (InlineProcessing); what cannot be -- handshakes,
	// closes -- is set aside and run by that goroutine after the batch
	// (BatchProcessing; see Endpoint.afterBatch), with one processor
	// goroutine per stack as the fallback. TxHashFromRx copies the queue
	// index rx.go puts in pkt.Hash into the connection, so its replies
	// leave on the queue its segments arrive on.
	tcp.InlineProcessing = true
	tcp.BatchProcessing = true
	tcp.TxHashFromRx = true
	tcp.ProcessorCount = 1
	// Buffer chunks are not zeroed before reuse (every user writes before it
	// reads), a delivered packet is shared with its segment rather than
	// cloned, and the sentry's save-time endpoint registry is off.
	buffer.ZeroOnReuse = false
	tcp.ClonePacketForSegment = false
	tcpip.TrackDanglingEndpoints = false
}

// drainDeferred runs the TCP work set aside for after a batch.
func drainDeferred(s *stack.Stack) { tcp.DrainDeferred(s) }
