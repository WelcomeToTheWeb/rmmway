package update

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// Asset is one platform's build in a release manifest.
type Asset struct {
	// Filename is the name of the binary inside the releases directory
	// (e.g. "rmmway-agent-linux-amd64"). Its signature is Filename+".minisig".
	Filename string `json:"filename"`
	// SHA256 is the lowercase-hex sha256 of the binary. Empty = not checked
	// (the minisign signature is the primary gate).
	SHA256 string `json:"sha256,omitempty"`
}

// Manifest is what the server serves at GET /agent/releases/latest (W4-2).
// It is read by the server from RMMWAY_RELEASES_DIR/release.json.
//
// PublicKey is the FULL minisign public key file (both lines) that signed
// this release. The agent compares it to its pinned key before trusting
// ANY asset — the server naming a different publisher is a refusal, not a
// warning.
type Manifest struct {
	Version    string           `json:"version"`
	ReleasedAt string           `json:"released_at,omitempty"`
	PublicKey  string           `json:"public_key"`
	Assets     map[string]Asset `json:"assets"` // key: "<goos>-<goarch>"
}

// AssetFor returns the manifest entry for os/arch (e.g. "linux", "amd64").
func (m Manifest) AssetFor(goos, goarch string) (Asset, bool) {
	a, ok := m.Assets[goos+"-"+goarch]
	return a, ok
}

// sameKey reports whether two minisign public-key file contents encode the
// same key (comment lines and surrounding whitespace ignored).
func sameKey(a, b string) bool {
	ka, errA := keyID(a)
	kb, errB := keyID(b)
	if errA != nil || errB != nil {
		return false
	}
	return ka == kb
}

// keyID extracts the 8-byte minisign key id from a public-key file's
// contents, rendered the way the C reference and tools/signer render it:
// the little-endian uint64 of the 8 bytes as 16 uppercase hex chars (the
// value in the "untrusted comment: minisign public key <ID>" line).
func keyID(pub string) (string, error) {
	lines := strings.Split(strings.TrimSpace(pub), "\n")
	if len(lines) < 2 {
		return "", fmt.Errorf("incomplete public key (want 2 lines)")
	}
	b, err := decodeB64(lines[1])
	if err != nil {
		return "", err
	}
	if len(b) != 42 || string(b[0:2]) != "Ed" {
		return "", fmt.Errorf("malformed public key (want 42-byte Ed blob)")
	}
	return fmt.Sprintf("%016X", binary.LittleEndian.Uint64(b[2:10])), nil
}

// newer reports whether want should replace have. Release versions are
// semver-ish ("v0.5.0", "0.5.1"): a numerically HIGHER release updates, an
// equal or lower one does not (no silent downgrades). Dev builds
// ("0.0.0-dev", "...-dirty") are not ordered — any dev→release or dev→dev
// move with a different version string is allowed (how internal rollouts
// happen), a release→dev move is not.
func newer(have, want string) bool {
	if have == "" || have == want {
		return have != want // "" → anything; equal → no
	}
	h, hNum := parseVer(have)
	w, wNum := parseVer(want)
	switch {
	case hNum && wNum:
		return compareVer(w, h) > 0
	case hNum && !wNum:
		return false // release → dev: no
	case !hNum && wNum:
		return true // dev → release: yes
	default:
		return true // dev → dev, different strings: yes
	}
}

// parseVer splits "v0.5.0-rc1" into ([0,5,0], true). A version is numeric
// only if its leading components are all integer triples.
func parseVer(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func compareVer(a, b [3]int) int {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			if a[i] > b[i] {
				return 1
			}
			return -1
		}
	}
	return 0
}
