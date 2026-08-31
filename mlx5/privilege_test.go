//go:build linux && cgo && mlx5 && (amd64 || arm64)

package mlx5

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// The hint is added only when this process actually lacks the capability, so
// that a root run does not get told to use sudo.
func TestPrivilegeHint(t *testing.T) {
	err := errors.New("ibv_create_qp_ex for the inline probe: Operation not permitted")
	got := withPrivilegeHint(err)
	if hasNetRaw() {
		if got.Error() != err.Error() {
			t.Errorf("a privileged process was given a hint: %v", got)
		}
		return
	}
	if !strings.Contains(got.Error(), "CAP_NET_RAW") {
		t.Errorf("an unprivileged process was not told what it needs: %v", got)
	}
	if !errors.Is(got, err) {
		t.Error("the hint did not wrap the original error")
	}
}

// An error that is not about permission is left alone.
func TestPrivilegeHintLeavesOtherErrorsAlone(t *testing.T) {
	err := errors.New("mlx5: a transmit queue of 1000, which must be a power of two")
	if got := withPrivilegeHint(err); got.Error() != err.Error() {
		t.Errorf("decorated an unrelated error: %v", got)
	}
}

// hasNetRaw must agree with being root, where being root is the answer.
func TestHasNetRawAgreesWithRoot(t *testing.T) {
	if os.Geteuid() == 0 && !hasNetRaw() {
		t.Error("running as root but hasNetRaw says otherwise")
	}
}
