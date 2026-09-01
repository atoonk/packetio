//go:build linux && cgo && mlx5 && (amd64 || arm64)

// Command pingpong measures how long a packet takes to go out, come back, and
// be seen again, and how much of that time was spent here rather than at the
// far end.
//
// Run it on two machines facing each other:
//
//	# the far end, which sends every frame straight back
//	sudo pingpong -i eno2 -mode reflect -vlan 2043
//
//	# the near end, which times the round trips
//	sudo pingpong -i eno2 -mode measure -vlan 2043 \
//	        -src-mac <local> -dst-mac <far end> -n 200000
//
// # What the number means
//
// A round trip is not a network measurement. On a cable between two machines
// in one rack the wire is tens of nanoseconds; nearly all of a round trip is
// the two computers. It covers six things: building and sending here, the
// wire, the far end noticing and turning the frame round, the wire back, and
// this program seeing the reply. One number for all of it.
//
// So the reply's arrival timestamp is worth having, because it splits that
// number in two. The card records when the reply reached the port, before the
// transfer to memory and before any of this code ran, so:
//
//	arrival stamp - send time   everything outside this program's receive path
//	now           - arrival     this program's own receive path
//
// The second is the only part packetio is responsible for. Without the stamp
// there is no way to tell one from the other, and "it took four microseconds"
// says nothing about whose four they were.
//
// The two clocks are unrelated -- the card's has its own epoch -- so the split
// is anchored on the fastest round trip seen, on the argument that the quickest
// reply is the one that waited least. It is an estimate. The round trip itself,
// and the intervals between arrival stamps, are exact.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/examples/internal/affinity"
	"github.com/atoonk/packetio/examples/internal/frame"
	"github.com/atoonk/packetio/mlx5"
)

func main() {
	var (
		iface  = flag.String("i", "", "interface")
		mode   = flag.String("mode", "measure", "measure or reflect")
		vlan   = flag.Int("vlan", 0, "vlan id, 0 for untagged")
		srcMAC = flag.String("src-mac", "", "this interface's address (measure)")
		dstMAC = flag.String("dst-mac", "", "the reflector's address (measure)")
		count  = flag.Int("n", 100000, "round trips to time (measure)")
		size   = flag.Int("size", 64, "frame size on the wire, including the 4-byte FCS")
		cpu    = flag.Int("cpu", -1, "processor to pin to, -1 to leave it alone")
	)
	flag.Parse()
	if *iface == "" {
		flag.Usage()
		os.Exit(2)
	}
	if *cpu >= 0 {
		if err := affinity.Pin(*cpu); err != nil {
			fail(err)
		}
	}

	// One queue each way. A round trip is one frame in flight, so more queues
	// would only spread the work of a single packet over cores that then wait
	// for each other.
	steer := packetio.SteeringFilter{}
	if *vlan != 0 {
		steer.Match = []packetio.Match{packetio.MatchVLAN(uint16(*vlan))}
	} else {
		steer.Promiscuous = true
	}
	d, err := mlx5.Open(*iface, mlx5.WithQueues(1), mlx5.WithSteering(steer))
	if err != nil {
		fail(err)
	}
	defer d.Close()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	switch *mode {
	case "reflect":
		reflect(d, stop)
	case "measure":
		measure(d, *srcMAC, *dstMAC, *vlan, *size, *count, stop)
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", *mode)
		os.Exit(2)
	}
}

// reflect sends every frame back where it came from, in the frame it arrived
// in: swap the addresses in place and transmit, then hand the frames back to
// the receive queue they belong to. Nothing is copied and nothing allocated.
func reflect(d *mlx5.Device, stop <-chan os.Signal) {
	rx, tx := d.RxQueue(0), d.TxQueue(0)
	region := rx.Region()
	rx.Fill(rx.NumFreeFillSlots())
	fmt.Println("reflecting; ^C to stop")

	var seen uint64
	back := make([]packetio.Desc, 0, 256)
	for {
		select {
		case <-stop:
			fmt.Printf("reflected %d frames\n", seen)
			return
		default:
		}
		descs := rx.Receive(64)
		out := descs[:0]
		for _, dd := range descs {
			b := region.Frame(dd)
			if len(b) < 12 {
				continue
			}
			// Swap destination and source, so the frame goes home.
			var mac [6]byte
			copy(mac[:], b[0:6])
			copy(b[0:6], b[6:12])
			copy(b[6:12], mac[:])
			out = append(out, dd)
		}
		if n := tx.Transmit(out); n < len(out) {
			rx.Recycle(out[n:])
		}
		seen += uint64(len(out))
		// The frames belong to the receive queue, so they come home through
		// Reclaim rather than Complete.
		back = tx.Reclaim(tx.NumInFlight(), back[:0])
		rx.Recycle(back)
		rx.Fill(rx.NumFreeFillSlots())
	}
}

func measure(d *mlx5.Device, src, dst string, vlan, size, count int, stop <-chan os.Signal) {
	sm, err := net.ParseMAC(src)
	if err != nil {
		fail(fmt.Errorf("-src-mac: %w", err))
	}
	dm, err := net.ParseMAC(dst)
	if err != nil {
		fail(fmt.Errorf("-dst-mac: %w", err))
	}
	f, err := frame.Build(frame.Spec{
		SrcMAC: sm, DstMAC: dm, VLAN: vlan,
		SrcIP: netip.MustParseAddr("10.0.0.1"), DstIP: netip.MustParseAddr("10.0.0.2"),
		SrcPort: 9000, DstPort: 9000, Size: size - 4,
	})
	if err != nil {
		fail(err)
	}

	rx, tx := d.RxQueue(0), d.TxQueue(0)
	stamps, canStamp := rx.(packetio.TimestampReceiver)
	canStamp = canStamp && d.Capabilities().RxTimestamps
	rx.Fill(rx.NumFreeFillSlots())

	rtt := make([]time.Duration, 0, count)
	inbound := make([]uint64, 0, count) // arrival stamp of each reply
	sentAt := make([]time.Time, 0, count)

	fmt.Printf("timing %d round trips of %d bytes\n", count, size)
	for i := 0; i < count; i++ {
		select {
		case <-stop:
			count = i
		default:
		}
		if count == i {
			break
		}
		start := time.Now()
		if _, err := tx.SendFunc(1, func(_ int, buf []byte) int {
			return copy(buf, f.Bytes)
		}); err != nil {
			fail(err)
		}
		// One frame in flight: wait for its reply before sending the next, so
		// nothing queues behind anything.
		deadline := time.Now().Add(50 * time.Millisecond)
		for time.Now().Before(deadline) {
			var descs []packetio.Desc
			var ts []uint64
			if canStamp {
				descs, ts = stamps.ReceiveTimestamps(8)
			} else {
				descs = rx.Receive(8)
			}
			if len(descs) > 0 {
				rtt = append(rtt, time.Since(start))
				sentAt = append(sentAt, start)
				if canStamp {
					inbound = append(inbound, ts[0])
				}
				rx.Recycle(descs)
				rx.Fill(rx.NumFreeFillSlots())
				break
			}
			rx.Recycle(descs)
		}
		tx.Complete(8)
	}
	report(rtt, inbound, sentAt, canStamp)
}

func report(rtt []time.Duration, inbound []uint64, sentAt []time.Time, canStamp bool) {
	if len(rtt) < 2 {
		fmt.Println("no replies came back")
		return
	}
	sorted := append([]time.Duration(nil), rtt...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	at := func(q float64) time.Duration { return sorted[int(float64(len(sorted)-1)*q)] }

	fmt.Printf("\n%d round trips, microseconds:\n", len(rtt))
	for _, r := range []struct {
		name string
		v    time.Duration
	}{
		{"median", at(0.5)}, {"p99", at(0.99)}, {"p99.9", at(0.999)}, {"max", sorted[len(sorted)-1]},
	} {
		fmt.Printf("  %-8s %8.2f\n", r.name, float64(r.v.Nanoseconds())/1000)
	}

	if !canStamp || len(inbound) != len(rtt) {
		fmt.Println("\n(the device does not timestamp, so the round trip cannot be split)")
		return
	}
	// Split the round trip. The card's clock and this program's clock have
	// nothing in common, so the offset between them is anchored on the
	// quickest round trip seen: the reply that waited least is the one whose
	// arrival is closest to when this code saw it.
	best, bestI := sorted[0], 0
	for i, r := range rtt {
		if r == best {
			bestI = i
			break
		}
	}
	// Align the two clocks on the fastest trip, assuming its reply spent the
	// least possible time in this program's receive path: its arrival stamp
	// then lines up with the moment this code saw it, which is send time plus
	// the round trip. Anchoring on the send time instead would charge the
	// whole round trip to the receive path, which is the error this comment
	// exists to stop someone repeating.
	offset := uint64(sentAt[bestI].Add(rtt[bestI]).UnixNano()) - inbound[bestI]
	ours := make([]float64, 0, len(rtt))
	for i := range rtt {
		arrived := int64(inbound[i] + offset)
		our := sentAt[i].Add(rtt[i]).UnixNano() - arrived
		if our >= 0 {
			ours = append(ours, float64(our)/1000)
		}
	}
	if len(ours) == 0 {
		return
	}
	sort.Float64s(ours)
	med := ours[len(ours)/2]
	fmt.Printf("\n  of the median round trip, this program's receive path took about %.2f us\n", med)
	fmt.Printf("  the rest was the wire and the far end. The split is anchored on the\n")
	fmt.Printf("  fastest trip seen, so treat it as an estimate; the round trips are exact.\n")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
