//go:build windows

package exec

import (
	"os"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows has no process groups; a Job Object is the equivalent. Each
// script interpreter is put in its own job with KILL_ON_JOB_CLOSE, so the
// timeout kill reaches the script's WHOLE process tree — killing just the
// interpreter would leave children (e.g. `sh -c "sleep 5"` → sleep.exe)
// alive holding the output pipes open, and cmd.Wait would block until
// they exit on their own (observed: a 300ms timeout took the full 5s).

var (
	kernel32                      = windows.NewLazySystemDLL("kernel32.dll")
	procCreateJobObjectW          = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject   = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject  = kernel32.NewProc("AssignProcessToJobObject")
)

type jobObject struct {
	handle windows.Handle
}

var (
	jobsMu sync.Mutex
	jobs   = map[int]*jobObject{} // pid -> job
)

func newJobObject() (*jobObject, error) {
	h, _, err := procCreateJobObjectW.Call(0, 0) // default (NULL) security descriptor
	if h == 0 {
		return nil, err
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	r1, _, e2 := procSetInformationJobObject.Call(
		h,
		uintptr(windows.JobObjectExtendedLimitInformation),
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Sizeof(info)),
	)
	if r1 == 0 {
		_ = windows.CloseHandle(windows.Handle(h))
		return nil, e2
	}
	return &jobObject{handle: windows.Handle(h)}, nil
}

func (j *jobObject) close() { _ = windows.CloseHandle(j.handle) }

func storeJob(pid int, j *jobObject) {
	jobsMu.Lock()
	jobs[pid] = j
	jobsMu.Unlock()
}

// takeJob removes and returns the job for pid (nil when absent).
func takeJob(pid int) *jobObject {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	j := jobs[pid]
	delete(jobs, pid)
	return j
}

// setupProcessGroup: the job is attached in joinProcessGroup (it needs
// the started process handle).
func setupProcessGroup(cmd *exec.Cmd) {}

// joinProcessGroup puts the just-started interpreter in a KILL_ON_JOB_CLOSE
// job. Best-effort: an interpreter already in another job can't be
// re-attached; the caller ignores the error (timeout then kills only the
// interpreter — the pre-job behavior).
func joinProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	j, err := newJobObject()
	if err != nil {
		return err
	}
	// Go 1.27 removed os.Process.Sys(); WithHandle yields the raw handle.
	var ph uintptr
	if err := cmd.Process.WithHandle(func(h uintptr) { ph = h }); err != nil {
		j.close()
		return err
	}
	r, _, e2 := procAssignProcessToJobObject.Call(uintptr(j.handle), ph)
	if r == 0 {
		j.close()
		return e2
	}
	storeJob(cmd.Process.Pid, j)
	return nil
}

// killProcessGroup closes the process's job — with KILL_ON_JOB_CLOSE that
// terminates the entire process tree (interpreter + children), which also
// releases the output pipes so cmd.Wait unblocks.
func killProcessGroup(p *os.Process) {
	if j := takeJob(p.Pid); j != nil {
		j.close()
		return
	}
	_ = p.Kill()
}

// cleanupProcessGroup closes the job after a NORMAL finish so the
// KILL_ON_JOB_CLOSE handle isn't held for the agent's lifetime (and the
// map doesn't grow per command).
func cleanupProcessGroup(p *os.Process) {
	if j := takeJob(p.Pid); j != nil {
		j.close()
	}
}
