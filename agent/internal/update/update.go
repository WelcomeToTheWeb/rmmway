package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Status is the outcome of one update pass.
type Status string

const (
	StatusApplied   Status = "applied"  // verified + installed (+ restarted, or staged)
	StatusVerified  Status = "verified" // --check: verifies OK, nothing installed
	StatusUpToDate  Status = "up-to-date"
	StatusRefused   Status = "refused"    // signature/check failed; current binary untouched
	StatusNoRelease Status = "no-release" // server has no releases / no asset for this platform
	StatusError     Status = "error"      // transport / install failure
)

// Result is what a single Run() pass reports.
type Result struct {
	Status  Status
	Version string // manifest version seen ("" if none)
	Comment string // minisign untrusted comment on a verified release
	Err     error  // refusal / error reason (nil otherwise)
}

// ErrNotFound is returned by the downloader for an HTTP 404 (e.g. the
// release has no .minisig → "unsigned").
var ErrNotFound = errors.New("not found (404)")

// ErrStagedPending: the binary is in use (Windows) — the new version was
// staged as <exe>.pending and activates on the next agent start.
var ErrStagedPending = errors.New("binary in use: update staged, activates on next restart")

// ErrRestartDeferred: installed (or staged) but the process could not be
// restarted in place; activation is deferred to the next start.
var ErrRestartDeferred = errors.New("restart deferred: update activates on next start")

// maxDownload caps a release download (an agent binary is a few MB).
const maxDownload = 256 << 20

// Config wires an Updater.
type Config struct {
	// BaseURL is the server's base URL (e.g. "http://rmm.local").
	BaseURL string
	// CurrentVersion is this agent's stamped version (main.version).
	CurrentVersion string
	// PublicKey is the resolved pinned key (PublicKey(); .pub contents).
	PublicKey string
	// ExecPath is the binary to update (default: os.Executable()).
	ExecPath string
	// OS/Arch select the manifest asset (default: runtime).
	OS, Arch string
	// HTTPClient (default: a 5-min-timeout client).
	HTTPClient *http.Client
	// Logger (default: slog.Default()).
	Logger *slog.Logger
	// Install replaces currentPath with the staged, verified binary
	// (default: atomic rename; in-use binaries are staged as .pending).
	// newSig is the .minisig staged next to a .pending binary.
	Install func(newPath, newSig, currentPath string) error
	// Reexec activates the installed binary (default: re-exec self on
	// unix; ErrRestartDeferred elsewhere).
	Reexec func() error
}

// Updater runs one update pass against a server's release endpoint.
type Updater struct{ cfg Config }

// New fills Config defaults and returns an Updater.
func New(cfg Config) *Updater {
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.OS == "" {
		cfg.OS = runtime.GOOS
	}
	if cfg.Arch == "" {
		cfg.Arch = runtime.GOARCH
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 5 * time.Minute}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ExecPath == "" {
		if exe, err := os.Executable(); err == nil {
			cfg.ExecPath = exe
		}
	}
	if cfg.Install == nil {
		cfg.Install = defaultInstall
	}
	if cfg.Reexec == nil {
		cfg.Reexec = defaultReexec
	}
	return &Updater{cfg: cfg}
}

// Run is one full update pass: fetch the latest manifest, pick this
// platform's asset, gate it on the version, check the publisher key,
// download the binary + its .minisig, verify sha256 + signature against
// the pinned key, then install and (unless noRestart) re-exec.
//
// checkOnly stops after verification (StatusVerified). A refused release
// never touches ExecPath; it is reported via Result{Status: StatusRefused}.
func (u *Updater) Run(ctx context.Context, checkOnly, noRestart bool) *Result {
	log := u.cfg.Logger
	fail := func(st Status, format string, a ...any) *Result {
		r := &Result{Status: st, Err: fmt.Errorf(format, a...)}
		log.Warn("update: "+string(st), "err", r.Err.Error())
		return r
	}

	// 1. Manifest.
	man, err := u.latest(ctx)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return &Result{Status: StatusNoRelease}
		}
		return fail(StatusError, "fetch release manifest: %v", err)
	}

	// 2. This platform's asset.
	asset, ok := man.AssetFor(u.cfg.OS, u.cfg.Arch)
	if !ok {
		return &Result{Status: StatusNoRelease}
	}

	// 3. Version gate (no silent downgrades).
	if !newer(u.cfg.CurrentVersion, man.Version) {
		log.Debug("update: up to date", "version", man.Version)
		return &Result{Status: StatusUpToDate, Version: man.Version}
	}

	// 4. Publisher gate: the manifest must name the pinned key. A server
	// naming a different key is a refusal, full stop.
	if !sameKey(man.PublicKey, u.cfg.PublicKey) {
		return fail(StatusRefused, "release signed by key %s, agent is pinned to %s",
			mustKeyID(man.PublicKey), mustKeyID(u.cfg.PublicKey))
	}

	// 5. Download the binary + its signature to a temp dir.
	dir, err := os.MkdirTemp("", "rmmway-update-")
	if err != nil {
		return fail(StatusError, "temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	binPath := filepath.Join(dir, asset.Filename)
	sigPath := binPath + ".minisig"
	if err := u.download(ctx, u.cfg.BaseURL+"/agent/releases/latest/"+asset.Filename, binPath); err != nil {
		return fail(StatusError, "download %s: %v", asset.Filename, err)
	}
	if err := u.download(ctx, u.cfg.BaseURL+"/agent/releases/latest/"+asset.Filename+".minisig", sigPath); err != nil {
		if errors.Is(err, ErrNotFound) {
			return fail(StatusRefused, "release %s is unsigned (no .minisig served)", asset.Filename)
		}
		return fail(StatusError, "download %s.minisig: %v", asset.Filename, err)
	}

	// 6. Verify: sha256 (when the manifest gives one) + minisign signature
	// against the pinned key. THE gate — anything less than a verified
	// signature refuses the update.
	if err := VerifySHA256(binPath, asset.SHA256); err != nil {
		return fail(StatusRefused, "release %s failed checksum: %v", asset.Filename, err)
	}
	comment, err := VerifySignature(u.cfg.PublicKey, binPath, sigPath)
	if err != nil {
		return fail(StatusRefused, "release %s refused: %v", asset.Filename, err)
	}
	log.Info("update: release verified", "version", man.Version, "comment", comment,
		"key", mustKeyID(u.cfg.PublicKey))
	if checkOnly {
		return &Result{Status: StatusVerified, Version: man.Version, Comment: comment}
	}

	// 7. Install (atomic; an in-use binary is staged as .pending).
	if err := u.cfg.Install(binPath, sigPath, u.cfg.ExecPath); err != nil && !errors.Is(err, ErrStagedPending) {
		return fail(StatusError, "install: %v", err)
	}

	// 8. Activate.
	var activateErr error
	if !noRestart {
		activateErr = u.cfg.Reexec()
		if activateErr != nil && !errors.Is(activateErr, ErrRestartDeferred) && !errors.Is(activateErr, ErrStagedPending) {
			// The new binary is in place even if the re-exec failed; the
			// service manager (Restart=on-failure) or the next start
			// activates it.
			log.Warn("update: installed but re-exec failed", "err", activateErr)
			activateErr = ErrRestartDeferred
		}
	}
	r := &Result{Status: StatusApplied, Version: man.Version, Comment: comment}
	if activateErr != nil {
		r.Err = activateErr
	}
	return r
}

// latest fetches + decodes the release manifest.
func (u *Updater) latest(ctx context.Context) (*Manifest, error) {
	var man Manifest
	if err := u.getJSON(ctx, u.cfg.BaseURL+"/agent/releases/latest", &man); err != nil {
		return nil, err
	}
	if man.Version == "" || len(man.Assets) == 0 {
		return nil, fmt.Errorf("manifest is empty")
	}
	return &man, nil
}

// getJSON GETs url into v; a 404 surfaces as ErrNotFound.
func (u *Updater) getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := u.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return decodeJSON(io.LimitReader(resp.Body, 1<<20), v)
	case http.StatusNotFound:
		return ErrNotFound
	default:
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
}

// download GETs url to dest (0600) with a size cap.
func (u *Updater) download(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := u.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		// fall through
	case http.StatusNotFound:
		return ErrNotFound
	default:
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, io.LimitReader(resp.Body, maxDownload+1)); err != nil {
		f.Close()
		return err
	}
	if fi, err := f.Stat(); err == nil && fi.Size() > maxDownload {
		f.Close()
		return fmt.Errorf("release too large (> %d bytes)", maxDownload)
	}
	return f.Close()
}

// ApplyPending installs an update staged by a previous pass
// (ExecPath+".pending", with its ".pending.minisig"): it RE-VERIFIES the
// signature before touching the executable (the file sat on disk since the
// previous pass), then renames it into place. Called at agent start.
func ApplyPending(execPath, pubKey string, logf func(msg string, args ...any)) error {
	pend := execPath + ".pending"
	if _, err := os.Stat(pend); err != nil {
		return nil // nothing staged
	}
	if _, err := VerifySignature(pubKey, pend, pend+".minisig"); err != nil {
		_ = os.Remove(pend)
		_ = os.Remove(pend + ".minisig")
		return fmt.Errorf("pending update refused: %w", err)
	}
	if err := os.Rename(pend, execPath); err != nil {
		return fmt.Errorf("install pending: %w", err)
	}
	_ = os.Remove(pend + ".minisig")
	if logf != nil {
		logf("pending update installed", "path", execPath)
	}
	return nil
}
