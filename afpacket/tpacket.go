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

	// batchTooSmall counts the subset of oversize whose cause is the caller's
	// batch rather than its frames: a chained packet needing more slots than
	// max. It is the one drop here the caller can fix without reconfiguring
	// the device, so it is worth being able to see on its own.
	batchTooSmall atomic.Uint64
	packets       atomic.Uint64

	// vnet is set when the kernel prefixes every frame with a virtio-net
	// header, which is what carries the segmentation and checksum metadata.
	// chain is true when a packet too big for one buffer is laid across
	// several instead of being dropped. Off by default: a caller that does
	// not expect OptContinued would see a fragment as a whole packet.
	chain bool

	vnet bool
}

// setupRing switches the socket to TPACKET_V3 and maps its receive ring. With
// vnet set it uses the big-frame geometry, so segmentation-offloaded traffic
// arrives whole instead of being dropped or truncated. PACKET_VNET_HDR itself
// is set by the caller, before this, because a transmit-only socket needs it
// too and has no ring.
func setupRing(fd int, vnet, chain bool) (*ring, error) {
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
	return &ring{fd: fd, mem: mem, blockSize: req.blockSize, blockNr: req.blockNr, vnet: vnet, chain: chain}, nil
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
// When the ring is chaining, a packet too big for one buffer is split across
// several output slots instead of being dropped: cont[i] is then true for
// every slot but the last of a packet, which is what the caller turns into
// OptContinued. A packet that cannot be finished -- slots or frames ran out --
// produces nothing and is left in the ring for the next call, because half a
// packet is not a shorter packet.
func (r *ring) read(max int, dst func(i int) []byte, lens []int, offs []packetio.Offload, tss []uint64, cont []bool) int {
	out := 0
	// Drain consecutive ready blocks until max is met or the ring runs dry.
	// Each block goes back to the kernel the moment it is empty -- holding a
	// finished block across the caller's processing pass starves the ring
	// under pressure for no benefit.
blocks:
	for out < max {
		bd := r.blockDesc(r.block)
		if atomic.LoadUint32(&bd.blockStatus)&tpStatusUser == 0 {
			break
		}
		blockOff := r.block * r.blockSize
		if !r.pending {
			r.numPkts = bd.numPkts
			r.frameOff = bd.offsetToFirst
		}
		r.pending = false

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
			if tph.tpSnaplen < tph.tpLen {
				// The kernel clipped it: a packet longer than a block's
				// payload area arrives with tp_snaplen cut and tp_len still
				// saying how long it really was. That is a truncated packet
				// with a plausible length, which is the one thing this loop
				// must never deliver, so it is counted oversize and skipped.
				// Unreachable through a ring frame -- only a region frame
				// near the block size can hold what the kernel clipped.
				r.oversize.Add(1)
				r.advance(next)
				continue
			}

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
				if !r.chain {
					// Does not fit: count it and skip it. Delivering a clamped length
					// instead would make an oversized frame look like a frame-sized
					// packet, with nothing saying otherwise.
					r.oversize.Add(1)
					r.advance(next)
					continue
				}
				// Chaining: lay the packet across as many slots as it takes.
				// The whole packet or none of it -- a caller handed half of
				// one has no way to know that is what it is.
				n, ok, noSlots := r.spread(out, max, d, plen, tagLen, tpid, tci, dst, lens, cont)
				if !ok {
					if noSlots && out == 0 {
						// The caller's whole batch is too small for this
						// packet, so no call of this size will ever take it
						// and waiting would stop the queue for good. Count it
						// and step over it, which is what a packet too big for
						// one frame gets when chaining is off.
						r.oversize.Add(1)
						r.batchTooSmall.Add(1)
						r.advance(next)
						continue
					}
					// Room ran out with work already done, or frames did.
					// Either will be there next time; leave the packet where
					// it is.
					r.pending = true
					break blocks
				}
				if offs != nil {
					// Metadata describes the packet, so it rides its first
					// slot; the rest carry none. They must be cleared rather
					// than left, because this scratch is reused and a stale
					// NeedsCsum on a continuation would have the completion
					// pass write two bytes into the middle of a packet.
					offs[out] = r.offloadFor(d, tagLen)
					for i := out + 1; i < out+n; i++ {
						offs[i] = packetio.Offload{}
					}
				}
				if tss != nil {
					// Likewise the arrival time: one packet, one stamp, on the
					// slot that starts it.
					tss[out] = uint64(tph.tpSec)*1e9 + uint64(tph.tpNsec)
					for i := out + 1; i < out+n; i++ {
						tss[i] = 0
					}
				}
				out += n
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
				offs[out] = r.offloadFor(d, tagLen)
			}
			if tss != nil {
				// The kernel stamps every frame as it puts it in the ring,
				// whatever the socket asked for, so this costs a read and
				// nothing else. Which clock it used is PACKET_TIMESTAMP's
				// business; the header carries the answer either way.
				tss[out] = uint64(tph.tpSec)*1e9 + uint64(tph.tpNsec)
			}
			out++
			r.advance(next)
		}

		if r.numPkts != 0 {
			r.pending = true
			break blocks
		}
		atomic.StoreUint32(&bd.blockStatus, tpStatusKernel)
		r.block = (r.block + 1) % r.blockNr
	}
	r.packets.Add(uint64(out))
	return out
}

// offloadFor reads the virtio header the kernel put in front of the frame at
// d, with the offsets moved by a tag put back in front of the packet.
//
// Both offsets are measured from the start of the frame, so both move with the
// tag. They move independently: the kernel accepts a segmented frame with no
// partial checksum (it finds the transport header by dissecting the flow
// instead), and such a frame has a header length to shift and no csum_start.
//
// Saturating, not wrapping. These are kernel-written and bounds-checked
// downstream rather than trusted, and a value near the top of the field would
// wrap to a small one -- turning an offset the checker would have refused into
// one it accepts, which writes over a header and then reports the frame as
// complete.
func (r *ring) offloadFor(d uint32, tagLen int) packetio.Offload {
	var o packetio.Offload
	if !r.vnet {
		return o
	}
	o = packetio.UnmarshalOffload(r.mem[d-packetio.OffloadHdrLen : d])
	if tagLen == 0 {
		return o
	}
	shift := func(v uint16) uint16 {
		if int(v)+tagLen > 0xffff {
			return 0xffff
		}
		return v + uint16(tagLen)
	}
	if o.Flags&packetio.OffloadNeedsCsum != 0 {
		o.CsumStart = shift(o.CsumStart)
	}
	if o.HdrLen != 0 {
		o.HdrLen = shift(o.HdrLen)
	}
	return o
}

// spread lays one packet across several output slots, starting at out, and
// reports how many it used. It reports false when the packet does not fit in
// the slots or the frames available, having written nothing the caller will
// use: a packet is delivered whole or not at all, because a caller handed the
// first half of one cannot tell that is what it has.
//
// The packet is the frame with its VLAN tag put back, which is why the copy
// walks segments rather than one range: the tag sits between byte 12 and the
// rest, and a chain boundary can fall anywhere, including inside the tag.
// It reports noSlots when what ran out was output slots rather than frames.
// The difference decides what the caller does next: a batch with no room left
// will have room next time, but a packet needing more slots than the caller's
// whole batch will never fit one, and retrying it forever would stop the queue.
func (r *ring) spread(out, max int, d uint32, plen, tagLen int, tpid, tci uint16,
	dst func(i int) []byte, lens []int, cont []bool) (n int, ok, noSlots bool) {

	var tag [4]byte
	var segs [3][]byte
	nsegs := 0
	if tagLen != 0 {
		tag[0], tag[1] = byte(tpid>>8), byte(tpid)
		tag[2], tag[3] = byte(tci>>8), byte(tci)
		segs[0] = r.mem[d : d+12]
		segs[1] = tag[:]
		segs[2] = r.mem[d+12 : d+uint32(plen)]
		nsegs = 3
	} else {
		segs[0] = r.mem[d : d+uint32(plen)]
		nsegs = 1
	}

	seg, at := 0, 0
	for seg < nsegs {
		if out+n >= max {
			return 0, false, true
		}
		buf := dst(out + n)
		if buf == nil {
			return 0, false, false
		}
		room := len(buf)
		written := 0
		for seg < nsegs && written < room {
			c := copy(buf[written:], segs[seg][at:])
			written += c
			at += c
			if at == len(segs[seg]) {
				seg, at = seg+1, 0
			}
		}
		lens[out+n] = written
		n++
	}
	// Only now, with the whole packet placed, are the slots marked. Marking
	// as they were filled left the marks behind on the failure returns above,
	// and the next packet delivered into one of those slots -- by the
	// single-frame path, which writes lens and offs but not cont -- arrived
	// claiming to continue into a packet that was never delivered.
	if cont != nil {
		for i := out; i < out+n-1; i++ {
			cont[i] = true
		}
		cont[out+n-1] = false
	}
	return n, true, false
}
