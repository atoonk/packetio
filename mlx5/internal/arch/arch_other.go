//go:build !amd64 && !arm64

package arch

import "unsafe"

// The mlx5 backend runs on amd64 and arm64. These stubs exist so that the
// module still builds elsewhere; calling one is a programming error, not a
// runtime condition to handle, because nothing should have opened a device.

func PublishDoorbell(dbrec *uint32, dbval uint32, uar unsafe.Pointer, doorbell uint64) {
	panic("packetio/mlx5: no doorbell implementation for this architecture")
}

func PublishDbrec(dbrec *uint32, v uint32) {
	panic("packetio/mlx5: no doorbell implementation for this architecture")
}

func ReleaseCQ(dbrec *uint32, v uint32) {
	panic("packetio/mlx5: no doorbell implementation for this architecture")
}

func LoadCQEHeader(p unsafe.Pointer) uint32 {
	panic("packetio/mlx5: no doorbell implementation for this architecture")
}

func LoadCQEHeaderRaw(p unsafe.Pointer) uint32 {
	panic("packetio/mlx5: no doorbell implementation for this architecture")
}

func FromDeviceBarrier() {
	panic("packetio/mlx5: no doorbell implementation for this architecture")
}

// Prefetch is a hint, and no hint is a correct implementation of a hint.
func Prefetch(p unsafe.Pointer) {}
