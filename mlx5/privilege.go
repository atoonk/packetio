//go:build linux && cgo && mlx5 && (amd64 || arm64)

package mlx5

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// capNetRaw is CAP_NET_RAW, the capability a process needs to open a raw
// Ethernet queue pair. Direct Verbs also wants CAP_IPC_LOCK to pin the frame
// region, but that failure already names the memory lock limit.
const capNetRaw = 13

// withPrivilegeHint appends what a caller needs, when the error looks like the
// kernel refused for want of privilege.
//
// libibverbs reports these as a bare strerror -- "Operation not permitted" from
// somewhere inside device setup -- which says nothing about what to do. It is
// the first thing anyone hits, so it is worth the two lines. The hint is only
// added when this process really does lack the capability, so that a genuine
// EPERM from something else is not decorated with advice that does not apply.
func withPrivilegeHint(err error) error {
	if err == nil || !looksLikePermission(err) || hasNetRaw() {
		return err
	}
	return fmt.Errorf("%w; this needs CAP_NET_RAW and CAP_IPC_LOCK, so run it as root "+
		"or grant them with 'sudo setcap cap_net_raw,cap_ipc_lock+ep <binary>'", err)
}

func looksLikePermission(err error) bool {
	s := err.Error()
	return strings.Contains(s, "Operation not permitted") ||
		strings.Contains(s, "Permission denied") ||
		strings.Contains(s, "No such device") && os.Geteuid() != 0
}

// hasNetRaw reports whether this process has CAP_NET_RAW in its effective set.
//
// Being root is not the question: a binary given the capability with setcap
// runs unprivileged and works, and telling that caller to use sudo would be
// wrong. Reading the effective set answers it exactly.
func hasNetRaw() bool {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		// No proc to ask, so fall back to the coarse question rather than
		// claiming a capability that may not be there.
		return os.Geteuid() == 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "CapEff:") {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "CapEff:")), 16, 64)
		if err != nil {
			return os.Geteuid() == 0
		}
		return v&(1<<capNetRaw) != 0
	}
	return os.Geteuid() == 0
}
