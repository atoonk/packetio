//go:build linux

package afpacket

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/internal/pool"
	"golang.org/x/sys/unix"
)

// RxQueue receives from one socket's TPACKET_V3 ring.
//
// The ring is the buffer the kernel fills, so unlike the other backends there
// is nothing to hand it in advance: Fill reports how many frames are free to
// copy into, and Receive copies out of the ring into them. That copy is not
// the expensive part of this backend, and removing it would mean handing
// callers pointers into a block the kernel wants back, which is a much worse
// trade.
//
// One goroutine per queue. Nothing here is synchronised except the wake
// channel, which exists so Close can be called from another one.
type RxQueue struct {
	region *region
	fd     int
	ring   *ring
	pool   *pool.Frames

	// wake is an eventfd in the poll set. Closing the socket does not wake a
	// poll(2) already blocked on it, so without this a goroutine in Poll(-1)
	// could never be shut down.
	wake int

	// ringMu guards only the question "is the ring still mapped", so that
	// Close can unmap it while another goroutine sits in Poll. It is taken
	// once per Poll and once per wakeup, never per packet: Receive walks the
	// ring without it, and Receive is not safe to run during Close.
	ringMu sync.Mutex

	lens  []int
	descs []packetio.Desc
	offs  []packetio.Offload
	tss   []uint64
	addrs []uint64

	// The counters are atomics because Stats reads them from a monitoring
	// goroutine while the queue's own goroutine drives it -- and, for drops
	// and freeze, adds to them from whichever goroutine read the kernel's
	// destructive counters last. The owner adds once per batch.
	bytes   atomic.Uint64
	polls   atomic.Uint64
	batches atomic.Uint64
	empty   atomic.Uint64
	badCsum atomic.Uint64
	// drops accumulates PACKET_STATISTICS, whose read is destructive.
	drops  atomic.Uint64
	freeze atomic.Uint64

	closed atomic.Bool
}

func newRxQueue(r *region, fd int, rg *ring, firstFrame, frames int) (*RxQueue, error) {
	wake, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("afpacket: eventfd for the receive queue: %w", err)
	}
	return &RxQueue{
		region: r,
		fd:     fd,
		ring:   rg,
		wake:   wake,
		pool:   pool.New(firstFrame, frames, r.frameSize),
		lens:   make([]int, rxFramesPerBlock),
		descs:  make([]packetio.Desc, 0, rxFramesPerBlock),
		offs:   make([]packetio.Offload, rxFramesPerBlock),
		addrs:  make([]uint64, 0, 1),
	}, nil
}

// Region is the frame memory this queue receives into.
func (q *RxQueue) Region() packetio.Region { return q.region }

// frameHeadroom is where a packet starts within its frame -- received OR
// allocated: one cache line in, not zero. Two reasons, both measured. A
// forwarder that must prepend a longer link-layer header (a VLAN push) needs
// room before the packet, and a packet at offset zero has none. And on Zen,
// a transmit copy whose source and destination share a page offset (both
// frame stores and the kernel's skb slab are 2048-strided) serializes on
// 4 KiB-aliasing false dependencies -- measured at 933 versus 188 cycles for
// one 1500-byte packet -- and one line of offset moves the copy out of the
// window. Alloc takes the same offset as Receive, because a generated packet
// goes through the same sendmmsg as a forwarded one.
const frameHeadroom = 64

// Fill reports how many frames are free to receive into. The kernel owns the
// ring it fills, so there is nothing to post; this exists so that code written
// against the other backends works unchanged.
func (q *RxQueue) Fill(n int) int {
	if q.closed.Load() {
		return 0
	}
	return min(n, q.pool.Len())
}

// Poll waits for packets and returns how many are ready.
//
// A negative timeout waits indefinitely, zero returns at once, and a positive
// one waits that long. It returns [packetio.ErrClosed] if the queue is closed
// while it waits.
func (q *RxQueue) Poll(timeout time.Duration) (int, error) {
	if q.closed.Load() {
		return 0, packetio.ErrClosed
	}
	q.polls.Add(1)
	if n := q.ready(); n > 0 {
		return n, nil
	}
	if timeout == 0 {
		return 0, nil
	}

	// poll(2) on an AF_PACKET socket reports more than the ring: it reflects
	// the socket's ordinary receive queue too, and it always reports error
	// conditions whether or not they were asked for. A wakeup is therefore not
	// evidence of a packet -- re-check the ring and go back to sleep if there
	// is nothing there, rather than returning 0 and inviting the caller into a
	// busy loop.
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	fds := []unix.PollFd{
		{Fd: int32(q.fd), Events: unix.POLLIN},
		{Fd: int32(q.wake), Events: unix.POLLIN},
	}
	for {
		ms := -1
		if timeout > 0 {
			left := time.Until(deadline)
			if left <= 0 {
				return 0, nil
			}
			if ms = int(left.Milliseconds()); ms == 0 {
				ms = 1
			}
		}
		fds[0].Revents, fds[1].Revents = 0, 0
		n, err := unix.Poll(fds, ms)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("afpacket: poll: %w", err)
		}
		if q.closed.Load() || fds[1].Revents != 0 {
			return 0, packetio.ErrClosed
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return 0, fmt.Errorf("afpacket: poll reported 0x%x on the socket", fds[0].Revents)
		}
		if n == 0 {
			return 0, nil // timed out
		}
		if r := q.ready(); r > 0 {
			return r, nil
		}
		// Spurious: readable for some reason that is not a block of packets.
	}
}

// ready reports how many packets the current block holds, without consuming
// them. It takes ringMu because Poll's caller may be racing a Close.
func (q *RxQueue) ready() int {
	q.ringMu.Lock()
	defer q.ringMu.Unlock()
	if q.ring.mem == nil {
		return 0
	}
	if q.ring.pending {
		return int(q.ring.numPkts)
	}
	bd := q.ring.blockDesc(q.ring.block)
	if atomic.LoadUint32(&bd.blockStatus)&tpStatusUser == 0 {
		return 0
	}
	return int(bd.numPkts)
}

// Receive copies up to max packets out of the ring into free frames and returns
// descriptors naming them. The returned slice is reused by the next call.
func (q *RxQueue) Receive(max int) []packetio.Desc {
	d, _ := q.receive(max, false)
	return d
}

// ReceiveOffload is Receive, and also returns the segmentation and checksum
// metadata the kernel reported for each frame. Without [WithGSO] every Offload
// is the zero value, which means an ordinary frame.
//
// It implements [packetio.OffloadReceiver].
func (q *RxQueue) ReceiveOffload(max int) ([]packetio.Desc, []packetio.Offload) {
	return q.receive(max, false)
}

// ReceiveTimestamps is Receive, and also returns when the kernel stamped each
// frame, in nanoseconds.
//
// The clock is CLOCK_REALTIME: this backend does not offer PACKET_TIMESTAMP,
// so it is always the kernel's wall-clock reading as the frame went into the
// ring, not the wire time a NIC would record. Being the wall clock, it can
// step: an adjustment moves these stamps with it, so a long measurement should
// treat a large jump as the clock being set rather than as traffic.
//
// Offload metadata is not carried on this path. A device opened [WithGSO]
// whose caller needs both should use ReceiveOffload and read the times from
// its own clock, or open a second queue.
//
// It implements [packetio.TimestampReceiver].
func (q *RxQueue) ReceiveTimestamps(max int) ([]packetio.Desc, []uint64) {
	d, _ := q.receive(max, true)
	if d == nil {
		return nil, nil
	}
	return d, q.tss[:len(d)]
}

func (q *RxQueue) receive(max int, wantTimestamps bool) ([]packetio.Desc, []packetio.Offload) {
	if q.closed.Load() || max <= 0 {
		return nil, nil
	}
	// The old cap here -- blockSize/frameSize -- came from a TPACKET_V3 field
	// the kernel does not use as a delivery bound: V3 packs frames into a
	// block by BYTES, so a 64 KiB block holds ~40 full-size packets, not 32,
	// and hundreds of small ones. Capping at 32 split every block into two
	// deliveries and held the block from the kernel across the caller's whole
	// processing pass. The ring now drains across consecutive ready blocks up
	// to the caller's max, like every other backend.
	if cap(q.lens) < max {
		q.lens = make([]int, max)
	}
	if cap(q.offs) < max {
		q.offs = make([]packetio.Offload, max)
	}
	// Only a caller that asked pays for the walk to fill this; the ring skips
	// the read entirely when it is nil.
	var tss []uint64
	if wantTimestamps {
		if cap(q.tss) < max {
			q.tss = make([]uint64, max)
		}
		tss = q.tss[:max]
	}
	q.descs = q.descs[:0]

	// One frame is taken per output slot. read may call this more than once for
	// the same slot, when it skipped an oversized frame, so hand back the frame
	// already allocated for that slot rather than a fresh one -- otherwise the
	// descriptors and the lengths would drift out of step by one for every skip.
	got := q.ring.read(max, func(i int) []byte {
		if i < len(q.descs) {
			return q.region.Writable(q.descs[i])
		}
		q.addrs = q.pool.Pop(1, q.addrs[:0])
		if len(q.addrs) == 0 {
			return nil
		}
		q.descs = append(q.descs, packetio.Desc{Addr: q.addrs[0] + frameHeadroom})
		return q.region.Writable(q.descs[i])
	}, q.lens[:max], q.offs[:max], tss)

	// A frame may have been taken for a slot that produced nothing, when the
	// walk ended on an oversized frame. Give those back -- by frame start,
	// because a descriptor points frameHeadroom bytes into its frame and the
	// pool refuses an address that is not a frame boundary. Pushing the
	// descriptor address instead loses the frame for the life of the queue
	// and counts it in Rejected.
	for i := len(q.descs) - 1; i >= got; i-- {
		q.pool.Push(q.pool.Base(q.descs[i].Addr))
	}
	q.descs = q.descs[:got]
	var bytes uint64
	for i := range q.descs {
		q.descs[i].Len = uint32(q.lens[i])
		bytes += uint64(q.lens[i])
	}
	if got > 0 {
		// One atomic add per batch, not one per packet.
		q.batches.Add(1)
		q.bytes.Add(bytes)
	}
	if got == 0 && q.pool.Len() == 0 {
		q.empty.Add(1)
	}
	// A frame the kernel says carries a partial checksum has only the
	// pseudo-header sum in its checksum field. Finish it here, or everything
	// downstream sees a corrupt packet.
	// Without a vnet header the kernel never reports a partial checksum, and
	// this loop would still walk every descriptor to discover that; skip it
	// outright on the configuration nearly everyone runs.
	if !q.ring.vnet {
		return q.descs, q.offs[:got]
	}
	for i := range q.descs {
		o := &q.offs[i]
		if o.Flags&packetio.OffloadNeedsCsum == 0 {
			continue
		}
		if !completeL4(q.region.Frame(q.descs[i]), int(o.CsumStart), int(o.CsumOff)) {
			// The offsets did not fit the frame. The packet is delivered with
			// the partial still in it, which is wrong on the wire, so say so
			// rather than let it pass for correct.
			q.badCsum.Add(1)
		}
	}
	return q.descs, q.offs[:got]
}

// Fd is the receive socket's file descriptor, for a caller that must wait on
// several queues at once.
//
// Poll covers the ordinary case: one queue, its own wakeup on close. A caller
// driving several devices from one goroutine needs them all in a single
// poll(2) alongside its own wake descriptors, and cannot get that by calling
// Poll per queue. This is the afpacket twin of the afxdp backend's Socket().
//
// The descriptor belongs to the queue: poll it, do not read, close, or
// otherwise operate on it, and do not use it after Close.
func (q *RxQueue) Fd() int { return q.fd }

// Recycle returns received frames to the pool. A frame this pool does not own
// is refused there and counted, not silently accepted.
func (q *RxQueue) Recycle(descs []packetio.Desc) {
	for _, d := range descs {
		q.pool.Push(q.pool.Base(d.Addr))
	}
}

// NumFreeFillSlots is how many frames could be received into now. The kernel
// owns the ring, so a free frame is a free slot.
func (q *RxQueue) NumFreeFillSlots() int { return q.pool.Len() }

// NumFreeFrames is how many frames are in the free pool.
func (q *RxQueue) NumFreeFrames() int { return q.pool.Len() }

// NumReceived is at least how many packets Receive would return now: it counts
// the block the ring is on, and Receive drains across every block that is
// ready, so a busy queue often has more waiting than this reports.
func (q *RxQueue) NumReceived() int {
	if q.closed.Load() {
		return 0
	}
	return q.ready()
}

// Stats reports what this queue received, plus what the kernel dropped on its
// behalf. The kernel figure is the important one: it is the only evidence that
// a receiver fell behind, and it is invisible everywhere else.
func (q *RxQueue) Stats() (packetio.RxStats, error) {
	if !q.closed.Load() {
		q.readKernelStats()
	}
	return packetio.RxStats{
		Packets:   q.ring.packets.Load(),
		Batches:   q.batches.Load(),
		Bytes:     q.bytes.Load(),
		Polls:     q.polls.Load(),
		PoolEmpty: q.empty.Load(),
		Errors:    q.ring.oversize.Load() + q.badCsum.Load(),
		Dropped:   q.drops.Load(),
		Backend: map[string]uint64{
			// A block freeze is the signal that this reader is too slow: the
			// kernel had no block to fill and had to wait for one back.
			"freeze_q_count": q.freeze.Load(),
			"bad_checksum":   q.badCsum.Load(),
			"oversize":       q.ring.oversize.Load(),
			"pool_rejected":  q.pool.Rejected(),
		},
	}, nil
}

// tpacketStatsV3 is struct tpacket_stats_v3, read through PACKET_STATISTICS.
type tpacketStatsV3 struct {
	packets      uint32
	drops        uint32
	freezeQCount uint32
}

// readKernelStats adds this socket's counters to the running totals. The
// getsockopt resets the kernel's copy, which is why they are accumulated here
// rather than reported raw.
func (q *RxQueue) readKernelStats() {
	var st tpacketStatsV3
	size := uint32(unsafe.Sizeof(st))
	if _, _, e := unix.Syscall6(unix.SYS_GETSOCKOPT, uintptr(q.fd),
		uintptr(unix.SOL_PACKET), uintptr(unix.PACKET_STATISTICS),
		uintptr(unsafe.Pointer(&st)), uintptr(unsafe.Pointer(&size)), 0); e != 0 {
		return
	}
	q.drops.Add(uint64(st.drops))
	q.freeze.Add(uint64(st.freezeQCount))
}

// Close stops the queue, waking any goroutine blocked in Poll, and unmaps its
// ring. The socket belongs to the Device. It is safe to call twice.
func (q *RxQueue) Close() error {
	if q.closed.Swap(true) {
		return nil
	}
	// Take the counters one last time while the socket is still open.
	q.readKernelStats()
	// Wake the poller before anything is unmapped, so it sees closed rather
	// than a ring that has gone.
	var one [8]byte
	one[7] = 1
	unix.Write(q.wake, one[:])
	q.ringMu.Lock()
	err := q.ring.close()
	q.ringMu.Unlock()
	if e := unix.Close(q.wake); e != nil && err == nil {
		err = fmt.Errorf("afpacket: closing the wake eventfd: %w", e)
	}
	return err
}

// Err reports that the queue is out of service, or nil.
//
// A packet socket has no failure that outlives one call: a ring that fills is
// backpressure and its drops are counted, not a fault. So this is nil until
// the queue is closed.
func (q *RxQueue) Err() error {
	if q.closed.Load() {
		return packetio.ErrClosed
	}
	return nil
}

var (
	_ packetio.TimestampReceiver = (*RxQueue)(nil)
	_ packetio.RxQueue           = (*RxQueue)(nil)
	_ packetio.OffloadReceiver   = (*RxQueue)(nil)
)
