//go:build amd64 || arm64

package arch

import "unsafe"

// PublishDoorbell hands a batch of work queue entries to the NIC.
//
// It performs, in order: the barrier that makes the entries visible to the
// device, the store of dbval to the queue's doorbell record, the fence that
// keeps that store ahead of what follows, the eight-byte write of doorbell to
// the user access region at uar, and the fence that pushes it out of the CPU's
// write-combining buffers.
//
// dbval is the producer index in the byte order the device expects, big-endian,
// and doorbell is the first eight bytes of the last work queue entry read as
// they lie in memory.
//
//go:noescape
func PublishDoorbell(dbrec *uint32, dbval uint32, uar unsafe.Pointer, doorbell uint64)

// PublishDbrec stores an index into a doorbell record, after the barrier that
// makes everything written before it visible to the device.
//
// This is how a receive queue publishes newly posted buffers: unlike a send
// queue there is no register to write, because the NIC reads the record itself
// when it needs work.
//
//go:noescape
func PublishDbrec(dbrec *uint32, v uint32)

// ReleaseCQ stores a consumer index into a completion queue's doorbell record,
// after the barrier that keeps every read of the entries being released ahead
// of it.
//
// The ordering matters: the index tells the NIC those entries may be
// overwritten, so it must not become visible while software is still reading
// them. On x86-64 that is free, because a load is never reordered past a later
// store, which is the same reasoning rdma-core's bare store relies on. On ARM
// it takes a full barrier, which is what separates this from PublishDbrec, and
// which costs one fence per polling round rather than per packet.
//
//go:noescape
func ReleaseCQ(dbrec *uint32, v uint32)

// LoadCQEHeader reads the last four bytes of a completion queue entry, the word
// holding the work queue entry counter, the signature and the opcode-and-owner
// byte, and returns it big-endian: counter in the high half, opcode and owner
// in the low byte.
//
// The barrier it performs afterwards is what makes the rest of the entry safe
// to read. The owner bit in this word is the hardware's statement that it has
// finished writing the entry; without the barrier, a later read of the entry's
// body could be satisfied by a value fetched before that statement was true.
//
//go:noescape
func LoadCQEHeader(p unsafe.Pointer) uint32

// LoadCQEHeaderRaw reads the same word without the barrier.
//
// It is for code that reads the header word and nothing else: whether an entry
// is owned, its format, its opcode, its work queue counter. A poll that only
// counts what is ready costs no fence at all that way.
//
// It is NOT for the ownership test in front of reading an entry's body. The
// rule is per entry -- the check saying THIS entry is ready must be ordered
// before THIS entry is read -- and one fence covering a run of entries does not
// give that on a weakly ordered machine, where the ownership test orders the
// loads after it only by a control dependency. Use LoadCQEHeader there.
//
// This is still an opaque call, so the compiler cannot hold the word in a
// register across iterations and miss the hardware's update.
//
//go:noescape
func LoadCQEHeaderRaw(p unsafe.Pointer) uint32

// FromDeviceBarrier makes everything the device wrote before the word just read
// visible to loads that follow. It is rdma-core's udma_from_device_barrier.
func FromDeviceBarrier()

// Prefetch hints the cache line at p into every level of the cache. It is
// advice, not a load: it cannot fault, and on a machine with no prefetch
// instruction doing nothing is a correct implementation.
//
// The receive ring issues it for each frame it is about to hand out. The
// frames were written by the NIC's DMA engine, and on machines whose inbound
// DMA does not land in any cache (EPYC among them) the caller's first read of
// a frame otherwise pays the full memory latency, packet after packet;
// hinting them one per completion as the ring walks its queue spaces the
// misses so they overlap instead. Measured on a ConnectX-6 Dx forwarding
// 64-byte frames on 8 cores: 70.6 Mpps without it, 139.3 with.
//
//go:noescape
func Prefetch(p unsafe.Pointer)
