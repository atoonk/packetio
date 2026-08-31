//go:build linux && cgo && mlx5 && (amd64 || arm64)

package dv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSysfs builds the parts of /sys that Resolve reads.
type fakeSysfs struct{ root string }

func newFakeSysfs(t *testing.T) *fakeSysfs {
	t.Helper()
	f := &fakeSysfs{root: t.TempDir()}
	old := sysfs
	sysfs = f.root
	t.Cleanup(func() { sysfs = old })
	return f
}

// netdev creates an interface, optionally belonging to a verbs device.
func (f *fakeSysfs) netdev(t *testing.T, name string, ibdevs ...string) {
	t.Helper()
	dir := filepath.Join(f.root, "class", "net", name, "device", "infiniband")
	if len(ibdevs) == 0 {
		dir = filepath.Join(f.root, "class", "net", name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range ibdevs {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// port creates a port of a verbs device, carrying the named interfaces.
func (f *fakeSysfs) port(t *testing.T, ibdev string, port int, ifnames ...string) {
	t.Helper()
	dir := filepath.Join(f.root, "class", "infiniband", ibdev, "ports", itoa(port), "gid_attrs", "ndevs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, name := range ifnames {
		if err := os.WriteFile(filepath.Join(dir, itoa(i)), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// A two-port card presents one verbs device and two interfaces, so the device
// name alone does not say which port an interface is on.
func TestResolveTwoPortCard(t *testing.T) {
	f := newFakeSysfs(t)
	f.netdev(t, "ens1f0np0", "mlx5_0")
	f.netdev(t, "ens1f1np1", "mlx5_0")
	f.port(t, "mlx5_0", 1, "ens1f0np0")
	f.port(t, "mlx5_0", 2, "ens1f1np1")

	for _, tc := range []struct {
		ifname string
		port   uint32
	}{
		{"ens1f0np0", 1},
		{"ens1f1np1", 2},
	} {
		ibdev, port, err := Resolve(tc.ifname)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", tc.ifname, err)
		}
		if ibdev != "mlx5_0" || port != tc.port {
			t.Errorf("Resolve(%s) = %s port %d, want mlx5_0 port %d", tc.ifname, ibdev, port, tc.port)
		}
	}
}

// A single-port device whose sysfs does not say which interfaces a port carries
// still has only one possible answer.
func TestResolveSinglePortWithoutNdevs(t *testing.T) {
	f := newFakeSysfs(t)
	f.netdev(t, "eth0", "mlx5_2")
	f.port(t, "mlx5_2", 1) // no interfaces listed

	ibdev, port, err := Resolve("eth0")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ibdev != "mlx5_2" || port != 1 {
		t.Errorf("Resolve = %s port %d, want mlx5_2 port 1", ibdev, port)
	}
}

// A multi-port device that claims nothing must not be guessed at: opening the
// wrong port would transmit out of the wrong socket.
func TestResolveAmbiguousPortIsAnError(t *testing.T) {
	f := newFakeSysfs(t)
	f.netdev(t, "eth0", "mlx5_0")
	f.port(t, "mlx5_0", 1)
	f.port(t, "mlx5_0", 2)

	if _, _, err := Resolve("eth0"); err == nil {
		t.Fatal("an ambiguous port was resolved anyway")
	} else if !strings.Contains(err.Error(), "claims") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestResolveErrors(t *testing.T) {
	f := newFakeSysfs(t)
	f.netdev(t, "veth0") // exists, but has no verbs device

	t.Run("no such interface", func(t *testing.T) {
		_, _, err := Resolve("nope0")
		if err == nil || !strings.Contains(err.Error(), "no interface named") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("not an rdma interface", func(t *testing.T) {
		_, _, err := Resolve("veth0")
		if err == nil || !strings.Contains(err.Error(), "no verbs device") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("no interface named at all", func(t *testing.T) {
		if _, _, err := Resolve(""); err == nil {
			t.Fatal("the empty interface name was accepted")
		}
	})
}
