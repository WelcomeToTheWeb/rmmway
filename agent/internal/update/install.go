package update

import (
	"io"
	"os"
	"path/filepath"
	"strings"
)

// defaultInstall atomically replaces currentPath with the verified binary
// at newPath: copy into the same directory as <base>.new, then rename over
// the target. On unix a rename over a RUNNING executable is fine (the old
// inode lives on until the process re-execs). On Windows an in-use .exe
// cannot be renamed; in that case the update is staged as
// <target>.pending (+ <target>.pending.minisig) and ErrStagedPending is
// returned — ApplyPending installs it at the next agent start.
func defaultInstall(newPath, newSig, currentPath string) error {
	dir := filepath.Dir(currentPath)
	base := filepath.Base(currentPath)
	staged := filepath.Join(dir, base+".new")
	if err := copyFile(newPath, staged, 0o755); err != nil {
		return err
	}
	if err := os.Rename(staged, currentPath); err != nil {
		if isInUse(err) {
			pend := filepath.Join(dir, base+".pending")
			if err := os.Rename(staged, pend); err != nil {
				_ = os.Remove(staged)
				return err
			}
			if newSig != "" {
				if err := copyFile(newSig, pend+".minisig", 0o600); err != nil {
					_ = os.Remove(pend)
					return err
				}
			}
			return ErrStagedPending
		}
		_ = os.Remove(staged)
		return err
	}
	return nil
}

// copyFile copies src to dst (created with mode), replacing dst.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

func isInUse(err error) bool {
	// Windows reports ERROR_ACCESS_DENIED when renaming an in-use file.
	return strings.Contains(strings.ToLower(err.Error()), "access is denied")
}
