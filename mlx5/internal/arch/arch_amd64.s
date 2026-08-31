#include "textflag.h"

// On x86-64, stores to ordinary memory reach the device in program order, so
// rdma-core's udma_to_device_barrier is nothing but a compiler barrier here;
// the call itself provides that. Write-combining stores to the user access
// region are the exception and need the fences spelled out.

// func PublishDoorbell(dbrec *uint32, dbval uint32, uar unsafe.Pointer, doorbell uint64)
TEXT ·PublishDoorbell(SB), NOSPLIT, $0-32
	MOVQ	dbrec+0(FP), DI
	MOVL	dbval+8(FP), AX
	MOVL	AX, (DI)		// publish the producer index
	SFENCE				// mmio_wc_start: order it ahead of the register write
	MOVQ	uar+16(FP), DI
	MOVQ	doorbell+24(FP), AX
	MOVQ	AX, (DI)		// the doorbell: one eight-byte write-combining store
	SFENCE				// mmio_flush_writes: do not let it linger in a WC buffer
	RET

// func PublishDbrec(dbrec *uint32, v uint32)
TEXT ·PublishDbrec(SB), NOSPLIT, $0-12
	MOVQ	dbrec+0(FP), DI
	MOVL	v+8(FP), AX
	MOVL	AX, (DI)
	RET

// func ReleaseCQ(dbrec *uint32, v uint32)
TEXT ·ReleaseCQ(SB), NOSPLIT, $0-12
	MOVQ	dbrec+0(FP), DI
	MOVL	v+8(FP), AX
	MOVL	AX, (DI)		// x86-64 does not reorder loads after later stores
	RET

// func LoadCQEHeaderRaw(p unsafe.Pointer) uint32
TEXT ·LoadCQEHeaderRaw(SB), NOSPLIT, $0-12
	MOVQ	p+0(FP), DI
	MOVL	(DI), AX
	BSWAPL	AX			// the field is big-endian on the wire
	MOVL	AX, ret+8(FP)
	RET

// func FromDeviceBarrier()
TEXT ·FromDeviceBarrier(SB), NOSPLIT, $0-0
	LFENCE				// udma_from_device_barrier
	RET

// func LoadCQEHeader(p unsafe.Pointer) uint32
//
// The LFENCE is what rdma-core's udma_from_device_barrier does here, and it
// stays. It was removed once, on the argument that x86-64 does not reorder
// loads with other loads and DPDK's mlx5 driver omits it -- which is true, and
// it bought nothing: 122.7 cycles a packet against 123.0 with it, inside the
// run-to-run spread. Weakening a barrier on the packet path for no measurable
// gain is the wrong side of that trade, so this follows rdma-core.
//
// The fence is not merely conservative on arm64, where the ownership test
// orders the loads after it only by a control dependency. See arch_arm64.s.
TEXT ·LoadCQEHeader(SB), NOSPLIT, $0-12
	MOVQ	p+0(FP), DI
	MOVL	(DI), AX
	BSWAPL	AX			// the field is big-endian on the wire
	LFENCE				// udma_from_device_barrier
	MOVL	AX, ret+8(FP)
	RET

// func Prefetch(p unsafe.Pointer)
TEXT ·Prefetch(SB), NOSPLIT, $0-8
	MOVQ	p+0(FP), DI
	PREFETCHT0	(DI)
	RET
