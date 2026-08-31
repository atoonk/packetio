//go:build linux && cgo && mlx5 && (amd64 || arm64)

package dv

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// sysfs is the root the lookups below are relative to. Tests point it at a
// directory they built themselves.
var sysfs = "/sys"

// Resolve maps a network interface to the verbs device and port that carry it.
//
// An mlx5 network interface and its verbs device are two views of the same
// hardware, and the kernel records the link in two places: the interface's
// device directory lists the verbs devices it belongs to, and each verbs port
// lists the interfaces it carries. Both are needed. A card with two ports
// presents one verbs device and two network interfaces, so the device name
// alone does not say which port a given interface is.
func Resolve(ifname string) (ibdev string, port uint32, err error) {
	if ifname == "" {
		return "", 0, fmt.Errorf("mlx5: no interface named")
	}

	dir := filepath.Join(sysfs, "class", "net", ifname, "device", "infiniband")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// Either the interface does not exist, or it is not backed by an
			// RDMA device: a virtual interface, or a card whose driver does
			// not provide one.
			if _, e := os.Stat(filepath.Join(sysfs, "class", "net", ifname)); e != nil {
				return "", 0, fmt.Errorf("mlx5: no interface named %s", ifname)
			}
			return "", 0, fmt.Errorf("mlx5: %s has no verbs device; is it an mlx5 interface with the mlx5_ib module loaded?", ifname)
		}
		return "", 0, fmt.Errorf("mlx5: reading %s: %w", dir, err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "", 0, fmt.Errorf("mlx5: %s lists no verbs device", ifname)
	}
	if len(names) > 1 {
		return "", 0, fmt.Errorf("mlx5: %s lists several verbs devices (%s), which is not expected", ifname, strings.Join(names, ", "))
	}
	ibdev = names[0]

	port, err = resolvePort(ibdev, ifname)
	if err != nil {
		return "", 0, err
	}
	return ibdev, port, nil
}

// resolvePort finds which port of a verbs device carries an interface, by
// asking each port which interfaces it has.
func resolvePort(ibdev, ifname string) (uint32, error) {
	portsDir := filepath.Join(sysfs, "class", "infiniband", ibdev, "ports")
	entries, err := os.ReadDir(portsDir)
	if err != nil {
		return 0, fmt.Errorf("mlx5: reading %s: %w", portsDir, err)
	}

	var ports []uint32
	for _, e := range entries {
		n, err := strconv.ParseUint(e.Name(), 10, 32)
		if err != nil {
			continue
		}
		ports = append(ports, uint32(n))

		ndevs := filepath.Join(portsDir, e.Name(), "gid_attrs", "ndevs")
		files, err := os.ReadDir(ndevs)
		if err != nil {
			continue // a port may not expose this; fall back below
		}
		for _, f := range files {
			b, err := os.ReadFile(filepath.Join(ndevs, f.Name()))
			if err != nil {
				continue
			}
			if strings.TrimSpace(string(b)) == ifname {
				return uint32(n), nil
			}
		}
	}

	// Nothing claimed the interface. On a single-port device there is only one
	// answer it could be, which is worth taking rather than refusing to open a
	// card whose sysfs is arranged differently.
	if len(ports) == 1 {
		return ports[0], nil
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	return 0, fmt.Errorf("mlx5: none of the %d ports of %s claims %s", len(ports), ibdev, ifname)
}
