//go:build linux && cgo && mlx5 && (amd64 || arm64)

// Command send transmits a handful of Ethernet frames through an mlx5 NIC and
// reports what the hardware said about them.
//
// It is the smallest thing that proves the whole path works: a frame built in
// Go, a work queue entry written by Go, a doorbell rung by Go, and a completion
// read back by Go. If this puts frames on the far end's counters, everything
// after it is a matter of doing the same thing faster.
//
//	sudo send -i ens1f0np0 -dst-mac 02:00:00:00:00:02 -vlan 123 -count 10
//
// Watch for them on the other machine with something like
//
//	tcpdump -i ens1f0np0 -e -n vlan 123
package main

import (
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"time"

	"github.com/atoonk/packetio/examples/internal/frame"
	"github.com/atoonk/packetio/mlx5"
)

func main() {
	var (
		iface   = flag.String("i", "", "network interface, for example ens1f0np0")
		dstMAC  = flag.String("dst-mac", "", "destination Ethernet address (required)")
		srcMAC  = flag.String("src-mac", "", "source Ethernet address; defaults to the interface's own")
		vlan    = flag.Int("vlan", 0, "VLAN id to tag with, or 0 for untagged")
		outer   = flag.Int("outer-vlan", 0, "outer VLAN id, for a double-tagged frame")
		size    = flag.Int("size", 60, "frame length in bytes, not counting the 4-byte frame check sequence")
		count   = flag.Int("count", 1, "how many frames to send")
		srcIP   = flag.String("src-ip", "10.0.0.1", "source IPv4 address")
		dstIP   = flag.String("dst-ip", "10.0.0.2", "destination IPv4 address")
		srcPort = flag.Int("src-port", 1024, "UDP source port")
		dstPort = flag.Int("dst-port", 9, "UDP destination port")
	)
	flag.Parse()

	if *iface == "" || *dstMAC == "" {
		fmt.Fprintln(os.Stderr, "an interface and a destination address are needed")
		flag.Usage()
		os.Exit(2)
	}
	if err := run(opts{
		iface: *iface, dstMAC: *dstMAC, srcMAC: *srcMAC,
		vlan: *vlan, outer: *outer, size: *size, count: *count,
		srcIP: *srcIP, dstIP: *dstIP,
		srcPort: uint16(*srcPort), dstPort: uint16(*dstPort),
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type opts struct {
	iface, dstMAC, srcMAC    string
	vlan, outer, size, count int
	srcIP, dstIP             string
	srcPort, dstPort         uint16
}

func run(o opts) error {
	dst, err := net.ParseMAC(o.dstMAC)
	if err != nil {
		return fmt.Errorf("destination address: %w", err)
	}
	src, err := sourceMAC(o.srcMAC, o.iface)
	if err != nil {
		return err
	}
	srcIP, err := netip.ParseAddr(o.srcIP)
	if err != nil {
		return fmt.Errorf("source address: %w", err)
	}
	dstIP, err := netip.ParseAddr(o.dstIP)
	if err != nil {
		return fmt.Errorf("destination address: %w", err)
	}

	f, err := frame.Build(frame.Spec{
		SrcMAC: src, DstMAC: dst,
		VLAN: o.vlan, OuterVLAN: o.outer,
		SrcIP: srcIP, DstIP: dstIP,
		SrcPort: o.srcPort, DstPort: o.dstPort,
		Size: o.size,
	})
	if err != nil {
		return err
	}

	// A device in L2 inline mode must be shown the whole Ethernet header, and a
	// tagged frame's is longer than an untagged one's. Saying how long these
	// frames' headers are lets the library work out the rest.
	dev, err := mlx5.Open(o.iface,
		mlx5.WithTxQueues(1),
		mlx5.WithTxDepth(1024),
		mlx5.WithEthernetHeaderLen(mlx5.EthernetHeaderLen(f.VLANTags)),
	)
	if err != nil {
		return err
	}
	defer dev.Close()
	info := dev.Info()

	fmt.Printf("%s: %s port %d, %d inline header bytes, link %s\n",
		info.Interface, info.IBDev, info.Port, info.InlineHeader, upDown(info.PortActive))
	fmt.Printf("sending %d frames of %d bytes, %s -> %s", o.count, len(f.Bytes), src, dst)
	if f.VLANTags > 0 {
		fmt.Printf(", vlan %d", o.vlan)
		if o.outer != 0 {
			fmt.Printf(" inside %d", o.outer)
		}
	}
	fmt.Println()

	q := dev.Tx(0)
	sent, err := q.SendFunc(o.count, func(i int, b []byte) int {
		return copy(b, f.Bytes)
	})
	if err != nil {
		return err
	}
	if sent != o.count {
		fmt.Printf("the queue took %d of %d frames\n", sent, o.count)
	}

	// Wait for the hardware to say what became of them. A completion is the
	// only proof the frames left; nothing else in this program can tell.
	deadline := time.Now().Add(2 * time.Second)
	done := 0
	for done < sent && time.Now().Before(deadline) {
		done += q.Complete(sent - done)
	}

	st, _ := q.Stats()
	fmt.Printf("queued %d, completed %d, %d bytes, %d doorbells, %d completions\n",
		st.Packets, st.Completed, st.Bytes, st.Batches, st.Completions)

	if err := q.Err(); err != nil {
		return err
	}
	if done < sent {
		return fmt.Errorf("only %d of %d frames completed; the hardware has not reported on the rest", done, sent)
	}
	if st.Errors > 0 {
		return fmt.Errorf("the hardware reported %d errors", st.Errors)
	}
	fmt.Println("every frame completed without error")
	return nil
}

// sourceMAC picks the address to send from: the one asked for, or the
// interface's own, which is what a switch will already have learned.
func sourceMAC(want, iface string) (net.HardwareAddr, error) {
	if want != "" {
		mac, err := net.ParseMAC(want)
		if err != nil {
			return nil, fmt.Errorf("source address: %w", err)
		}
		return mac, nil
	}
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("looking up %s: %w", iface, err)
	}
	if len(ifi.HardwareAddr) != 6 {
		return nil, fmt.Errorf("%s has no Ethernet address; give one with -src-mac", iface)
	}
	return ifi.HardwareAddr, nil
}

func upDown(b bool) string {
	if b {
		return "up"
	}
	return "down"
}
