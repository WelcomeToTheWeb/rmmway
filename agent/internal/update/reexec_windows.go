//go:build windows

package update

// defaultReexec: on Windows the running .exe is locked, so the install is
// staged as <exe>.pending (defaultInstall) and applied at the next agent
// start (ApplyPending). We cannot re-exec in place, so signal that the
// update is staged and will activate on the next start.
func defaultReexec() error {
	return ErrRestartDeferred
}
