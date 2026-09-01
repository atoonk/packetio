//go:build linux

// Command timestamps shows when packets actually arrived, using the time the
// device recorded for each one rather than the time this program got round to
// looking.
//
// That difference is the whole point. Reading a clock in the receive loop
// measures the receive loop: if the loop is busy for a millisecond, every
// packet it then collects looks like it arrived at once. A timestamp taken by
// the NIC (or, on AF_PACKET, by the kernel as it fills the ring) was taken
// before any of this code ran, so the gaps between them are what happened on
// the wire.
//
//	sudo go run ./examples/timestamps -i eth0
//
// It uses the AF_PACKET backend, so it needs no particular hardware. On a
// ConnectX the mlx5 backend reports the card's own 4 ns clock instead; the
// loop below does not change, only the Open call.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/afpacket"
)

func main() {
	iface := flag.String("i", "", "interface to listen on, for example eth0")
	dur := flag.Duration("d", 10*time.Second, "how long to listen")
	flag.Parse()
	if *iface == "" {
		flag.Usage()
		os.Exit(2)
	}

	d, err := afpacket.Open(*iface, afpacket.WithTxQueues(0), afpacket.WithRxQueues(1),
		afpacket.WithPromiscuous())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer d.Close()

	// Whether this device stamps at all is a capability, and the queue only
	// offers the extra method when it does. Both are worth checking: the
	// assertion says the backend understands timestamps, the capability says
	// this device really produces them.
	if !d.Capabilities().RxTimestamps {
		fmt.Fprintf(os.Stderr, "%s does not timestamp received packets\n", *iface)
		os.Exit(1)
	}
	rx, ok := d.RxQueue(0).(packetio.TimestampReceiver)
	if !ok {
		fmt.Fprintln(os.Stderr, "this queue cannot report timestamps")
		os.Exit(1)
	}

	rx.Fill(rx.NumFreeFillSlots())
	fmt.Printf("listening on %s for %s\n", *iface, *dur)

	var (
		gaps []float64 // milliseconds between one packet and the next
		last uint64
		pkts int
	)
	deadline := time.Now().Add(*dur)
	for time.Now().Before(deadline) {
		if _, err := rx.Poll(200 * time.Millisecond); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		// The only line that differs from an ordinary receive loop: ask for
		// the times as well as the packets.
		descs, ts := rx.ReceiveTimestamps(256)
		for i := range descs {
			// A device stamps a packet when it arrives and places it in the
			// queue afterwards, so under heavy loss a stamp can be older than
			// the one before it. Such a packet is skipped rather than counted
			// as a negative gap -- and, just as important, it must not become
			// the point the next gap is measured from, or the gap after it
			// gets the skipped time added to it and the tail is inflated by
			// the anomaly rather than by the traffic.
			if last != 0 && ts[i] > last {
				gaps = append(gaps, float64(ts[i]-last)/1e6)
			}
			if ts[i] > last {
				last = ts[i]
			}
			pkts++
		}
		rx.Recycle(descs)
		rx.Fill(rx.NumFreeFillSlots())
	}

	if len(gaps) < 2 {
		fmt.Printf("%d packets: too few to say anything about the gaps between them\n", pkts)
		return
	}
	sort.Float64s(gaps)
	at := func(q float64) float64 { return gaps[int(float64(len(gaps)-1)*q)] }
	fmt.Printf("\n%d packets, %d gaps, in milliseconds:\n", pkts, len(gaps))
	fmt.Printf("  shortest %8.3f\n", gaps[0])
	fmt.Printf("  median   %8.3f\n", at(0.5))
	fmt.Printf("  p99      %8.3f\n", at(0.99))
	fmt.Printf("  longest  %8.3f\n", gaps[len(gaps)-1])
}
