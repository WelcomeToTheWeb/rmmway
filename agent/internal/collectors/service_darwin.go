//go:build darwin

package collectors

import (
	"context"
	"os/exec"
	"strings"
)

// defaultServiceSampler (darwin) reads the launchd job table
// (`launchctl list`: "pid<TAB>status<TAB>label" per job, header first).
// A pid that is not "-" means the job is currently running; a pid of "-"
// is a loaded-but-not-running job (stopped); a label absent from the table
// is unknown (no sample). Exec-based, no cgo — the static-binary property
// holds.
func defaultServiceSampler(ctx context.Context, name string) (float64, error) {
	out, err := exec.CommandContext(ctx, "launchctl", "list").Output()
	if err != nil {
		return 0, err
	}
	lines := strings.Split(string(out), "\n")
	for _, line := range lines[1:] { // line 0 is the "PID Status Label" header
		f := strings.Split(line, "\t")
		if len(f) < 3 || f[2] != name {
			continue
		}
		if f[0] == "-" {
			return 0, nil
		}
		return 1, nil
	}
	return 0, ErrServiceUnknown
}
