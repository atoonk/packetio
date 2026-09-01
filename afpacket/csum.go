//go:build linux

package afpacket

import (
	"encoding/binary"

	"github.com/atoonk/packetio"
)

// completeL4 finishes a partial L4 checksum in place, and reports whether it
// could.
//
// A frame delivered with VIRTIO_NET_HDR_F_NEEDS_CSUM carries only the
// pseudo-header sum in its checksum field; whoever consumes the packet has to
// sum the rest. csumStart is the offset of the L4 header from the start of the
// frame and csumOff the offset of the checksum field within it.
//
// Both offsets are written by the kernel or by a guest, so both are
// bounds-checked here rather than trusted: a bad pair would otherwise index
// out of the frame. A refused pair leaves the frame exactly as it was.
//
// The sum runs from csumStart to the end of the IP payload, not to the end of
// the frame. A frame shorter than the 60-byte Ethernet minimum is padded by
// the sender, and summing that padding produces a wrong checksum for every
// small packet -- which is why the length is taken from the L3 header instead.
// The partial already in the checksum field is part of the summed range and is
// counted once, exactly as the kernel's own skb_checksum_help does.
func completeL4(frame []byte, csumStart, csumOff int) bool {
	if csumStart < 0 || csumOff < 0 {
		return false
	}
	// Subtract, never add: csumStart is bounded first, so the second operand
	// cannot wrap and csumStart+csumOff+2 <= len(frame) follows.
	if csumStart > len(frame) || csumOff > len(frame)-csumStart-2 {
		return false
	}
	at := csumStart + csumOff
	if at+2 > len(frame) {
		// Redundant with the subtraction above, which already implies it.
		// Kept because the write below is the one thing in this file that
		// must never be wrong, and one comparison is a cheap second opinion.
		return false
	}
	// The L4 range ends where the IP payload ends. Anything past that is
	// Ethernet padding and must not be summed.
	end := len(frame)
	// A frame longer than a 16-bit length field can express makes that field
	// meaningless: the kernel writes the low bits and they name a range with
	// no relation to the packet. Summing to it would produce a confident
	// wrong answer over an arbitrary slice, which is worse than summing
	// everything. Only super-frames get that big, and those are not completed
	// at all, so this is the belt to that braces.
	// Step over any VLAN tags to reach the type. The tag the kernel stripped
	// is put back before this runs, so on a tagged link bytes 12 and 13 are a
	// TPID; reading the type there finds neither IPv4 nor IPv6, which turns
	// this bound off and puts the padding of every short tagged frame into
	// its checksum. The loop is bounded by the frame, not by a tag count: a
	// stack this does not walk to the end is a stack whose padding is summed.
	l3 := 12
	for l3+4 <= len(frame) {
		switch uint16(frame[l3])<<8 | uint16(frame[l3+1]) {
		case 0x8100, 0x88a8, 0x9100, 0x9200, 0x9300: // every TPID in use
			l3 += 4
			continue
		}
		break
	}
	if hdr := l3 + 2; hdr+8 <= len(frame) && len(frame) <= hdr+0xffff {
		switch uint16(frame[l3])<<8 | uint16(frame[l3+1]) {
		case 0x0800: // IPv4: everything before it plus total_length
			end = hdr + int(uint16(frame[hdr+2])<<8|uint16(frame[hdr+3]))
		case 0x86dd: // IPv6: the fixed 40-byte header plus payload_length
			end = hdr + 40 + int(uint16(frame[hdr+4])<<8|uint16(frame[hdr+5]))
		}
	}
	// The bound is only believed where it could be telling the truth. It may
	// stop short of the frame only by padding, and padding exists solely to
	// reach the 46-byte Ethernet minimum after the type; on anything longer a
	// length field that stops short is not describing padding, it is wrong or
	// lying. It must also leave the checksum field inside the summed range,
	// or the partial that is being completed is never counted and the answer
	// is confidently wrong -- with the frame then marked complete, so nothing
	// downstream can tell.
	if end > len(frame) || end < at+2 || (end < len(frame) && len(frame) > l3+2+46) {
		// Fall back to the whole frame rather than refusing: the checksum may
		// be a little wrong, but a truncated range makes it certainly wrong.
		end = len(frame)
	}

	sum := onesComplement(frame[csumStart:end])
	if sum == 0 {
		// A zero UDP checksum means "not computed", so the ones-complement
		// convention is to send all ones instead.
		sum = 0xffff
	}
	binary.BigEndian.PutUint16(frame[at:], sum)
	return true
}

// completeOffloads finishes the checksum of every frame in a batch that needs
// it, and reports how many could not be finished. frame(i) is the bytes of the
// i'th packet.
//
// A super-frame is left alone. Its partial is not a defect to be repaired: it
// is what the segmenter needs, and every segment's checksum is computed from
// it after the frame is cut up. Completing it writes a sum over the whole
// 64 KB in the field the segmenter reads, so each segment goes out with a
// checksum for bytes it does not contain, the far end drops the lot, and no
// counter anywhere moves. The kernel's own transmit path never completes a
// segmented frame for the same reason.
//
// A frame that IS completed has the flag cleared, because the flag means
// somebody downstream still has to finish the sum and after this nobody does.
// Leaving it set makes a forwarder that passes the metadata back to the kernel
// complete it a second time, over a field that now holds the finished sum.
func completeOffloads(offs []packetio.Offload, cont []bool, frame func(i int) []byte) int {
	bad := 0
	for i := range offs {
		o := &offs[i]
		if o.Flags&packetio.OffloadNeedsCsum == 0 || o.Segmented() {
			continue
		}
		if chained(cont, i) {
			// The packet is spread over several frames and this sees one of
			// them, so the sum would cover a fraction of the bytes it is
			// supposed to cover -- and clearing the flag afterwards would say
			// the answer is final. Leave the partial in place, exactly as for
			// a super-frame, and let whoever puts the packet back together
			// finish it.
			continue
		}
		if completeL4(frame(i), int(o.CsumStart), int(o.CsumOff)) {
			o.Flags &^= packetio.OffloadNeedsCsum
			continue
		}
		// The offsets did not fit the frame. The packet is delivered with the
		// partial still in it, which is wrong on the wire, so the flag stays
		// set -- it is still true -- and the caller is told.
		bad++
	}
	return bad
}

// chained reports whether slot i is part of a packet spread over several
// frames -- either one that continues, or one continued into.
func chained(cont []bool, i int) bool {
	if cont == nil {
		return false
	}
	return cont[i] || (i > 0 && cont[i-1])
}

// onesComplement sums b as 16-bit big-endian words and returns the folded
// complement. The checksum field is inside b and holds the pseudo-header
// partial, which is what makes this a completion rather than a computation
// from scratch.
func onesComplement(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}
