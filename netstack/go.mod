module github.com/atoonk/packetio/netstack

go 1.26.0

require (
	github.com/atoonk/go-afxdp v0.12.0
	github.com/atoonk/packetio v0.1.6
	github.com/google/btree v1.1.2
	golang.org/x/exp v0.0.0-20250711185948-6ae5c78190dc
	golang.org/x/sys v0.43.0
	golang.org/x/time v0.15.0
)

require (
	github.com/cilium/ebpf v0.16.0 // indirect
	github.com/vishvananda/netlink v1.3.1 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
)

replace github.com/atoonk/packetio => ../
