// Package releases serves signed agent release artifacts (W4-2). It is the
// distribution half of the agent's signed auto-update: the operator (or CI)
// populates a directory with a release manifest (release.json) plus the
// per-platform binaries and their minisign signatures, and the server serves
// them at:
//
//	GET /agent/releases/latest            -> the manifest (JSON)
//	GET /agent/releases/latest/<file>     -> one asset (binary or .minisig)
//
// The manifest names the minisign PUBLIC key that signed the release. The
// agent compares it to its own pinned key and verifies each binary's
// signature before installing — the server is only a carrier, not a trust
// anchor. A missing/empty directory simply means "no release yet" (404),
// which the agent treats as up-to-date.
package releases

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ManifestPath is the fixed manifest filename inside the releases dir.
const ManifestPath = "release.json"

// Asset is one platform's build in the manifest.
type Asset struct {
	// Filename is the name of the binary inside the releases dir. Its
	// signature is Filename+".minisig".
	Filename string `json:"filename"`
	// SHA256 is the lowercase-hex sha256 of the binary (defense in depth;
	// the minisign signature is the primary gate). Empty = not checked.
	SHA256 string `json:"sha256,omitempty"`
}

// Manifest is the on-disk release.json.
type Manifest struct {
	Version    string           `json:"version"`
	ReleasedAt string           `json:"released_at,omitempty"`
	PublicKey  string           `json:"public_key"` // full minisign .pub (2 lines)
	Assets     map[string]Asset `json:"assets"`     // key: "<goos>-<goarch>"
}

// Server serves a releases directory. The manifest is re-read from disk on
// every request so an operator can publish a new release without restarting
// the server.
type Server struct {
	dir string
}

// New opens a releases directory. dir must exist and contain release.json;
// a not-yet-populated directory is the caller's concern (return an error so
// a typo in RMMWAY_RELEASES_DIR fails fast at boot).
func New(dir string) (*Server, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("releases dir: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("releases dir %s is not a directory", abs)
	}
	if _, err := os.Stat(filepath.Join(abs, ManifestPath)); err != nil {
		return nil, fmt.Errorf("releases dir %s has no %s", abs, ManifestPath)
	}
	return &Server{dir: abs}, nil
}

// Dir returns the absolute releases directory (for logging).
func (s *Server) Dir() string { return s.dir }

// Manifest reads + validates the current release.json.
func (s *Server) Manifest() (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(s.dir, ManifestPath))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", ManifestPath, err)
	}
	if m.Version == "" {
		return nil, fmt.Errorf("%s: missing version", ManifestPath)
	}
	if strings.TrimSpace(m.PublicKey) == "" {
		return nil, fmt.Errorf("%s: missing public_key", ManifestPath)
	}
	if len(m.Assets) == 0 {
		return nil, fmt.Errorf("%s: no assets", ManifestPath)
	}
	return &m, nil
}

// AssetPath resolves a requested asset name to a file path inside the
// releases dir. Only names the current manifest allows are served: an
// asset's Filename or Filename+".minisig". Anything else (including any
// path separator / traversal) is rejected, so the endpoint can never read
// an arbitrary file off disk.
func (s *Server) AssetPath(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return "", fmt.Errorf("bad asset name %q", name)
	}
	m, err := s.Manifest()
	if err != nil {
		return "", err
	}
	allowed := map[string]bool{}
	for _, a := range m.Assets {
		allowed[a.Filename] = true
		allowed[a.Filename+".minisig"] = true
	}
	if !allowed[name] {
		return "", fmt.Errorf("asset %q not in the current release", name)
	}
	p := filepath.Join(s.dir, name)
	if !strings.HasPrefix(p, s.dir+string(os.PathSeparator)) {
		return "", fmt.Errorf("asset %q escapes the releases dir", name)
	}
	return p, nil
}

// PublishDir is a helper (used by scripts + the e2e) that writes a
// releases directory: it copies each src binary (+ its .minisig) into dir
// and writes release.json describing them (version, publicKey, per-asset
// sha256). goosArch keys the assets map (e.g. "linux-amd64").
func PublishDir(dir, version, publicKey string, files map[string]string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	m := Manifest{Version: version, PublicKey: publicKey, Assets: map[string]Asset{}}
	for ga, src := range files {
		base := path.Base(src)
		dest := filepath.Join(dir, base)
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dest, b, 0o755); err != nil {
			return err
		}
		sum := sha256hex(b)
		a := Asset{Filename: base, SHA256: sum}
		// Carry the signature across when present.
		if sig, err := os.ReadFile(src + ".minisig"); err == nil {
			if err := os.WriteFile(dest+".minisig", sig, 0o644); err != nil {
				return err
			}
		}
		m.Assets[ga] = a
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ManifestPath), append(out, '\n'), 0o644)
}

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
