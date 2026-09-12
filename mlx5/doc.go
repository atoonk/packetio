// Package mlx5 moves Ethernet frames through an NVIDIA ConnectX or BlueField
// NIC using mlx5 Direct Verbs.
//
// Opening a device asks libibverbs and libmlx5 for a raw Ethernet queue pair
// and then asks where the memory they allocated for it lives. From that point
// the packet path is Go over that memory: writing work queue entries, reading
// completions, and one eight-byte write to a device register per batch. There
// is no syscall, no library call and no cgo call per packet or per batch.
//
// A queue is owned by one goroutine. Two goroutines may drive two queues of the
// same device; two goroutines may not drive one queue.
//
// # Building
//
// The package is behind the mlx5 build tag, which keeps cgo and rdma-core out
// of everyone else's build:
//
//	go build -tags mlx5 ./...
//
// Without the tag the package has no declarations, so pkg.go.dev shows this
// text and nothing else; go doc has no way to be told about a tag. The API is
// described in https://github.com/atoonk/packetio/tree/main/mlx5 and, built
// with the tag, is what any editor or go list -tags mlx5 will show.
//
// Building needs rdma-core's headers: apt install libibverbs-dev on Ubuntu
// 24.04, which brings the libibverbs and libmlx5 link targets. The backend
// links -libverbs -lmlx5 and nothing else. Running a binary built elsewhere
// needs apt install libibverbs1 ibverbs-providers.
//
// Opening a device needs CAP_NET_RAW, a memory lock limit big enough for the
// frame region (ulimit -l, or root), and /dev/infiniband present, which means
// the mlx5_ib module is loaded.
package mlx5
