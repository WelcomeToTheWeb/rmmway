//go:build !windows

package update

import (
	"fmt"
	"os"
	"syscall"
)

// defaultReexec replaces the running process image with the just-installed
// binary (same PID, so the service manager keeps supervising it). On unix
// the install renamed the new file over the old one, so re-execing the same
// path picks up the new version. It does not return on success.
func defaultReexec() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		return fmt.Errorf("re-exec %s: %w", exe, err)
	}
	return nil // not reached on success
}
