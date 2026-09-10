// Command update is the W4-2 definition-of-done harness: "an update with a
// valid signature is applied; a tampered/unsigned build is refused."
//
// It is a REAL end-to-end: it builds two agent binaries (old + new) with the
// actual build flags, signs the new one with a fresh throwaway minisign key
// using tools/signer (the exact signer CI uses), serves them through a real
// in-process rmmway HTTP server (the /agent/releases/* routes), and runs the
// real agent binary's `update` command against it:
//
//  1. VALID   — a correctly signed new release is verified + installed; the
//     on-disk binary is replaced by the new version.
//  2. TAMPERED — a build whose bytes don't match its signature is REFUSED
//     (the signature gate, not just the checksum); the old binary survives.
//  3. UNSIGNED — a build with no .minisig is REFUSED; the old binary survives.
//
// Usage: cd server && go run ./cmd/e2e/update
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func die(f string, a ...any) {
	fmt.Printf("FAIL: "+f+"\n", a...)
	os.Exit(1)
}

var stepName = "(init)"

func step(name string) {
	stepName = name
	fmt.Printf("\n== %s ==\n", name)
}
func info(f string, a ...any) { fmt.Printf("[%s] %s\n", stepName, fmt.Sprintf(f, a...)) }

// run runs a command, returning combined output; on error it dies with the
// output for diagnosis.
func run(dir, name string, env []string, args ...string) string {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		die("cmd %s %v (dir=%s): %v\n%s", name, args, dir, err, out.String())
	}
	return out.String()
}

func sha256File(t string) string {
	b, err := os.ReadFile(t)
	if err != nil {
		die("read %s: %v", t, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// repoRoot walks up from cwd to the directory containing agent/ and
// tools/signer/.
func repoRoot() string {
	d, err := os.Getwd()
	if err != nil {
		die("cwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, e1 := os.Stat(filepath.Join(d, "agent", "go.mod")); e1 == nil {
			if _, e2 := os.Stat(filepath.Join(d, "tools", "signer", "go.mod")); e2 == nil {
				return d
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	cwd, _ := os.Getwd()
	die("could not locate the repo root from %s", cwd)
	return ""
}

func main() {
	root := repoRoot()
	agentDir := filepath.Join(root, "agent")
	signerDir := filepath.Join(root, "tools", "signer")
	work, err := os.MkdirTemp("", "rmmway-update-e2e-")
	if err != nil {
		die("temp: %v", err)
	}
	defer os.RemoveAll(work)
	pass := "e2e-update-pass"
	goosArch := runtime.GOOS + "-" + runtime.GOARCH
	assetName := "rmmway-agent-" + goosArch

	step("build the real agent binaries (old 1.0.0, new 2.0.0) + the signer")
	oldBin := filepath.Join(work, "agent-old")
	newBin := filepath.Join(work, "agent-new")
	run(agentDir, "go", nil, "build", "-trimpath", "-ldflags", "-s -w -X main.version=1.0.0", "-o", oldBin, "./cmd/agent")
	run(agentDir, "go", nil, "build", "-trimpath", "-ldflags", "-s -w -X main.version=2.0.0", "-o", newBin, "./cmd/agent")
	signer := filepath.Join(work, "rmmway-signer")
	run(signerDir, "go", nil, "build", "-o", signer, ".")
	info("old=%s new=%s signer=%s", oldBin, newBin, signer)

	step("mint a throwaway minisign keypair (the test trust anchor)")
	keys := filepath.Join(work, "keys")
	run(work, signer, []string{"MINISIGN_PASS=" + pass}, "keygen", "-dir", keys, "-force", "-pass", pass)
	pubFile := filepath.Join(keys, "minisign.pub")
	pubKey, err := os.ReadFile(pubFile)
	if err != nil {
		die("read pub: %v", err)
	}
	info("throwaway release key: %s", strings.TrimSpace(string(bytes.SplitN(pubKey, []byte("\n"), 2)[0])))

	// The "current" install: the old binary at the path the agent will update.
	installDir := filepath.Join(work, "install")
	_ = os.MkdirAll(installDir, 0o755)
	current := filepath.Join(installDir, "rmmway-agent")
	_ = copyFile(oldBin, current, 0o755)
	curSHA := sha256File(current)

	// Sign the new binary and publish a release dir for it.
	step("sign the new build + publish a release dir")
	run(work, signer, []string{"MINISIGN_PASS=" + pass}, "sign", "-k", filepath.Join(keys, "minisign.key"), "-pass", pass, "-c", "rmmway release v2.0.0", newBin)
	// Give the served asset a clean name (the agent fetches by manifest name).
	served := filepath.Join(work, assetName)
	_ = copyFile(newBin, served, 0o755)
	_ = copyFile(newBin+".minisig", served+".minisig", 0o644)
	newSHA := sha256File(served)
	info("new build sha256=%s (signed)", newSHA)

	relDir := filepath.Join(work, "releases")
	if err := publish(relDir, "2.0.0", string(pubKey), goosArch, served); err != nil {
		die("publish: %v", err)
	}
	info("release dir: %s (release.json + %s + %s.minisig)", relDir, assetName, assetName)

	step("boot a real in-process rmmway server serving the release dir")
	srv := serveReleases(relDir)
	defer srv.Close()
	baseURL := srv.URL
	info("server up at %s", baseURL)

	// ---- 1. VALID: a correctly signed release is applied -------------------
	step("1. VALID release -> verified + installed (old 1.0.0 -> new 2.0.0)")
	out := runAgentUpdate(work, current, srv.URL, pubFile, nil)
	info("agent said: %s", strings.TrimSpace(out))
	if !strings.Contains(out, "applied 2.0.0") {
		die("expected 'applied 2.0.0', got: %s", out)
	}
	if got := sha256File(current); got != newSHA {
		die("installed binary sha %s != new build sha %s", got, newSHA)
	}
	if ver := versionOf(current); ver != "2.0.0" {
		die("installed binary reports version %q, want 2.0.0", ver)
	}
	info("PASS: signed release applied — on-disk binary is now %s", versionOf(current))

	// Re-install the OLD binary so the next cases start from the same state.
	_ = copyFile(oldBin, current, 0o755)

	// ---- 2. TAMPERED: bytes don't match the signature -> refused -----------
	step("2. TAMPERED release (bytes flip, signature stale) -> REFUSED, old binary survives")
	// Flip one byte of the SERVED binary, then point the manifest sha at the
	// tampered bytes so the checksum passes and ONLY the signature can catch
	// it (this isolates the W4-2 signature gate).
	b, _ := os.ReadFile(filepath.Join(relDir, assetName))
	b[len(b)/2] ^= 0xff
	_ = os.WriteFile(filepath.Join(relDir, assetName), b, 0o755)
	if err := patchManifestSHA(relDir, goosArch, sha256File(filepath.Join(relDir, assetName))); err != nil {
		die("patch manifest: %v", err)
	}
	out = runAgentUpdate(work, current, srv.URL, pubFile, nil)
	info("agent said: %s", strings.TrimSpace(out))
	if !strings.Contains(out, "refused") {
		die("expected 'refused', got: %s", out)
	}
	if got := sha256File(current); got != curSHA {
		die("tampered release changed the binary (sha %s != original %s)", got, curSHA)
	}
	if ver := versionOf(current); ver != "1.0.0" {
		die("binary changed to %q after a refused update, want 1.0.0", ver)
	}
	info("PASS: tampered release refused — binary still %s (sha unchanged)", versionOf(current))

	// Re-install the OLD binary + restore the clean served bytes.
	_ = copyFile(oldBin, current, 0o755)
	_ = copyFile(newBin, filepath.Join(relDir, assetName), 0o755)
	_ = copyFile(newBin+".minisig", filepath.Join(relDir, assetName+".minisig"), 0o644)
	if err := patchManifestSHA(relDir, goosArch, newSHA); err != nil {
		die("restore manifest: %v", err)
	}

	// ---- 3. UNSIGNED: no .minisig -> refused --------------------------------
	step("3. UNSIGNED release (no .minisig) -> REFUSED, old binary survives")
	_ = os.Remove(filepath.Join(relDir, assetName+".minisig"))
	out = runAgentUpdate(work, current, srv.URL, pubFile, nil)
	info("agent said: %s", strings.TrimSpace(out))
	if !strings.Contains(out, "refused") || !strings.Contains(out, "unsigned") {
		die("expected 'refused ... unsigned', got: %s", out)
	}
	if got := sha256File(current); got != curSHA {
		die("unsigned release changed the binary (sha %s != original %s)", got, curSHA)
	}
	info("PASS: unsigned release refused — binary still %s (sha unchanged)", versionOf(current))

	step("PASS: W4-2 DoD — a validly signed release is applied; a tampered or unsigned build is refused and the running binary is left untouched")
}

// runAgentUpdate runs the real agent binary's `update` command with the
// pinned key override, returning its combined output (it exits 0 for
// applied/up-to-date and 1 for refused).
func runAgentUpdate(work, current, baseURL, pubFile string, extraEnv []string) string {
	env := append([]string{
		"RMMWAY_UPDATE_PUBKEY=" + pubFile,
		"RMMWAY_SERVER=" + baseURL,
	}, extraEnv...)
	cmd := exec.Command(current, "update", "--server", baseURL, "--no-restart")
	cmd.Dir = work
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		// A refused update exits 1 by design — that's still a "success" of
		// the refusal; the caller inspects the output. Only die on a hard
		// failure (non-refusal) is the caller's job; return the output.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return out.String()
		}
		die("agent update crashed: %v\n%s", err, out.String())
	}
	return out.String()
}

func versionOf(bin string) string {
	out := run(filepath.Dir(bin), bin, nil, "--version")
	// "rmmway-agent 2.0.0" -> "2.0.0"
	fields := strings.Fields(out)
	if len(fields) >= 2 {
		return fields[len(fields)-1]
	}
	return strings.TrimSpace(out)
}

func copyFile(src, dst string, mode os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, mode)
}
