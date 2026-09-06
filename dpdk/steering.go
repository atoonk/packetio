//go:build linux && cgo && dpdk && amd64

package dpdk

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/atoonk/packetio"
	"github.com/atoonk/packetio/dpdk/internal/eal"
)

// resolve turns what the caller named into something the environment
// understands: a PCI address, or a virtual device specification.
//
// A network interface is resolved through sysfs to the PCI address behind it,
// and then the driver bound to it decides whether this can go any further. A
// device still held by a kernel driver is refused, with the command that would
// hand it over -- this library never rebinds anything itself, because doing so
// to the wrong interface takes a machine off the network.
func resolve(name string) (string, error) {
	switch {
	case name == "":
		return "", fmt.Errorf("dpdk: no device named")
	case strings.HasPrefix(name, "net_"):
		return name, nil // a virtual device, with or without arguments
	case eal.IsPCI(name):
		addr, args, _ := strings.Cut(name, ",")
		if !safePCIAddr(addr) {
			return "", fmt.Errorf("dpdk: %q is not a PCI address", addr)
		}
		if err := checkDriver(addr, addr); err != nil {
			return "", err
		}
		if args != "" {
			return name, nil // keep the caller's driver arguments
		}
		return addr, nil
	}

	if !safeIfname(name) {
		return "", fmt.Errorf("dpdk: %q is not an interface name", name)
	}
	link, err := os.Readlink(filepath.Join("/sys/class/net", name, "device"))
	if err != nil {
		if _, e := os.Stat(filepath.Join("/sys/class/net", name)); e != nil {
			return "", fmt.Errorf("dpdk: no interface named %s", name)
		}
		return "", fmt.Errorf("dpdk: %s has no PCI device behind it; a virtual interface "+
			"cannot be opened this way", name)
	}
	addr := filepath.Base(link)
	if !safePCIAddr(addr) {
		// A USB or platform NIC: the kernel gives it a device but not a PCI
		// address, and nothing below this line would mean anything for it.
		return "", fmt.Errorf("dpdk: %s is not a PCI device (the kernel calls it %q), and "+
			"this backend can only open PCI devices and virtual ones", name, addr)
	}

	if err := checkDriver(addr, name); err != nil {
		return "", err
	}
	return addr, nil
}

// checkDriver refuses a device held by a kernel driver this backend cannot
// share, and says what to run. addr is the PCI address; named is what the
// caller asked for, which is either the same address or an interface name.
//
// Both forms of Open come through here. They did not always: the PCI form
// returned before this check, so the caller who followed the documentation --
// which uses PCI addresses throughout -- got the driver's bare "Operation not
// supported" instead of the command that fixes it.
func checkDriver(addr, named string) error {
	drv, err := os.Readlink(filepath.Join("/sys/bus/pci/devices", addr, "driver"))
	if err != nil {
		return nil // nothing bound: the device is free
	}
	switch d := filepath.Base(drv); d {
	case "vfio-pci", "uio_pci_generic", "igb_uio":
		return nil
	case "mlx5_core", "mlx4_core":
		// A bifurcated driver: the kernel keeps the interface and DPDK gets
		// its own queues on the same port. Nothing to rebind.
		return nil
	default:
		what := named
		if named == addr {
			// Name the interface too, so it is clear what goes off the
			// network -- that is the part worth knowing before running this.
			if ifname := ifnameOf(addr); ifname != "" {
				what = ifname
			}
		}
		return fmt.Errorf("dpdk: %s (%s) is bound to the %s driver, which this backend "+
			"cannot share. Hand the device over first:\n"+
			"    sudo dpdk-devbind.py --bind=vfio-pci %s\n"+
			"and note that this takes %s off the network until it is bound back",
			named, addr, d, addr, what)
	}
}

// ifnameOf is the kernel's interface name for a PCI address, or "".
func ifnameOf(addr string) string {
	entries, err := os.ReadDir(filepath.Join("/sys/bus/pci/devices", addr, "net"))
	if err != nil || len(entries) == 0 {
		return ""
	}
	return entries[0].Name()
}

// safePCIAddr reports whether addr is a PCI address in the form the kernel
// uses, so that it can be joined into a /sys path. Without this a name
// containing path separators would be read from wherever it pointed.
func safePCIAddr(addr string) bool {
	if len(addr) > 32 {
		return false
	}
	return pciAddr.MatchString(addr)
}

var pciAddr = regexp.MustCompile(`^([0-9a-fA-F]{4}:)?[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)

// isMellanox reports whether a PCI address names a Mellanox/NVIDIA card, read
// from sysfs before DPDK is asked about it. The vendor id 0x15b3 is theirs.
func isMellanox(devargs string) bool {
	addr := devargs
	if i := strings.IndexByte(addr, ','); i >= 0 {
		addr = addr[:i]
	}
	if !safePCIAddr(addr) {
		return false
	}
	b, err := os.ReadFile(filepath.Join("/sys/bus/pci/devices", addr, "vendor"))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == "0x15b3"
}

// safeIfname reports whether name can be joined into a /sys path without
// escaping it. filepath.Join cleans but does not confine.
func safeIfname(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	return !strings.ContainsAny(name, "/\x00") && name != "." && name != ".."
}

// open finds the port the device already became, or brings it up.
//
// The order matters. The environment brings the first device up itself, from
// its own arguments, so probing it again fails -- and the failure is logged by
// the EAL, where it then decorates the next unrelated error. Looking first
// costs nothing and keeps the log honest.
func open(devargs string) (eal.Port, error) {
	if port, ok := eal.PortOf(devargs); ok {
		return port, nil
	}
	return eal.Probe(devargs)
}

// steeringRules turns the filter into the rules the device installs: one per
// packetio.Rule, each a conjunction, a packet matching any of them arriving.
func (d *Device) steeringRules(ifname string) ([]eal.Match, error) {
	f := d.cfg.steering
	if f.Promiscuous || d.cfg.promisc {
		if err := d.setPromiscuous(); err != nil {
			return nil, err
		}
	}
	if f.Promiscuous || len(f.Match) == 0 {
		// Nothing to match: the port takes everything it is given.
		if !f.Promiscuous && !d.cfg.promisc {
			d.info.Steering = d.describeDefault()
		}
		return nil, nil
	}

	rules, err := f.Rules()
	if err != nil {
		return nil, fmt.Errorf("dpdk: %w", err)
	}
	// A rule that names no destination address takes packets addressed to this
	// port, which is what the rest of the network already sends here.
	var base eal.Match
	base.VLAN = -1
	if !namesDstMAC(f) {
		ifi, err := net.InterfaceByName(ifname)
		if err == nil && len(ifi.HardwareAddr) == 6 {
			copy(base.MAC[:], ifi.HardwareAddr)
			base.MACSet = true
		} else if d.info.MAC != [6]byte{} {
			base.MAC, base.MACSet = d.info.MAC, true
		}
	}

	out := make([]eal.Match, 0, len(rules))
	for _, r := range rules {
		m := base
		if r.MACSet {
			m.MAC, m.MACSet = r.MAC, true
		}
		if r.VLANSet {
			m.VLAN = int(r.VLAN)
		}
		if r.EtherType != 0 {
			m.EtherType = r.EtherType
		}
		if r.IPProtoSet {
			m.IPProto, m.ProtoSet = r.IPProto, true
		}
		for _, pre := range []struct {
			p        netip.Prefix
			ip, mask *[4]byte
		}{{r.SrcPrefix, &m.SrcIP, &m.SrcMask}, {r.DstPrefix, &m.DstIP, &m.DstMask}} {
			if !pre.p.IsValid() {
				continue
			}
			if !pre.p.Addr().Is4() {
				return nil, fmt.Errorf("%w: this backend's steering matches IPv4 addresses, "+
					"not %s", packetio.ErrUnsupported, pre.p)
			}
			*pre.ip, *pre.mask = prefixBytes(pre.p)
		}
		if r.SrcPortSet {
			m.SrcPort, m.SrcSet = r.SrcPort, true
		}
		if r.DstPortSet {
			m.DstPort, m.DstSet = r.DstPort, true
		}
		out = append(out, m)
	}
	d.info.Steering = describeRules(out)
	return out, nil
}

// setPromiscuous asks the port for every packet it sees, and records what it
// got.
//
// A driver with no promiscuous mode at all is not a refusal on a device this
// process owns outright: the port's own address filter is then the only thing
// in the way, there is nothing behind it to widen the filter for, and the
// queues get exactly what a promiscuous port would have got. The ENA on EC2 is
// such a driver. On a device shared with the kernel
// promiscuous means taking the kernel's traffic too, which a driver without
// the mode genuinely cannot do, so there it stays an error.
func (d *Device) setPromiscuous() error {
	err := eal.Promiscuous(d.port, true)
	switch {
	case err == nil:
		d.info.Promiscuous = true
		d.info.Steering = "every packet the port sees"
	case errors.Is(err, eal.ErrNoPromiscuous) && !d.info.Coexists:
		d.info.Steering = "whatever the port's own address filter admits " +
			"(this driver has no promiscuous mode to widen it)"
	default:
		return fmt.Errorf("%w: %v", packetio.ErrUnsupported, err)
	}
	return nil
}

func (d *Device) describeDefault() string {
	if d.cfg.rxQueues == 0 {
		return "nothing: this device does not receive"
	}
	return "whatever the port's own address filter admits"
}

func namesDstMAC(f packetio.SteeringFilter) bool {
	for _, m := range f.Match {
		if m.Kind == packetio.MatchKindDstMAC {
			return true
		}
	}
	return false
}

// prefixBytes splits a prefix into the address and the mask, both in wire order.
func prefixBytes(p netip.Prefix) (addr, mask [4]byte) {
	addr = p.Masked().Addr().As4()
	bits := p.Bits()
	for i := 0; i < 4; i++ {
		n := bits - i*8
		switch {
		case n >= 8:
			mask[i] = 0xff
		case n > 0:
			mask[i] = ^byte(0) << (8 - n)
		}
	}
	return addr, mask
}

// describeRules renders every rule installed, not just the first: this line is
// how a user checks the device got what they meant.
func describeRules(rules []eal.Match) string {
	if len(rules) == 0 {
		return "nothing"
	}
	parts := make([]string, 0, len(rules))
	for _, r := range rules {
		parts = append(parts, describeMatch(r))
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts, ", or ")
}

func describeMatch(m eal.Match) string {
	var parts []string
	if m.MACSet {
		parts = append(parts, "to "+net.HardwareAddr(m.MAC[:]).String())
	}
	if m.VLAN >= 0 {
		parts = append(parts, fmt.Sprintf("vlan %d", m.VLAN))
	}
	if m.EtherType != 0 {
		parts = append(parts, fmt.Sprintf("ethertype 0x%04x", m.EtherType))
	}
	if m.SrcMask != ([4]byte{}) {
		parts = append(parts, "from "+ipMask(m.SrcIP, m.SrcMask))
	}
	if m.DstMask != ([4]byte{}) {
		parts = append(parts, "to "+ipMask(m.DstIP, m.DstMask))
	}
	if m.ProtoSet {
		parts = append(parts, "proto "+protoName(m.IPProto))
	}
	if m.SrcSet {
		parts = append(parts, fmt.Sprintf("src port %d", m.SrcPort))
	}
	if m.DstSet {
		parts = append(parts, fmt.Sprintf("port %d", m.DstPort))
	}
	if len(parts) == 0 {
		return "every packet the port sees"
	}
	return "packets " + strings.Join(parts, " ")
}

func ipMask(ip, mask [4]byte) string {
	bits := 0
	for _, b := range mask {
		for ; b&0x80 != 0; b <<= 1 {
			bits++
		}
	}
	return fmt.Sprintf("%d.%d.%d.%d/%d", ip[0], ip[1], ip[2], ip[3], bits)
}

func protoName(p uint8) string {
	switch p {
	case 6:
		return "tcp"
	case 17:
		return "udp"
	}
	return fmt.Sprintf("ip/%d", p)
}
