//go:build linux && cgo && dpdk && (amd64 || arm64)

// Command info opens a device through DPDK and prints what it found: which
// driver claimed it, whether the kernel still has it too, what the port will
// take from the wire, and where the frame memory ended up.
//
// It moves no packets. It is the first thing to run on a new card, because
// almost everything that goes wrong later shows up here as a surprising
// number: no hugepages, a device still bound to its kernel driver, a link that
// has not come up, or an offload the NIC does not actually have.
//
//	sudo info -i eno2
//	sudo info -i 0000:c1:00.1
//	sudo info -i net_null0 -no-huge 64
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/dpdk"
)

func main() {
	var (
		iface   = flag.String("i", "", "interface, PCI address or vdev, for example eno2")
		txq     = flag.Int("tx", 1, "transmit queues to open")
		rxq     = flag.Int("rx", 1, "receive queues to open")
		depth   = flag.Int("depth", 1024, "queue depth, in packets")
		csum    = flag.Bool("csum", false, "ask for checksum offload, to see whether the device has it")
		tso     = flag.Bool("tso", false, "ask for segmentation offload as well")
		noHuge  = flag.Int("no-huge", 0, "run on ordinary memory with this many megabytes, for a vdev")
		devargs = flag.String("devargs", "", "extra driver arguments, key=value,key=value")
	)
	flag.Parse()

	if *iface == "" {
		fmt.Fprintln(os.Stderr, "no device given; try -i eno2")
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*iface, *txq, *rxq, *depth, *csum, *tso, *noHuge, *devargs); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(iface string, txq, rxq, depth int, csum, tso bool, noHuge int, devargs string) error {
	opts := []dpdk.Option{
		dpdk.WithTxQueues(txq), dpdk.WithRxQueues(rxq),
		dpdk.WithTxDepth(depth), dpdk.WithRxDepth(depth),
	}
	if csum {
		opts = append(opts, dpdk.WithChecksumOffload())
	}
	if tso {
		opts = append(opts, dpdk.WithTSO())
	}
	if noHuge > 0 {
		opts = append(opts, dpdk.WithoutHugePages(noHuge))
	}
	if devargs != "" {
		opts = append(opts, dpdk.WithDevArgs(devargs))
	}

	dev, err := dpdk.Open(iface, opts...)
	if err != nil {
		return err
	}
	defer dev.Close()

	i := dev.Info()
	fmt.Printf("device        %s, port %d\n", i.Device, i.Port)
	fmt.Printf("driver        %s\n", i.Driver)
	fmt.Printf("link          %s", upDown(i.LinkUp))
	if i.SpeedMbps > 0 {
		fmt.Printf(", %s", speed(i.SpeedMbps))
	}
	fmt.Println()
	fmt.Println()

	// Who owns the device is the thing that changes what everything else
	// means, so it is said in words rather than as a boolean.
	if i.Coexists {
		fmt.Println("ownership     shared with the kernel, which still has a network interface for")
		fmt.Println("              this device. A steering filter diverts what it matches and")
		fmt.Println("              leaves the rest to the host, so the machine stays reachable.")
	} else {
		fmt.Println("ownership     exclusive: this process has the device and the kernel has no")
		fmt.Println("              interface for it. Nothing else can receive from it, and a")
		fmt.Println("              filter only chooses which queue a packet lands on.")
	}
	if i.Steering != "" {
		fmt.Printf("steering      %s\n", i.Steering)
	}
	fmt.Println()

	c := dev.Capabilities()
	fmt.Printf("frames        %d of %d bytes, %d usable for a packet\n",
		i.Frames, i.FrameSize, c.MaxFrameSize)
	fmt.Printf("              a frame holds the mempool header, the mbuf and the headroom\n")
	fmt.Printf("              before the packet, so a descriptor starts %d bytes in\n",
		i.FrameSize-c.MaxFrameSize)
	fmt.Printf("queues        %d transmit of %d, %d receive of %d\n",
		i.TxQueues, i.TxDepth, i.RxQueues, i.RxDepth)
	fmt.Printf("memory        region at %#x, addressed by %s\n", i.RegionVA, i.IOVAMode)
	if i.Placement != "" {
		fmt.Printf("placement     %s\n", i.Placement)
	}
	fmt.Println()

	// What the device took, not what was asked for. Asking is not having: a
	// driver drops an offload it does not offer rather than failing to open.
	fmt.Println("offloads (what the device took, not what was asked for)")
	fmt.Printf("  rx checksum flags   %s\n", yesNo(c.RxChecksumFlags))
	fmt.Printf("  tx checksums        %s\n", yesNo(c.TxChecksumOffload))
	if csum && !c.TxChecksumOffload {
		fmt.Println("                      asked for, and this device does not have it")
	}
	fmt.Printf("  receive spreading   %s\n", yesNo(c.RSS))
	fmt.Printf("  zero copy           %s\n", yesNo(c.ZeroCopy))
	fmt.Printf("  hands back frames   %s\n", yesNo(c.HandsBackFrames))
	fmt.Printf("  blocking poll       %s\n", yesNo(c.BlockingPoll))
	if !c.BlockingPoll {
		fmt.Println("                      no interrupt is armed: a receive loop polls, and a")
		fmt.Println("                      negative Poll timeout burns a core")
	}

	if _, ok := dev.TxQueue(0).(packetio.OffloadTransmitter); ok && txq > 0 {
		fmt.Println()
		fmt.Println("this device's transmit queues carry per-packet offload metadata")
		fmt.Println("(packetio.OffloadTransmitter), so a super-frame can be segmented by the NIC")
	}
	return nil
}

func upDown(up bool) string {
	if up {
		return "up"
	}
	return "down (nothing will be received, and a transmit will fill the ring)"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// speed prints a link rate the way the people reading it say it.
func speed(mbps uint32) string {
	if mbps >= 1000 && mbps%1000 == 0 {
		return fmt.Sprintf("%d Gbps", mbps/1000)
	}
	return fmt.Sprintf("%d Mbps", mbps)
}
