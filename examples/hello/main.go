//go:build linux

// Command hello is the smallest complete packetio program: it sends one frame
// and prints the next few it receives.
//
// It uses the AF_PACKET backend, so it needs no particular hardware -- any
// interface on any Linux box will do. Everything below the Open call is the
// backend-neutral API, so changing that one line to mlx5.Open or afxdp.Open
// gives you the same program on a faster path.
//
//	sudo go run ./examples/hello -i eth0
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/afpacket"
)

func main() {
	iface := flag.String("i", "", "interface to use, for example eth0")
	dur := flag.Duration("duration", 3*time.Second, "how long to listen")
	flag.Parse()
	if *iface == "" {
		flag.Usage()
		log.Fatal("an interface is needed")
	}

	// One transmit queue and one receive queue, sharing one region of frame
	// memory. This is the only backend-specific line in the program.
	d, err := afpacket.Open(*iface, afpacket.WithTxQueues(1), afpacket.WithRxQueues(1))
	if err != nil {
		log.Fatalf("open %s: %v", *iface, err)
	}
	defer d.Close()
	fmt.Printf("%s on %s\n", d.Capabilities().Backend, *iface)

	send(d.TxQueue(0), *iface)
	receive(d.RxQueue(0), *dur)
}

// send builds one frame and hands it to the NIC.
func send(tx packetio.TxQueue, iface string) {
	src := sourceMAC(iface)

	// SendFunc is the whole transmit cycle: it reclaims what earlier calls
	// sent, takes frames from the pool, calls this function to fill each one,
	// and transmits them. Returning the packet's length is all it asks for.
	sent, err := tx.SendFunc(1, func(i int, frame []byte) int {
		// A minimum-size Ethernet frame: broadcast, from us, with a made-up
		// EtherType so nothing else on the network tries to interpret it.
		copy(frame[0:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
		copy(frame[6:12], src)
		frame[12], frame[13] = 0x88, 0xb5 // an EtherType reserved for experiments
		for i := 14; i < 60; i++ {
			frame[i] = byte(i)
		}
		return 60
	})
	if err != nil {
		log.Fatalf("send: %v", err)
	}
	fmt.Printf("sent %d frame(s)\n", sent)

	// Sent frames stay with the NIC until they are asked for. Complete puts
	// them back in the pool; a program that never calls it runs out of frames.
	tx.Complete(tx.NumCompleted())
}

// receive prints what arrives, until the deadline.
func receive(rx packetio.RxQueue, d time.Duration) {
	fmt.Printf("listening for %s...\n", d)
	deadline := time.Now().Add(d)
	seen := 0
	for time.Now().Before(deadline) && seen < 5 {
		// Give the NIC frames to receive into. On some backends this is what
		// posts buffers; on others the kernel owns the ring and it just
		// reports how many are free.
		rx.Fill(rx.NumFreeFillSlots())

		n, err := rx.Poll(200 * time.Millisecond)
		if err != nil {
			log.Fatalf("poll: %v", err)
		}
		if n == 0 {
			continue
		}

		// Receive hands back descriptors, not bytes: each names a frame in the
		// shared region. Region().Frame turns one into the bytes it points at.
		descs := rx.Receive(16)
		for _, desc := range descs {
			b := rx.Region().Frame(desc)
			fmt.Printf("  %d bytes, %s -> %s, ethertype 0x%02x%02x\n",
				len(b), net.HardwareAddr(b[6:12]), net.HardwareAddr(b[0:6]), b[12], b[13])
			seen++
		}
		// And Recycle gives the frames back, or the pool runs dry.
		rx.Recycle(descs)
	}
	fmt.Printf("received %d frame(s)\n", seen)
}

func sourceMAC(iface string) net.HardwareAddr {
	ifi, err := net.InterfaceByName(iface)
	if err != nil || len(ifi.HardwareAddr) != 6 {
		return net.HardwareAddr{0x02, 0, 0, 0, 0, 1}
	}
	return ifi.HardwareAddr
}
