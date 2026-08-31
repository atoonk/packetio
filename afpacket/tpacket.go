//go:build linux

package afpacket

// TPACKET_V3 mmap receive ring.
//
// The bounds checks below exist because earlier versions of them were not
// enough: every cursor in a block is written by the kernel, and this code has
// to distrust all of them. Each has a test that forges the exact field it
// guards against.
//
// Why a ring rather than recvmmsg: the plain socket path costs one syscall and
// one copy per batch, with SO_RCVBUF as the only burst absorber, and it was
// measured forwarding ~600-700 kpps while the kernel silently dropped a third
// to half of any faster offered load. With the ring the kernel fills a large
// mapped buffer at its own pace and the reader walks it in shared memory with
// no syscall per packet, so a burst the reader is briefly behind on waits in
// the ring instead of being dropped.
//
// V3 is block-based: the kernel fills a whole block with variable-length frames
// and flips the block's status to TP_STATUS_USER; the reader walks every frame
// in the block, then hands the whole block back with TP_STATUS_KERNEL. One
// status write per block rather than per frame.

import (
	"fmt"
	"sync/atomic"
	"unsafe"

	"github.com/atoonk/packetio"
	"golang.org/x/sys/unix"
)

const (
	packetVersion        = 10 // PACKET_VERSION
	packetRxRing         = 5  // PACKET_RX_RING
	packetAuxdata        = 8  // PACKET_AUXDATA
	packetFanout         = 18 // PACKET_FANOUT
	packetQdiscBypass    = 20 // PACKET_QDISC_BYPASS
	packetIgnoreOutgoing = 23 // PACKET_IGNORE_OUTGOING
	tpacketV3            = 2  // TPACKET_V3

	tpStatusUser       = 1 << 0 // block or frame is ready for userspace
	tpStatusKernel     = 0      // block handed back to the kernel
	tpStatusVlanValid  = 1 << 4 // tp_vlan_tci holds a tag the kernel stripped
	fanoutHash         = 0      // PACKET_FANOUT_HASH
	fanoutFlagDefrag   = 0x8000
	fanoutFlagRollover = 0x1000
)

// Ring geometry: 160 blocks of 32
// 2048-byte frames, so 5120 frames and about 10 MB per queue. Deep enough that
// a reader briefly behind never makes the kernel drop.
const (
	rxFrameSize      = 2048
	rxFramesPerBlock = 32
	rxBlockSize      = rxFrameSize * rxFramesPerBlock // 64 KiB
	rxBlockNr        = 160
	rxRetireTovMs    = 1 // flush a partly filled block after a millisecond
)

// GSO ring geometry. With PACKET_VNET_HDR the kernel delivers TCP segmentation
// offload traffic as whole super-frames of up to ~64 KB, so one ring frame must
// hold one of them: 69632 is 17 pages, enough for a full 65535-byte packet plus
// its Ethernet header and the virtio header in front. Far fewer, far bigger
// frames than the ordinary ring, and only used when GSO is asked for.
const (
	gsoFrameSize      = 69632
	gsoFramesPerBlock = 4
	gsoBlockSize      = gsoFrameSize * gsoFramesPerBlock // 272 KiB, page aligned
	gsoBlockNr        = 64
)

// packetVnetHdr is PACKET_VNET_HDR (linux/if_packet.h).
const packetVnetHdr = 15

// tpacketReq3 is struct tpacket_req3, the PACKET_RX_RING request under V3.
type tpacketReq3 struct {
	blockSize      uint32
	blockNr        uint32
	frameSize      uint32
	frameNr        uint32
	retireBlkTov   uint32
	sizeofPriv     uint32
	featureReqWord uint32
}

// tpacketBlockDesc is struct tpacket_block_desc, the header at the start of
// every block. Only the tpacket_hdr_v1 fields actually read here are named.
type tpacketBlockDesc struct {
	version       uint32
	offsetToPriv  uint32
	blockStatus   uint32
	numPkts       uint32
	offsetToFirst uint32
	blkLen        uint32
	seqNum        uint64
	tsFirst       [2]uint32
	tsLast        [2]uint32
}

// tpacket3Hdr is struct tpacket3_hdr, the per-frame header inside a block.
type tpacket3Hdr struct {
	tpNextOffset uint32
	tpSec        uint32
	tpNsec       uint32
	tpSnaplen    uint32
	tpLen        uint32
	tpStatus     uint32
	tpMac        uint16
	tpNet        uint16
	tpRxhash     uint32
	tpVlanTci    uint32
	tpVlanTpid   uint16
	tpPadding    uint16
	tpPad2       [8]uint8
}

// ring is one socket's mapped receive ring and the cursor into it. It is
// walked by a single goroutine, so the cursor needs no lock.
type ring struct {
	fd        int
	mem       []byte
	blockSize uint32
	blockNr   uint32

	block    uint32 // block being drained
	frameOff uint32 // offset of the next frame within that block
	numPkts  uint32 // frames left in that block
	pending  bool   // the block is partly drained and must not be re-read

	// oversize counts frames skipped because they did not fit a region frame,
	// or because the kernel's own cursor fields did not describe something
	// inside the block. Both are reported as receive errors rather than
	// silently dropped: a clamped length would make a 64 KB frame look like a
	// frame-sized packet, which is a wrong packet count and a wrong byte count
	// with nothing saying so.
	oversize atomic.Uint64
	packets  atomic.Uint64

	// vnet is set when the kernel prefixes every frame with a virtio-net
	// header, which is what carries the segmentation and checksum metadata.
	vnet bool
}

// setupRing switches the socket to TPACKET_V3 and maps its receive ring. With
// vnet set it uses the big-frame geometry, so segmentation-offloaded traffic
// arrives whole instead of being dropped or truncated. PACKET_VNET_HDR itself
// is set by the caller, before this, because a transmit-only socket needs it
// too and has no ring.
func setupRing(fd int, vnet bool) (*ring, error) {
	if err := unix.SetsockoptInt(fd, unix.SOL_PACKET, packetVersion, tpacketV3); err != nil {
		return nil, fmt.Errorf("PACKET_VERSION v3: %w", err)
	}
	blockSize, blockNr, frameSize, perBlock := uint32(rxBlockSize), uint32(rxBlockNr),
		uint32(rxFrameSize), uint32(rxFramesPerBlock)
	if vnet {
		blockSize, blockNr, frameSize, perBlock = gsoBlockSize, gsoBlockNr, gsoFrameSize, gsoFramesPerBlock
	}
	req := tpacketReq3{
		blockSize:    blockSize,
		blockNr:      blockNr,
		frameSize:    frameSize,
		frameNr:      blockNr * perBlock,
		retireBlkTov: rxRetireTovMs,
	}
	if _, _, e := unix.Syscall6(unix.SYS_SETSOCKOPT, uintptr(fd),
		uintptr(unix.SOL_PACKET), uintptr(packetRxRing),
		uintptr(unsafe.Pointer(&req)), unsafe.Sizeof(req), 0); e != 0 {
		return nil, fmt.Errorf("PACKET_RX_RING: %w", e)
	}
	size := int(req.blockSize * req.blockNr)
	mem, err := unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_SHARED|unix.MAP_LOCKED)
	if err != nil {
		// MAP_LOCKED needs memlock headroom, and the ring works without
		// it, so retry unlocked before giving up.
		mem, err = unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			// The ring is configured in the kernel even though it could not be
			// mapped, and a configured ring diverts packets away from the
			// ordinary receive queue while making further PACKET_VERSION
			// setsockopts return EBUSY. Unconfigure it so the socket is still
			// usable to whoever handles this error.
			teardownRing(fd)
			return nil, fmt.Errorf("mmap rx ring (%d bytes): %w", size, err)
		}
	}
	return &ring{fd: fd, mem: mem, blockSize: req.blockSize, blockNr: req.blockNr, vnet: vnet}, nil
}

// teardownRing unconfigures a ring by requesting a zero-sized one, which is the
// kernel's "free the ring" request.
func teardownRing(fd int) {
	var req tpacketReq3
	unix.Syscall6(unix.SYS_SETSOCKOPT, uintptr(fd),
		uintptr(unix.SOL_PACKET), uintptr(packetRxRing),
		uintptr(unsafe.Pointer(&req)), unsafe.Sizeof(req), 0)
}

func (r *ring) close() error {
	if r.mem == nil {
		return nil
	}
	err := unix.Munmap(r.mem)
	r.mem = nil
	teardownRing(r.fd)
	if err != nil {
		return fmt.Errorf("unmapping rx ring: %w", err)
	}
	return nil
}

// blockDesc returns the header of ring block i. Indexing the mapped slice
// rather than doing uintptr arithmetic keeps the access bounds-checked and
// clear of the GC-move hazard vet warns about: the ring is mmap memory and is
// never moved, but slice indexing is the honest way to say so.
func (r *ring) blockDesc(i uint32) *tpacketBlockDesc {
	return (*tpacketBlockDesc)(unsafe.Pointer(&r.mem[i*r.blockSize]))
}

// frameHdr returns the frame header at byte offset off within the ring.
func (r *ring) frameHdr(off uint32) *tpacket3Hdr {
	return (*tpacket3Hdr)(unsafe.Pointer(&r.mem[off]))
}

// frameHdrFits reports whether a frame header at block-relative offset off lies
// inside the block at all. It is checked before frameHdr's unchecked struct
// read, that is, before frameInBlock can look at the header's own fields.
//
// Subtract, never add: off is kernel-written (offset_to_first_pkt, or the sum
// of tp_next_offsets) and "off + headerSize <= blockSize" wraps a uint32 for a
// large one, which lets exactly the value that most needs rejecting through.
// blockSize is always far larger than a header, so the subtraction is safe.
func (r *ring) frameHdrFits(off uint32) bool {
	return off <= r.blockSize-uint32(unsafe.Sizeof(tpacket3Hdr{}))
}

// frameInBlock reports whether the frame the kernel describes lies wholly
// inside the block. Every cursor value comes from kernel-written ring fields
// (offset_to_first_pkt, tp_next_offset); a wrong one would make frameHdr's
// unchecked read walk past the mapping or into a block the kernel still owns.
// Two compares per frame, on a path that already copies the packet.
func (r *ring) frameInBlock(off uint32, tph *tpacket3Hdr) bool {
	mac := uint32(tph.tpMac)
	// In VNET_HDR mode the kernel writes the virtio header immediately before
	// the frame data, so tp_mac must leave room for it or reading that header
	// underflows out of the block.
	if r.vnet && mac < packetio.OffloadHdrLen {
		return false
	}
	// Subtract, never add: tp_mac + tp_snaplen is kernel-written and would wrap
	// a uint32, which is how a wildly oversized snaplen once passed this check.
	avail := r.blockSize - off // off <= blockSize, guaranteed by frameHdrFits
	if mac > avail {
		return false
	}
	return tph.tpSnaplen <= avail-mac
}

// advance moves the cursor to the next frame in the block.
func (r *ring) advance(next uint32) {
	r.numPkts--
	if next == 0 {
		// A zero tp_next_offset means "last frame in the block" whether or not
		// the count agrees, so stop rather than trusting numPkts.
		r.numPkts = 0
		return
	}
	r.frameOff += next
}

// read drains ready frames from the current block into the buffers dst yields,
// and returns how many it filled. It never blocks: if the current block is not
// yet TP_STATUS_USER it returns 0 and the caller parks on a poll of the fd.
//
// dst(i) returns the buffer for the i'th output packet, or nil to stop early;
// the frame is copied into it and its length returned through lens. It may be
// called more than once with the same i, because a frame that does not fit the
// buffer is skipped without producing output, and the caller is expected to
// hand back the same buffer rather than a fresh one.
func (r *ring) read(max int, dst func(i int) []byte, lens []int, offs []packetio.Offload) int {
	bd := r.blockDesc(r.block)
	if atomic.LoadUint32(&bd.blockStatus)&tpStatusUser == 0 {
		return 0
	}
	blockOff := r.block * r.blockSize
	if !r.pending {
		r.numPkts = bd.numPkts
		r.frameOff = bd.offsetToFirst
	}
	r.pending = false

	out := 0
	for r.numPkts > 0 && out < max {
		if !r.frameHdrFits(r.frameOff) {
			r.oversize.Add(1)
			r.numPkts = 0
			break
		}
		frameOff := blockOff + r.frameOff
		tph := r.frameHdr(frameOff)
		next := tph.tpNextOffset
		if !r.frameInBlock(r.frameOff, tph) {
			r.oversize.Add(1)
			r.numPkts = 0
			break
		}
		buf := dst(out)
		if buf == nil { // no free frame: leave the rest of the block for later
			break
		}
		plen := int(tph.tpSnaplen)
		d := frameOff + uint32(tph.tpMac)

		// The kernel strips the 802.1Q tag into the frame header. Put it back,
		// so what the caller sees is what was on the wire.
		tagLen, tpid, tci := 0, uint16(0), uint16(0)
		if tph.tpStatus&tpStatusVlanValid != 0 && plen >= 12 {
			if tpid = tph.tpVlanTpid; tpid == 0 {
				tpid = 0x8100
			}
			tci = uint16(tph.tpVlanTci)
			tagLen = 4
		}
		if plen+tagLen > len(buf) {
			// Does not fit: count it and skip it. Delivering a clamped length
			// instead would make an oversized frame look like a frame-sized
			// packet, with nothing saying otherwise.
			r.oversize.Add(1)
			r.advance(next)
			continue
		}
		if tagLen != 0 {
			copy(buf[0:12], r.mem[d:d+12])
			buf[12], buf[13] = byte(tpid>>8), byte(tpid)
			buf[14], buf[15] = byte(tci>>8), byte(tci)
			copy(buf[16:], r.mem[d+12:d+uint32(plen)])
		} else {
			copy(buf, r.mem[d:d+uint32(plen)])
		}
		lens[out] = plen + tagLen
		if offs != nil {
			var o packetio.Offload
			if r.vnet {
				o = packetio.UnmarshalOffload(r.mem[d-packetio.OffloadHdrLen : d])
				// csum_start is measured from the start of the frame, so it
				// moves with a tag put back in front of it.
				if tagLen != 0 && o.Flags&packetio.OffloadNeedsCsum != 0 {
					o.CsumStart += uint16(tagLen)
					if o.HdrLen != 0 {
						o.HdrLen += uint16(tagLen)
					}
				}
			}
			offs[out] = o
		}
		out++
		r.advance(next)
	}

	if r.numPkts == 0 {
		atomic.StoreUint32(&bd.blockStatus, tpStatusKernel)
		r.block = (r.block + 1) % r.blockNr
	} else {
		r.pending = true
	}
	r.packets.Add(uint64(out))
	return out
}
