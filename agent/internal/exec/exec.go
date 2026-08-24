// Package exec executes agent commands (W3-3).
//
// The executor is the "act" half of the capability gate: it runs ONLY what
// the uplink's Commander has already authorized with a verified capability
// token (agent/internal/caps). It is deliberately tiny and injectable:
// RunScript runs a small script through the named interpreter with a hard
// timeout and captured output tails; Reboot restarts the host.
package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// Executor runs authorized commands. Both hooks are injectable for tests;
// Default() wires the real per-OS implementations.
type Executor struct {
	// RunScript executes the script (already decoded) with the interpreter
	// for lang, args passed positionally, and at most timeout. err is
	// non-nil only when the command could not run at all (interpreter
	// missing, temp file failure) — a non-zero exit code is NOT an err, it
	// is reported via exitCode. A ctx deadline surfaces as
	// context.DeadlineExceeded so callers can map it to TIMED_OUT.
	RunScript func(ctx context.Context, lang string, script []byte, args []string, timeout time.Duration) (exitCode int, stdout, stderr []byte, err error)
	// Reboot restarts the host. The caller is expected to have already
	// reported its result; Reboot does not return on success.
	Reboot func(ctx context.Context) error
	// DefaultTimeout applies when a command carries no timeout_s (0 = agent
	// default). Capped at MaxTimeout.
	DefaultTimeout time.Duration
	// MaxTimeout caps any command-supplied timeout.
	MaxTimeout time.Duration
}

// Default builds the real executor for this host.
func Default() *Executor {
	return &Executor{
		RunScript:      defaultRunScript,
		Reboot:         defaultReboot,
		DefaultTimeout: 30 * time.Second,
		MaxTimeout:     time.Hour,
	}
}

func (e *Executor) withDefaults() {
	if e.RunScript == nil {
		e.RunScript = defaultRunScript
	}
	if e.Reboot == nil {
		e.Reboot = defaultReboot
	}
	if e.DefaultTimeout <= 0 {
		e.DefaultTimeout = 30 * time.Second
	}
	if e.MaxTimeout <= 0 {
		e.MaxTimeout = time.Hour
	}
}

// TimeoutFor resolves the command's timeout (0 = agent default, capped).
func (e *Executor) TimeoutFor(commandTimeoutS int32) time.Duration {
	e.withDefaults()
	if commandTimeoutS > 0 {
		d := time.Duration(commandTimeoutS) * time.Second
		if d > e.MaxTimeout {
			return e.MaxTimeout
		}
		return d
	}
	return e.DefaultTimeout
}

// interpreterFor maps a script lang to (interpreter, args prefix, temp-file
// extension). The script body always goes to a temp file (uniform across
// sh / powershell / python), with the command's args appended positionally.
func interpreterFor(lang string) (interp string, extra []string, ext string, err error) {
	switch lang {
	case "sh":
		return "sh", nil, ".sh", nil
	case "powershell":
		if runtime.GOOS != "windows" {
			// pwsh is the cross-platform PowerShell; fall back to it.
			if _, e := lookPath("pwsh"); e == nil {
				return "pwsh", []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File"}, ".ps1", nil
			}
			return "", nil, "", fmt.Errorf("powershell unavailable on %s (need pwsh)", runtime.GOOS)
		}
		return "powershell", []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File"}, ".ps1", nil
	case "python":
		if _, e := lookPath("python3"); e == nil {
			return "python3", nil, ".py", nil
		}
		if _, e := lookPath("python"); e == nil {
			return "python", nil, ".py", nil
		}
		return "", nil, "", fmt.Errorf("python unavailable (need python3 or python)")
	default:
		return "", nil, "", fmt.Errorf("unsupported script lang %q (want sh|powershell|python)", lang)
	}
}

func lookPath(name string) (string, error) { return exec.LookPath(name) }

func defaultRunScript(ctx context.Context, lang string, script []byte, args []string, timeout time.Duration) (int, []byte, []byte, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	interp, extra, ext, err := interpreterFor(lang)
	if err != nil {
		return 0, nil, nil, err
	}
	if _, err := lookPath(interp); err != nil {
		return 0, nil, nil, fmt.Errorf("%s not found on PATH", interp)
	}

	dir, err := os.MkdirTemp("", "rmmway-cmd-")
	if err != nil {
		return 0, nil, nil, fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "script"+ext)
	if err := os.WriteFile(path, script, 0700); err != nil {
		return 0, nil, nil, fmt.Errorf("write script: %w", err)
	}

	cmdArgs := append(append([]string{}, extra...), path)
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.Command(interp, cmdArgs...)
	// Own process group on unix (exec_unix.go): the timeout kills the WHOLE
	// group, not just the interpreter — a script's children would otherwise
	// survive the kill and hold the output pipes open.
	setupProcessGroup(cmd)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		return 0, nil, nil, fmt.Errorf("start %s: %w", interp, err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	var ee *exec.ExitError
	select {
	case err := <-waitErr:
		switch {
		case errors.As(err, &ee):
			// A clean non-zero exit is a FAILED result, not an error:
			// the script ran and reported its own outcome.
			return ee.ExitCode(), out.Bytes(), errBuf.Bytes(), nil
		case err != nil:
			return 0, out.Bytes(), errBuf.Bytes(), err
		default:
			return 0, out.Bytes(), errBuf.Bytes(), nil
		}
	case <-ctx.Done():
		// Hard timeout: kill the process group (unix) / the interpreter
		// (windows), then reap. The deadline surfaces as
		// context.DeadlineExceeded (mapped to TIMED_OUT by the caller).
		if cmd.Process != nil {
			killProcessGroup(cmd.Process)
		}
		<-waitErr
		return 0, out.Bytes(), errBuf.Bytes(), ctx.Err()
	}
}

// defaultReboot restarts the host per-OS. It does not return on success
// (the process is killed by the reboot); errors surface for non-fatal
// misconfigurations (e.g. no reboot command found).
func defaultReboot(ctx context.Context) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		if p, err := lookPath("systemctl"); err == nil {
			cmd = exec.CommandContext(ctx, p, "reboot")
		} else if p, err := lookPath("shutdown"); err == nil {
			cmd = exec.CommandContext(ctx, p, "-r", "now")
		} else {
			return fmt.Errorf("no reboot mechanism found (need systemctl or shutdown)")
		}
	case "darwin":
		p, err := lookPath("shutdown")
		if err != nil {
			return fmt.Errorf("no reboot mechanism found (need shutdown)")
		}
		cmd = exec.CommandContext(ctx, p, "-r", "now")
	case "windows":
		p, err := lookPath("shutdown")
		if err != nil {
			return fmt.Errorf("no reboot mechanism found (need shutdown.exe)")
		}
		cmd = exec.CommandContext(ctx, p, "/r", "/t", "0", "/c", "rmmway-agent reboot")
	default:
		return fmt.Errorf("reboot unsupported on %s", runtime.GOOS)
	}
	return cmd.Run()
}
