//go:build linux && cgo && mlx5 && (amd64 || arm64)

package main

import (
	"context"
	"fmt"
	"math/bits"
	"sync/atomic"
	"time"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/examples/internal/affinity"
	"github.com/atoonk/packetio/examples/internal/forward"
	"github.com/atoonk/packetio/mlx5"
)

// counters is what one worker has done. It is padded to a cache line so that
// workers counting side by side do not slow each other down, and updated once
// per batch, not per packet.
type counters struct {
	rx    atomic.Uint64
	fwd   atomic.Uint64
	drops [forward.NumErrors]atomic.Uint64
	_     [64]byte
}

// sample keeps the first packet a worker saw, before and after the rewrite,
// so a run can be checked by eye.
type sample struct {
	before, after []byte
}

// forwarder is what every worker shares, and none of them writes to.
type forwarder struct {
	fib  *forward.V4Fib
	adjs []forward.Adjacency
}

// worker drives one receive queue and the transmit queues that carry its
// frames. Everything a packet needs is done here, in one pass, on one core:
// take what arrived, find the IPv4 header, look the destination up, rewrite
// the packet in the frame it arrived in, hand that same frame to the NIC to
// send, and when the NIC is done with it give it back to the receive queue.
//
// Nothing is copied and nothing is allocated. The frames are the receive
// queue's, so they go home through Reclaim and Recycle rather than Complete,
// which would hand them to the transmit pool where the receive queue could
// never find them again.
func worker(ctx context.Context, dev *mlx5.Device, index, cpu int, c config, fw *forwarder, cnt *counters, smp *sample) error {
	if cpu >= 0 {
		if err := affinity.Pin(cpu); err != nil {
			return err
		}
	}
	rx := dev.Rx(index)
	txs := make([]*mlx5.TxQueue, 0, c.txPerWorker)
	for i := index * c.txPerWorker; i < (index+1)*c.txPerWorker; i++ {
		txs = append(txs, dev.Tx(i))
	}
	// The region's bytes and frame size, taken once. Going through the
	// packetio.Region interface for every packet costs a call the compiler
	// cannot inline and a divide to find the end of the frame; the packet loop
	// does the same arithmetic here with a shift.
	region := rx.Region()
	mem := region.Bytes()
	frameShift := uint(bits.TrailingZeros(uint(region.FrameSize())))
	poolFrames := rx.NumFreeFrames()

	out := make([]packetio.Desc, 0, c.batch)
	drop := make([]packetio.Desc, 0, c.batch)
	back := make([]packetio.Desc, 0, c.txDepth)

	// Nothing arrives until the NIC has been given somewhere to put it.
	rx.Fill(rx.NumFreeFillSlots())

	next := 0
	for ctx.Err() == nil {
		descs := rx.Receive(c.batch)
		if len(descs) == 0 {
			back = reclaim(txs, rx, back)
			rx.Fill(rx.NumFreeFillSlots())
			continue
		}

		var dropped [forward.NumErrors]uint64
		out, drop = out[:0], drop[:0]
		for _, d := range descs {
			end := (d.Addr>>frameShift + 1) << frameShift
			buf := mem[d.Addr:end:end]
			l3, dst, code := forward.Parse(buf[:d.Len], d.Options&packetio.OptL3ChecksumOK != 0)
			if code != forward.ErrNone {
				dropped[code]++
				drop = append(drop, d)
				continue
			}
			adj := fw.fib.Lookup(dst)
			if adj == 0 {
				dropped[forward.ErrNoRoute]++
				drop = append(drop, d)
				continue
			}
			if smp != nil && smp.before == nil {
				smp.before = append([]byte(nil), buf[:d.Len]...)
			}
			n := forward.Rewrite(buf, l3, &fw.adjs[adj])
			if n == 0 {
				dropped[forward.ErrNoRoute]++ // does not fit the egress frame
				drop = append(drop, d)
				continue
			}
			d.Len = uint32(n)
			if smp != nil && smp.after == nil {
				smp.after = append([]byte(nil), buf[:d.Len]...)
			}
			out = append(out, d)
		}

		sent := 0
		if len(out) > 0 {
			// Round-robin over this worker's transmit queues: a send queue
			// tops out well below what one core can forward, so a worker
			// above that rate needs more than one.
			tx := txs[next]
			if next++; next == len(txs) {
				next = 0
			}
			sent = tx.Transmit(out)
			if sent < len(out) {
				// A full ring is a drop with its own count, not something to
				// wait for: a worker that spins on it looks busy for nothing.
				dropped[forward.ErrTxFull] += uint64(len(out) - sent)
				drop = append(drop, out[sent:]...)
			}
		}
		rx.Recycle(drop)
		back = reclaim(txs, rx, back)
		rx.Fill(rx.NumFreeFillSlots())

		cnt.rx.Add(uint64(len(descs)))
		cnt.fwd.Add(uint64(sent))
		for i, n := range dropped {
			if n != 0 {
				cnt.drops[i].Add(n)
			}
		}
	}

	// Let the NIC finish with what it holds before anyone counts the frames.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		back = reclaim(txs, rx, back)
		busy := false
		for _, tx := range txs {
			if tx.NumInFlight() != 0 {
				busy = true
			}
		}
		if !busy {
			break
		}
	}
	for _, tx := range txs {
		if err := tx.Err(); err != nil {
			return err
		}
	}

	// Every frame this worker was given is now either free or posted to the
	// receive queue. Anything else is a frame lost or a frame counted twice,
	// which is the one bug in a forwarder that no counter on the wire shows.
	inFlight := 0
	for _, tx := range txs {
		inFlight += tx.NumInFlight()
	}
	free, posted := rx.NumFreeFrames(), rx.NumOutstanding()
	if free+posted+inFlight != poolFrames {
		return fmt.Errorf("worker %d: %d frames, but %d free + %d posted + %d in flight = %d",
			index, poolFrames, free, posted, inFlight, free+posted+inFlight)
	}
	fmt.Printf("worker %d: every one of its %d frames accounted for (%d free, %d posted to the receive queue)\n",
		index, poolFrames, free, posted)
	return nil
}

// reclaim takes the frames the NIC has finished sending and gives them back to
// the receive queue they came from.
func reclaim(txs []*mlx5.TxQueue, rx *mlx5.RxQueue, back []packetio.Desc) []packetio.Desc {
	for _, tx := range txs {
		back = tx.Reclaim(tx.NumInFlight(), back[:0])
		if len(back) > 0 {
			rx.Recycle(back)
		}
	}
	return back
}
