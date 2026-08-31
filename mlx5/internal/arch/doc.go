// Package arch holds the memory-ordering and memory-mapped I/O primitives the
// mlx5 packet path needs.
//
// Everything a driver shares with a NIC falls into one of four kinds of memory,
// and the rules differ for each:
//
//   - Ordinary memory. Frame pools, counters, scratch slices. Only the CPU
//     touches it and the Go memory model covers it.
//
//   - DMA-visible queue memory. Packet frames, send and receive work queue
//     entries. The CPU writes it and the NIC reads it, or the NIC writes it and
//     the CPU reads it. Writes to it must be complete before the doorbell that
//     tells the NIC to look, and reads of it must not be hoisted above the
//     ownership check that says the NIC is finished writing.
//
//   - Doorbell records. Two 32-bit words of ordinary host memory per queue that
//     the NIC reads by DMA to learn a producer or consumer index. Publishing
//     one is an ordinary store, ordered after the queue writes it refers to.
//
//   - The user access region. A page of the device's memory-mapped registers,
//     usually mapped write-combining. Writing eight bytes there is what
//     actually wakes the NIC. Write-combining stores need explicit fences
//     around them, and they must never be done with a Go atomic operation: a
//     read-modify-write instruction against device memory is not a doorbell.
//
// The sequences here mirror rdma-core's providers/mlx5 exactly, so they can be
// checked against it line by line: post_send_db in qp.c for the transmit
// doorbell, and the udma_to_device_barrier, mmio_wc_start and mmio_flush_writes
// macros in util/udma_barrier.h for the fences it uses.
//
// Correctness here does not rest on any assumption about what the Go compiler
// will or will not reorder. Every device-visible publication and every read of
// device-written control data goes through one of these functions, which are
// assembly and therefore opaque: the compiler cannot move a memory access
// across the call, and the fences inside are the ones the architecture
// requires.
package arch
