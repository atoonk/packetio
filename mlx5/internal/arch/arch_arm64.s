#include "textflag.h"

// ARM64 needs every barrier written out. The outer-shareable domain is the one
// that includes the device, which is why these are the OSH variants rather than
// the inner-shareable ones a purely CPU-side barrier would use.
//
// Barrier option encodings: OSHLD is 1, OSHST is 2, OSH is 3, ST is 14.

// func PublishDoorbell(dbrec *uint32, dbval uint32, uar unsafe.Pointer, doorbell uint64)
TEXT ·PublishDoorbell(SB), NOSPLIT, $0-32
	MOVD	dbrec+0(FP), R0
	MOVWU	dbval+8(FP), R1
	DMB	$2			// udma_to_device_barrier: the entries before the index
	MOVW	R1, (R0)		// publish the producer index
	DSB	$14			// mmio_wc_start: dsb st, as rdma-core does
	MOVD	uar+16(FP), R2
	MOVD	doorbell+24(FP), R3
	MOVD	R3, (R2)		// the doorbell
	DSB	$14			// mmio_flush_writes: dsb st
	RET

// func PublishDbrec(dbrec *uint32, v uint32)
TEXT ·PublishDbrec(SB), NOSPLIT, $0-12
	MOVD	dbrec+0(FP), R0
	MOVWU	v+8(FP), R1
	DMB	$2			// udma_to_device_barrier
	MOVW	R1, (R0)
	RET

// func ReleaseCQ(dbrec *uint32, v uint32)
TEXT ·ReleaseCQ(SB), NOSPLIT, $0-12
	MOVD	dbrec+0(FP), R0
	MOVWU	v+8(FP), R1
	DMB	$3			// full outer-shareable: the reads being released, then the index
	MOVW	R1, (R0)
	RET

// func LoadCQEHeaderRaw(p unsafe.Pointer) uint32
TEXT ·LoadCQEHeaderRaw(SB), NOSPLIT, $0-12
	MOVD	p+0(FP), R0
	MOVWU	(R0), R1
	REVW	R1, R1			// the field is big-endian on the wire
	MOVW	R1, ret+8(FP)
	RET

// func FromDeviceBarrier()
TEXT ·FromDeviceBarrier(SB), NOSPLIT, $0-0
	DMB	$1			// udma_from_device_barrier
	RET

// func LoadCQEHeader(p unsafe.Pointer) uint32
TEXT ·LoadCQEHeader(SB), NOSPLIT, $0-12
	MOVD	p+0(FP), R0
	MOVWU	(R0), R1
	REVW	R1, R1			// the field is big-endian on the wire
	DMB	$1			// udma_from_device_barrier
	MOVW	R1, ret+8(FP)
	RET

// func Prefetch(p unsafe.Pointer)
TEXT ·Prefetch(SB), NOSPLIT, $0-8
	MOVD	p+0(FP), R0
	PRFM	(R0), PLDL1KEEP
	RET
