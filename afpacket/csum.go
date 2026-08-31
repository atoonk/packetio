//go:build linux

package afpacket

import "encoding/binary"

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
		// Reachable when len(frame) is 0 and both offsets are 0: the check
		// above compares 0 > -2 and lets it through. This is the only thing
		// that catches it.
		return false
	}
	// The L4 range ends where the IP payload ends. Anything past that is
	// Ethernet padding and must not be summed.
	end := len(frame)
	if len(frame) >= 20 {
		switch uint16(frame[12])<<8 | uint16(frame[13]) {
		case 0x0800: // IPv4: Ethernet header plus total_length
			end = 14 + int(uint16(frame[16])<<8|uint16(frame[17]))
		case 0x86dd: // IPv6: Ethernet header, fixed 40, plus payload_length
			end = 14 + 40 + int(uint16(frame[18])<<8|uint16(frame[19]))
		}
	}
	if end > len(frame) || end <= csumStart {
		// A length field that does not agree with the frame it arrived in.
		// Fall back to the whole frame rather than refusing: the checksum may
		// be a little wrong, but truncating the range would make it certainly
		// wrong.
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
