//go:build !windows

package exec

import (
	"os"
	"os/exec"
	"syscall"
)

// setupProcessGroup puts the script in its own process group (unix): the
// timeout then kills the WHOLE group — a script's children would otherwise
// survive the interpreter's kill and hold the output pipes open (Wait
// blocks to their death).
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup SIGKILLs the process group (negative pid).
func killProcessGroup(p *os.Process) {
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
}
