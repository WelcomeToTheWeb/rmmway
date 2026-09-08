//go:build linux

package collectors

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
)

// defaultServiceSampler (linux) asks systemd for the unit's ActiveState.
// Exec-based on purpose: gopsutil v4 ships no cross-OS service package, and
// a dbus/go-systemd dependency would threaten the agent's static-binary
// property. A host without systemctl, or a unit systemd does not know,
// yields no sample (ErrServiceUnknown), not a probe error.
func defaultServiceSampler(ctx context.Context, name string) (float64, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "show", "-p", "ActiveState", "--", name).Output()
	if err != nil {
		// Non-zero exit (unit unknown on some systemd versions) or the
		// binary itself missing: both read as "not monitorable here".
		return 0, ErrServiceUnknown
	}
	state := strings.TrimSpace(string(bytes.TrimPrefix(out, []byte("ActiveState="))))
	switch state {
	case "active", "activating":
		return 1, nil
	case "inactive", "failed", "deactivating":
		return 0, nil
	default: // empty (unit absent) or a state we do not interpret
		return 0, ErrServiceUnknown
	}
}
