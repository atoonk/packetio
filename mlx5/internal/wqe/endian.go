package wqe

import "encoding/binary"

// Small wrappers so the byte order of every field is stated where it is
// written, rather than left to the reader to infer.

func beU16(b []byte, v uint16) { binary.BigEndian.PutUint16(b, v) }
func beU32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }
func beU64(b []byte, v uint64) { binary.BigEndian.PutUint64(b, v) }

func getBEU16(b []byte) uint16 { return binary.BigEndian.Uint16(b) }
func getBEU32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }
func getBEU64(b []byte) uint64 { return binary.BigEndian.Uint64(b) }

// nativeU64 reads eight bytes in host order, so that storing the result again
// reproduces those bytes exactly. It is for handing a chunk of a work queue
// entry to the device doorbell, which takes the bytes as they lie in memory:
// rdma-core does the same with *(__be64 *)ctrl.
func nativeU64(b []byte) uint64 { return binary.NativeEndian.Uint64(b) }
