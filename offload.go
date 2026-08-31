package packetio

// Offload describes segmentation and checksum work that something other than
// this program will do: the kernel, a NIC, or a virtio peer.
//
// The layout is deliberately virtio_net_hdr, because that is the common
// currency of every path this library is likely to grow. PACKET_VNET_HDR hands
// it to an AF_PACKET socket, vhost-user puts it in front of every buffer
// between guest and device, and tap and memif use the same fields. A backend
// that speaks any of them can fill this in without translating, and code above
// packetio does not have to know which one it got.
//
// The point of carrying it at all is throughput. A GSO "super-frame" is one
// buffer of up to 64 KB that the receiver segments to MTU, so one descriptor
// does the work of forty. Dropping the metadata means dropping to one packet
// per MSS, which is most of the difference between line rate on a core and not.
type Offload struct {
	// Flags is OffloadNeedsCsum when the L4 checksum is only the pseudo-header
	// partial and somebody downstream must finish it.
	Flags uint8

	// GSOType is OffloadGSONone for an ordinary frame, or one of the
	// OffloadGSO* values for a super-frame to be segmented.
	GSOType uint8

	// HdrLen is how many bytes of headers (L2, L3 and L4 together) are
	// replicated into every segment.
	HdrLen uint16

	// GSOSize is the MSS: payload bytes per segment.
	GSOSize uint16

	// CsumStart is the offset from the start of the frame to the L4 header,
	// and CsumOff the offset from there to the two-byte checksum field.
	CsumStart uint16
	CsumOff   uint16
}

// Virtio-net header flags and segmentation types (linux/virtio_net.h). These
// are the values that go on the wire, so they are fixed, not ours to choose.
const (
	OffloadNeedsCsum uint8 = 1 // VIRTIO_NET_HDR_F_NEEDS_CSUM

	OffloadGSONone  uint8 = 0    // VIRTIO_NET_HDR_GSO_NONE
	OffloadGSOTCPv4 uint8 = 1    // VIRTIO_NET_HDR_GSO_TCPV4
	OffloadGSOUDP   uint8 = 3    // VIRTIO_NET_HDR_GSO_UDP
	OffloadGSOTCPv6 uint8 = 4    // VIRTIO_NET_HDR_GSO_TCPV6
	OffloadGSOECN   uint8 = 0x80 // VIRTIO_NET_HDR_GSO_ECN, or'd into GSOType
)

// OffloadHdrLen is the size of the virtio-net header as it appears on the wire
// in front of a frame, without the mergeable-receive-buffer field.
const OffloadHdrLen = 10

// Segmented reports whether this descriptor is a super-frame that somebody has
// to cut up, as opposed to an ordinary frame.
func (o Offload) Segmented() bool { return o.GSOType&^OffloadGSOECN != OffloadGSONone }

// Marshal writes the header little-endian into dst, which must be at least
// [OffloadHdrLen] bytes.
func (o Offload) Marshal(dst []byte) {
	_ = dst[OffloadHdrLen-1]
	dst[0] = o.Flags
	dst[1] = o.GSOType
	dst[2], dst[3] = byte(o.HdrLen), byte(o.HdrLen>>8)
	dst[4], dst[5] = byte(o.GSOSize), byte(o.GSOSize>>8)
	dst[6], dst[7] = byte(o.CsumStart), byte(o.CsumStart>>8)
	dst[8], dst[9] = byte(o.CsumOff), byte(o.CsumOff>>8)
}

// UnmarshalOffload reads a virtio-net header from src, which must be at least
// [OffloadHdrLen] bytes.
//
// Every field is written by something outside this program -- the kernel, or a
// guest -- so nothing here is trusted. Callers must bounds-check CsumStart and
// CsumOff against the frame before using them.
func UnmarshalOffload(src []byte) Offload {
	_ = src[OffloadHdrLen-1]
	return Offload{
		Flags:     src[0],
		GSOType:   src[1],
		HdrLen:    uint16(src[2]) | uint16(src[3])<<8,
		GSOSize:   uint16(src[4]) | uint16(src[5])<<8,
		CsumStart: uint16(src[6]) | uint16(src[7])<<8,
		CsumOff:   uint16(src[8]) | uint16(src[9])<<8,
	}
}

// OffloadReceiver is implemented by a receive queue that can report per-packet
// offload metadata. Use it through a type assertion:
//
//	if r, ok := rq.(packetio.OffloadReceiver); ok {
//	    descs, offs := r.ReceiveOffload(64)
//	}
//
// A queue that does not implement it delivers ordinary frames, already
// segmented by whatever was in front of it.
type OffloadReceiver interface {
	RxQueue

	// ReceiveOffload is Receive, and additionally returns one Offload per
	// descriptor. Both slices have the same length and are reused by the next
	// call, exactly as Receive's is.
	ReceiveOffload(max int) ([]Desc, []Offload)
}

// OffloadTransmitter is implemented by a transmit queue that can carry offload
// metadata alongside each frame, so a super-frame is segmented by the kernel or
// the peer rather than here.
type OffloadTransmitter interface {
	TxQueue

	// TransmitOffload is Transmit with one Offload per descriptor; a zero
	// Offload sends an ordinary frame. It returns how many were accepted,
	// always a prefix, and an error when a descriptor or its Offload was
	// refused -- an offset past the frame, a segmented frame with no segment
	// size, or offs not matching descs in length -- or ErrUnsupported when
	// this queue was not opened with offload enabled. A full ring is not an
	// error: it is a short return with a nil error, exactly as for Transmit.
	TransmitOffload(descs []Desc, offs []Offload) (int, error)
}
