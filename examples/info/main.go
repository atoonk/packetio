//go:build linux && cgo && mlx5 && (amd64 || arm64)

// Command info opens an mlx5 interface and prints what it found: the device,
// the port, what the packet path will have to do to satisfy it, and where the
// queue memory ended up.
//
// It transmits nothing. It is the first thing to run on a new card, because
// almost everything that can go wrong later shows up here as a surprising
// number.
//
//	sudo info -i ens1f0np0
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/atoonk/packetio/mlx5"
)

func main() {
	var (
		iface = flag.String("i", "", "network interface, for example ens1f0np0")
		depth = flag.Int("depth", 1024, "transmit queue depth to create, in packets")
		huge  = flag.Bool("hugepages", false, "ask for huge pages for the frame region")
	)
	flag.Parse()

	if *iface == "" {
		fmt.Fprintln(os.Stderr, "no interface given; try -i ens1f0np0")
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*iface, *depth, *huge); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(iface string, depth int, huge bool) error {
	opts := []mlx5.Option{mlx5.WithTxQueues(1), mlx5.WithTxDepth(depth)}
	if huge {
		opts = append(opts, mlx5.WithHugePages())
	}

	dev, err := mlx5.Open(iface, opts...)
	if err != nil {
		return err
	}
	defer dev.Close()

	i := dev.Info()
	fmt.Printf("interface     %s\n", i.Interface)
	fmt.Printf("device        %s port %d, firmware %s\n", i.IBDev, i.Port, i.Firmware)
	fmt.Printf("link          %s\n", upDown(i.PortActive))
	fmt.Println()

	fmt.Printf("inline header %d bytes required", i.RequiredInline)
	switch {
	case i.RequiredInline == 0:
		fmt.Printf(" (the device parses nothing out of the entry; enhanced multi-packet entries are possible)\n")
	case i.RequiredInline == 18:
		fmt.Printf(" (an Ethernet header and one VLAN tag; a QinQ frame needs %d)\n", mlx5.EthernetHeaderLen(2))
	default:
		fmt.Println()
	}
	fmt.Printf("              %d bytes in use\n", i.InlineHeader)
	fmt.Printf("multi-packet  %s by the device, %s in use\n", yesNo(i.EnhancedMPW), yesNo(i.MultiPacket))
	fmt.Println()

	fmt.Printf("region        %d frames of %d bytes, %d MiB at %#x, key %#x\n",
		i.Frames, i.FrameSize, i.Frames*i.FrameSize>>20, i.RegionVA, i.LKey)
	fmt.Printf("huge pages    %s\n", yesNo(i.HugePages))
	fmt.Println()

	q := dev.Tx(0)
	st, err := q.Stats()
	if err != nil {
		return err
	}
	fmt.Printf("transmit      queue pair %d, completion queue %d\n",
		st.Backend["qpn"], st.Backend["cqn"])
	fmt.Printf("              %d blocks of 64 bytes, %d completion entries\n",
		st.Backend["sq_blocks"], st.Backend["cq_entries"])
	fmt.Printf("              %d packets can be in flight\n", q.NumFreeSlots())
	fmt.Printf("              %d frames on the free list\n", q.NumFreeFrames())

	if !i.PortActive {
		fmt.Println()
		fmt.Println("the link is down: frames can be queued but nothing will leave the port")
	}
	return nil
}

func upDown(b bool) string {
	if b {
		return "up"
	}
	return "down"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
