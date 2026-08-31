// Command filter receives with a filter and reports what arrives, by
// destination port, so you can see the filter doing its job.
//
// It needs a backend that steers, which today means mlx5, so build it with
// -tags mlx5. AF_PACKET has no filter: it is a tap, and the kernel sees every
// packet whatever a socket takes.
//
//	sudo go run ./examples/steer -i eth0 -udp-port 9000
//	sudo go run ./examples/steer -i eth0 -udp-port 9000 -udp-port 9001 -vlan 2053
//	sudo go run ./examples/steer -i eth0 -cidr 10.0.0.0/8
//	sudo go run ./examples/steer -i eth0 -all
package main

import (
	"flag"
	"fmt"
	"log"
	"net/netip"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/atoonk/packetio"
)

// backends is filled by the per-backend files; mlx5 when built with -tags
// mlx5. Without any, the command says so rather than pretending.
var backends = map[string]func(iface string, f packetio.SteeringFilter) (packetio.Device, string, error){}

type ports []uint16

func (p *ports) String() string { return fmt.Sprint(*p) }
func (p *ports) Set(s string) error {
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return err
	}
	*p = append(*p, uint16(n))
	return nil
}

func main() {
	var udp, tcp ports
	var (
		iface   = flag.String("i", "", "interface to receive on")
		backend = flag.String("backend", "mlx5", "backend to receive with")
		vlan    = flag.Int("vlan", 0, "match this 802.1Q VLAN id (0 = any)")
		cidr    = flag.String("cidr", "", "match packets from this source prefix")
		all     = flag.Bool("all", false, "take every packet the port sees")
		dur     = flag.Duration("duration", 10*time.Second, "how long to run")
	)
	flag.Var(&udp, "udp-port", "match this UDP destination port (repeatable)")
	flag.Var(&tcp, "tcp-port", "match this TCP destination port (repeatable)")
	flag.Parse()
	if *iface == "" {
		flag.Usage()
		os.Exit(2)
	}
	open, ok := backends[*backend]
	if !ok && len(backends) == 0 {
		log.Fatalf("this build has no steering backend; build with -tags mlx5")
	}
	if !ok {
		names := make([]string, 0, len(backends))
		for n := range backends {
			names = append(names, n)
		}
		sort.Strings(names)
		log.Fatalf("no backend %q in this build; have %s", *backend, strings.Join(names, ", "))
	}

	// Build the filter. Repeated ports are alternatives; everything else is
	// ANDed with them.
	f := packetio.SteeringFilter{Promiscuous: *all}
	if *vlan > 0 {
		f.Match = append(f.Match, packetio.MatchVLAN(uint16(*vlan)))
	}
	if *cidr != "" {
		p, err := netip.ParsePrefix(*cidr)
		if err != nil {
			log.Fatalf("-cidr %q: %v", *cidr, err)
		}
		f.Match = append(f.Match, packetio.MatchSrcIP(p))
	}
	for _, p := range udp {
		f.Match = append(f.Match, packetio.MatchDstPort(packetio.IPProtoUDP, p)...)
	}
	for _, p := range tcp {
		f.Match = append(f.Match, packetio.MatchDstPort(packetio.IPProtoTCP, p)...)
	}

	d, installed, err := open(*iface, f)
	if err != nil {
		log.Fatalf("open %s with %s: %v", *iface, *backend, err)
	}
	defer d.Close()
	fmt.Printf("%s on %s\n  asked for: %s\n  installed: %s\n", *backend, *iface, f, installed)

	q := d.RxQueue(0)
	region := q.Region()
	seen := map[string]int{}
	total := 0

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	deadline := time.After(*dur)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	last := 0
	for {
		select {
		case <-stop:
			report(seen, total)
			return
		case <-deadline:
			report(seen, total)
			return
		case <-tick.C:
			fmt.Printf("  %d packets/s\n", total-last)
			last = total
		default:
		}
		q.Fill(q.NumFreeFillSlots())
		n, err := q.Poll(100 * time.Millisecond)
		if err != nil {
			log.Fatalf("poll: %v", err)
		}
		if n == 0 {
			continue
		}
		descs := q.Receive(256)
		for _, desc := range descs {
			seen[describe(region.Frame(desc))]++
			total++
		}
		q.Recycle(descs)
	}
}

// describe names a frame by its protocol and destination port, which is what
// the filters here select on.
func describe(b []byte) string {
	if len(b) < 34 {
		return "short"
	}
	off := 12
	if b[12] == 0x81 && b[13] == 0x00 {
		off = 16 // an 802.1Q tag the backend left in place
	}
	if b[off] != 0x08 || b[off+1] != 0x00 {
		return fmt.Sprintf("ethertype 0x%02x%02x", b[off], b[off+1])
	}
	ip := b[off+2:]
	if len(ip) < 20 {
		return "short ipv4"
	}
	proto := ip[9]
	l4 := ip[int(ip[0]&0x0f)*4:]
	switch {
	case proto == 17 && len(l4) >= 4:
		return fmt.Sprintf("udp/%d", uint16(l4[2])<<8|uint16(l4[3]))
	case proto == 6 && len(l4) >= 4:
		return fmt.Sprintf("tcp/%d", uint16(l4[2])<<8|uint16(l4[3]))
	}
	return fmt.Sprintf("ip proto %d", proto)
}

func report(seen map[string]int, total int) {
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("\nreceived %d packets", total)
	if len(keys) > 0 {
		fmt.Printf(":")
	}
	fmt.Println()
	for _, k := range keys {
		fmt.Printf("  %-16s %d\n", k, seen[k])
	}
}
