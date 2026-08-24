//go:build windows

package exec

import (
	"os"
	"os/exec"
)

// setupProcessGroup: windows has no process groups (Job Objects would be
// the equivalent); the interpreter is killed directly on timeout.
func setupProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup kills the interpreter process.
func killProcessGroup(p *os.Process) { _ = p.Kill() }
