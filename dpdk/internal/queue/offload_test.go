package queue

import (
	"errors"
	"testing"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/dpdk/internal/mbuf"
)

// tcpPacket is a tagged IPv4/TCP frame: the shape this library actually sends,
// so the header lengths under test are the ones a real packet produces.
func tcpPacket(payload int) []byte {
	p := make([]byte, 0, 18+20+20+payload)
	p = append(p, make([]byte, 12)...)    // dst, src
	p = append(p, 0x81, 0x00, 0x08, 0x07) // 802.1Q tag
	p = append(p, 0x08, 0x00)             // IPv4
	ip := make([]byte, 20)
	ip[0], ip[9] = 0x45, mbuf.ProtoTCP
	p = append(p, ip...)
	p = append(p, make([]byte, 20+payload)...) // TCP header and payload
	return p
}

// write puts a packet into the frame a descriptor names and sets its length.
func write(t *testing.T, r *rig, d *packetio.Desc, p []byte) {
	t.Helper()
	copy(r.region[d.Addr:], p)
	d.Len = uint32(len(p))
}

// mbufOf is the mbuf belonging to the frame a descriptor names.
func mbufOf(r *rig, d packetio.Desc) int {
	return r.l.MbufOffset(int(r.l.FrameOf(d.Addr)))
}

// With checksum offload on, a plain Transmit asks the NIC for the checksums and
// tells it where the headers end -- counting the VLAN tag, which is the whole
// point.
func TestTransmitChecksumOffloadSetsFlags(t *testing.T) {
	r := newRig(t, 64)
	q := r.txOffload(t, poolA, 0, 64, 32, true, false)

	descs := append([]packetio.Desc(nil), q.Alloc(1)...)
	write(t, r, &descs[0], tcpPacket(100))
	if sent := q.Transmit(descs); sent != 1 {
		t.Fatalf("sent %d, want 1", sent)
	}
	m := mbufOf(r, descs[0])
	flags := mbuf.OlFlags(r.region, m)
	if flags&mbuf.TxIPv4 == 0 || flags&mbuf.TxIPChecksum == 0 {
		t.Errorf("ol_flags %#x: an IPv4 packet must ask for the header checksum", flags)
	}
	if flags&mbuf.TxL4Mask != mbuf.TxTCPChecksum {
		t.Errorf("L4 field %#x, want TCP", flags&mbuf.TxL4Mask)
	}
	off := mbuf.TxOffload(r.region, m)
	if off&0x7f != 18 {
		t.Errorf("l2_len %d, want 18: the VLAN tag must be counted", off&0x7f)
	}
	if (off>>7)&0x1ff != 20 {
		t.Errorf("l3_len %d, want 20", (off>>7)&0x1ff)
	}
}

// Without the offload nothing is asked for, whatever the packet looks like.
func TestTransmitWithoutOffloadAsksForNothing(t *testing.T) {
	r := newRig(t, 64)
	q := r.tx(t, poolA, 0, 64, 32)
	descs := append([]packetio.Desc(nil), q.Alloc(1)...)
	write(t, r, &descs[0], tcpPacket(100))
	q.Transmit(descs)
	if f := mbuf.OlFlags(r.region, mbufOf(r, descs[0])); f != 0 {
		t.Errorf("ol_flags %#x, want none", f)
	}
}

// A packet the parser does not recognise still goes out: refusing it would make
// an ARP frame an error on a device opened with checksum offload.
func TestTransmitChecksumOffloadPassesUnparseablePackets(t *testing.T) {
	r := newRig(t, 64)
	q := r.txOffload(t, poolA, 0, 64, 32, true, false)
	descs := append([]packetio.Desc(nil), q.Alloc(1)...)
	arp := make([]byte, 42)
	arp[12], arp[13] = 0x08, 0x06
	write(t, r, &descs[0], arp)
	if sent := q.Transmit(descs); sent != 1 {
		t.Fatalf("sent %d, want an ARP frame to be sent unchanged", sent)
	}
	if f := mbuf.OlFlags(r.region, mbufOf(r, descs[0])); f != 0 {
		t.Errorf("ol_flags %#x, want none for a packet with no checksums to compute", f)
	}
}

func TestTransmitOffloadChecksumOnly(t *testing.T) {
	r := newRig(t, 64)
	q := r.txOffload(t, poolA, 0, 64, 32, true, false)
	descs := append([]packetio.Desc(nil), q.Alloc(1)...)
	write(t, r, &descs[0], tcpPacket(100))

	offs := []packetio.Offload{{
		Flags:     packetio.OffloadNeedsCsum,
		CsumStart: 38, // 18 of L2 and 20 of L3
		CsumOff:   16,
	}}
	n, err := q.TransmitOffload(descs, offs)
	if err != nil || n != 1 {
		t.Fatalf("TransmitOffload = %d, %v", n, err)
	}
	if f := mbuf.OlFlags(r.region, mbufOf(r, descs[0])); f&mbuf.TxL4Mask != mbuf.TxTCPChecksum {
		t.Errorf("ol_flags %#x, want a TCP checksum request", f)
	}
}

func TestTransmitOffloadSegmentation(t *testing.T) {
	r := newRig(t, 64)
	q := r.txOffload(t, poolA, 0, 64, 32, true, true)
	descs := append([]packetio.Desc(nil), q.Alloc(1)...)
	write(t, r, &descs[0], tcpPacket(1000))

	offs := []packetio.Offload{{
		Flags:     packetio.OffloadNeedsCsum,
		GSOType:   packetio.OffloadGSOTCPv4,
		GSOSize:   1448,
		HdrLen:    58, // 18 + 20 + 20
		CsumStart: 38,
		CsumOff:   16,
	}}
	if n, err := q.TransmitOffload(descs, offs); err != nil || n != 1 {
		t.Fatalf("TransmitOffload = %d, %v", n, err)
	}
	m := mbufOf(r, descs[0])
	f := mbuf.OlFlags(r.region, m)
	if f&mbuf.TxTCPSeg == 0 {
		t.Error("segmentation was asked for but TX_TCP_SEG is not set")
	}
	// DPDK requires the checksum flags on a segmented packet whatever the
	// caller asked for: each segment gets a fresh checksum.
	if f&mbuf.TxL4Mask != mbuf.TxTCPChecksum || f&mbuf.TxIPChecksum == 0 {
		t.Errorf("ol_flags %#x: a segmented packet must also request checksums", f)
	}
	off := mbuf.TxOffload(r.region, m)
	if (off>>16)&0xff != 20 {
		t.Errorf("l4_len %d, want 20", (off>>16)&0xff)
	}
	if (off>>24)&0xffff != 1448 {
		t.Errorf("tso_segsz %d, want 1448", (off>>24)&0xffff)
	}
}

// Segmentation alone still requests the checksums, because a NIC that cuts a
// packet up invalidates the checksum in every segment it makes. A caller that
// asks only for segmentation must not get segments the far end discards.
func TestSegmentationImpliesChecksums(t *testing.T) {
	r := newRig(t, 64)
	q := r.txOffload(t, poolA, 0, 64, 32, true, true)
	descs := append([]packetio.Desc(nil), q.Alloc(1)...)
	write(t, r, &descs[0], tcpPacket(1000))

	// No OffloadNeedsCsum: segmentation only.
	offs := []packetio.Offload{{GSOType: packetio.OffloadGSOTCPv4, GSOSize: 1448, HdrLen: 58}}
	if n, err := q.TransmitOffload(descs, offs); err != nil || n != 1 {
		t.Fatalf("TransmitOffload = %d, %v", n, err)
	}
	f := mbuf.OlFlags(r.region, mbufOf(r, descs[0]))
	if f&mbuf.TxL4Mask != mbuf.TxTCPChecksum {
		t.Errorf("ol_flags %#x: segmentation must request the L4 checksum even when "+
			"the caller did not", f)
	}
	if f&mbuf.TxTCPSeg == 0 {
		t.Error("TX_TCP_SEG missing")
	}
}

// Everything TransmitOffload refuses, and what it does with the batch when it
// does. Each of these reaches the wire as a corrupt packet if it is accepted.
func TestTransmitOffloadRefusals(t *testing.T) {
	good := packetio.Offload{Flags: packetio.OffloadNeedsCsum, CsumStart: 38, CsumOff: 16}
	for _, tc := range []struct {
		name string
		tso  bool
		off  packetio.Offload
		want error
	}{
		{
			name: "CsumStart disagreeing with the packet",
			off:  packetio.Offload{Flags: packetio.OffloadNeedsCsum, CsumStart: 34, CsumOff: 16},
			want: packetio.ErrBadLength,
		},
		{
			name: "segmentation the device does not have",
			off: packetio.Offload{Flags: packetio.OffloadNeedsCsum, GSOType: packetio.OffloadGSOTCPv4,
				GSOSize: 1448, HdrLen: 58, CsumStart: 38, CsumOff: 16},
			want: packetio.ErrUnsupported,
		},
		{
			name: "UDP segmentation, a separate offload",
			tso:  true,
			off:  packetio.Offload{GSOType: packetio.OffloadGSOUDP, GSOSize: 1448, HdrLen: 58},
			want: packetio.ErrUnsupported,
		},
		{
			name: "segmented with no segment size",
			tso:  true,
			off:  packetio.Offload{GSOType: packetio.OffloadGSOTCPv4, HdrLen: 58},
			want: packetio.ErrBadLength,
		},
		{
			name: "header length past the packet",
			tso:  true,
			off:  packetio.Offload{GSOType: packetio.OffloadGSOTCPv4, GSOSize: 1448, HdrLen: 60000},
			want: packetio.ErrBadLength,
		},
		{
			name: "header length not past the IP header",
			tso:  true,
			off:  packetio.Offload{GSOType: packetio.OffloadGSOTCPv4, GSOSize: 1448, HdrLen: 38},
			want: packetio.ErrBadLength,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, 64)
			q := r.txOffload(t, poolA, 0, 64, 32, true, tc.tso)
			descs := append([]packetio.Desc(nil), q.Alloc(2)...)
			write(t, r, &descs[0], tcpPacket(100))
			write(t, r, &descs[1], tcpPacket(100))

			// The bad one second: what comes before it must still be sent, and
			// the error must still be reported.
			n, err := q.TransmitOffload(descs, []packetio.Offload{good, tc.off})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error %v, want %v", err, tc.want)
			}
			if n != 1 {
				t.Errorf("sent %d, want the good prefix of 1", n)
			}
			if q.Stats.BadOffload.Load() != 1 {
				t.Errorf("BadOffload %d, want 1", q.Stats.BadOffload.Load())
			}
			// The refused frame is still the caller's to free.
			q.Free(descs[n:])
		})
	}
}

func TestTransmitOffloadLengthMismatch(t *testing.T) {
	r := newRig(t, 64)
	q := r.txOffload(t, poolA, 0, 64, 32, true, false)
	descs := append([]packetio.Desc(nil), q.Alloc(2)...)
	for i := range descs {
		write(t, r, &descs[i], tcpPacket(64))
	}
	if _, err := q.TransmitOffload(descs, make([]packetio.Offload, 1)); !errors.Is(err, packetio.ErrBadLength) {
		t.Errorf("error %v, want ErrBadLength for 2 descriptors and 1 offload", err)
	}
}

// A zero Offload is an ordinary frame, not a request for nothing-in-particular.
func TestTransmitOffloadZeroIsPlain(t *testing.T) {
	r := newRig(t, 64)
	q := r.txOffload(t, poolA, 0, 64, 32, true, false)
	descs := append([]packetio.Desc(nil), q.Alloc(1)...)
	write(t, r, &descs[0], tcpPacket(64))
	if n, err := q.TransmitOffload(descs, make([]packetio.Offload, 1)); err != nil || n != 1 {
		t.Fatalf("TransmitOffload = %d, %v", n, err)
	}
	if f := mbuf.OlFlags(r.region, mbufOf(r, descs[0])); f != 0 {
		t.Errorf("ol_flags %#x, want none for a zero Offload", f)
	}
}

// Frames must still be conserved when offloads are refused mid-batch.
func TestOffloadRefusalConservesFrames(t *testing.T) {
	r := newRig(t, 64)
	q := r.txOffload(t, poolA, 0, 64, 32, true, false)
	r.pmd.compThresh = 1
	before := q.NumFreeFrames()
	var back []packetio.Desc
	for i := 0; i < 100; i++ {
		descs := append([]packetio.Desc(nil), q.Alloc(4)...)
		if len(descs) < 4 {
			// Alloc gives what the ring and the free list allow, which is
			// fewer once frames are in flight; this test is about the frames,
			// not about the allocation.
			q.Free(descs)
			back = q.Reclaim(1024, back[:0])
			q.Free(back)
			continue
		}
		for j := range descs {
			write(t, r, &descs[j], tcpPacket(64))
		}
		offs := make([]packetio.Offload, len(descs))
		// One that cannot be honoured, every time.
		offs[2] = packetio.Offload{Flags: packetio.OffloadNeedsCsum, CsumStart: 9}
		n, _ := q.TransmitOffload(descs, offs)
		q.Free(descs[n:])
		back = q.Reclaim(1024, back[:0])
		q.Free(back)
	}
	// Drain what is still with the driver before counting: a frame in flight
	// is accounted for, not lost, and this test is about the ones that are not.
	for i := 0; i < 100 && q.NumInFlight() > 0; i++ {
		back = q.Reclaim(1024, back[:0])
		q.Free(back)
	}
	if got := q.NumInFlight(); got != 0 {
		t.Fatalf("%d frames still in flight after draining", got)
	}
	if got := q.NumFreeFrames(); got != before {
		t.Errorf("%d frames free after 100 rounds, want %d: refusals leaked", got, before)
	}
}
