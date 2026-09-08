//go:build linux

package netstack

// Ethernet and 802.1Q: the frame parser and header builder. Pure functions,
// shared by receive and transmit.

const (
	etherHdrLen   = 14
	vlanHdrLen    = 18
	etherTypeIPv4 = 0x0800
	etherTypeVLAN = 0x8100
	etherTypeARP  = 0x0806
	ipv4MinHdrLen = 20
	minFrameLen   = 60 // Ethernet minimum without FCS
)

var broadcastMAC = [6]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// parseFrame classifies a received Ethernet frame. It returns the length of
// the link header and the EtherType of what follows it. ok is false for a
// frame too short to hold a header, and for one whose 802.1Q tagging does not
// match the endpoint: a tagged frame on an untagged (vlan == 0) endpoint, an
// untagged frame on a tagged one, or a tag for a different VLAN. Only a single
// 802.1Q tag is understood; QinQ is treated as "some other VLAN".
func parseFrame(f []byte, vlan uint16) (linkLen int, etherType uint16, ok bool) {
	if len(f) < etherHdrLen {
		return 0, 0, false
	}
	et := uint16(f[12])<<8 | uint16(f[13])
	if et != etherTypeVLAN {
		if vlan != 0 {
			return 0, 0, false
		}
		return etherHdrLen, et, true
	}
	if len(f) < vlanHdrLen {
		return 0, 0, false
	}
	if vid := (uint16(f[14])<<8 | uint16(f[15])) & 0x0fff; vid != vlan {
		return 0, 0, false
	}
	return vlanHdrLen, uint16(f[16])<<8 | uint16(f[17]), true
}

// linkHeaderLen is the size of the header putHeader writes for this VLAN
// setting: 14 bytes untagged, 18 with an 802.1Q tag.
func linkHeaderLen(vlan uint16) int {
	if vlan != 0 {
		return vlanHdrLen
	}
	return etherHdrLen
}

// putHeader writes an Ethernet header into b, with an 802.1Q tag when vlan is
// nonzero (priority and DEI zero), and returns its length. b must have room
// for linkHeaderLen(vlan) bytes.
func putHeader(b []byte, dst, src [6]byte, vlan uint16, etherType uint16) int {
	copy(b[0:6], dst[:])
	copy(b[6:12], src[:])
	if vlan == 0 {
		b[12], b[13] = byte(etherType>>8), byte(etherType)
		return etherHdrLen
	}
	b[12], b[13] = 0x81, 0x00
	b[14], b[15] = byte(vlan>>8)&0x0f, byte(vlan)
	b[16], b[17] = byte(etherType>>8), byte(etherType)
	return vlanHdrLen
}

// isMulticast reports whether a MAC has the group bit set (broadcast included).
func isMulticast(mac []byte) bool { return mac[0]&1 != 0 }
