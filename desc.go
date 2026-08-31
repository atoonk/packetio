package packetio

// Desc identifies one frame within a Region: where it starts and how many
// bytes of it are meaningful.
//
// The layout matches AF_XDP's xdp_desc field for field, which keeps the
// AF_XDP adapter's conversion trivial. It is a field-by-field copy, not a
// cast: nothing here depends on the two structs having the same memory
// layout, and nothing should start to without a compile-time assertion.
type Desc struct {
	// Addr is the byte offset of the frame within the Region. For a received
	// frame it points at the first byte of the Ethernet header, which need not
	// be the start of the frame if the backend reserved headroom.
	Addr uint64

	// Len is the number of valid bytes at Addr: the Ethernet frame length,
	// excluding the FCS the NIC appends on transmit and strips on receive.
	Len uint32

	// Options carries backend-defined flags. Bits below OptionsBackendShift are
	// reserved for this package; the rest belong to the backend that produced
	// the descriptor. Transmit ignores these bits, so a forwarder may pass a
	// received descriptor straight back without clearing them.
	Options uint32
}

// Option bits defined by this package. A backend that cannot express one
// simply never sets it.
const (
	// OptContinued marks a frame that is one fragment of a larger packet, with
	// at least one more fragment following. The last fragment does not have it.
	OptContinued uint32 = 1 << 0

	// OptChecksumOK reports that the NIC verified the L3 and L4 checksums of a
	// received frame and found them correct.
	OptChecksumOK uint32 = 1 << 1

	// OptL3ChecksumOK reports that the NIC verified the received packet's IP
	// header checksum and found it correct. A forwarder that would otherwise
	// verify the header itself can trust this and skip the work; it says
	// nothing about the payload.
	OptL3ChecksumOK uint32 = 1 << 2

	// OptionsBackendShift is the first Options bit available to backends.
	OptionsBackendShift = 16
)

// Region is a contiguous run of frame memory shared with the NIC. It is
// allocated and registered once when a Device is opened, which is what pins it,
// and it stays valid until the Device is closed.
//
// Frames are fixed size and laid out end to end: frame i occupies
// [i*FrameSize(), (i+1)*FrameSize()). Backends hand out descriptors whose Addr
// falls inside a frame, not necessarily at its start.
type Region interface {
	// Bytes returns the whole region. The slice aliases the mapping; it is not
	// a copy, and writing outside a frame the caller owns corrupts traffic.
	Bytes() []byte

	// Frame returns the bytes a descriptor names: Bytes()[d.Addr:d.Addr+d.Len].
	Frame(d Desc) []byte

	// Writable returns the whole of the frame containing d, from d.Addr to the
	// end of that frame. Use it to build a packet whose final length is not
	// known yet, then set Desc.Len before transmitting.
	Writable(d Desc) []byte

	// FrameSize is the size of one frame in bytes, and so the largest single
	// frame a packet can occupy.
	FrameSize() int

	// NumFrames is how many frames the region holds.
	NumFrames() int
}
