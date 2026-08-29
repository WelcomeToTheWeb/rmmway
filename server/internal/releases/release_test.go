package releases

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testPub = "untrusted comment: minisign public key 21A03FBA54DCC8F5\nRWT1yNxUuj+gIY0TLM5GW8Gz1v/SPwLvy1+S3CMDpwwvnHnfx8qU7L3T\n"

// buildDir writes a releases dir with one signed asset + its signature.
func buildDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := "rmmway-agent-linux-amd64"
	_ = os.WriteFile(filepath.Join(dir, bin), []byte("bin-bytes"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, bin+".minisig"), []byte("sig-bytes"), 0o644)
	// A file NOT referenced by the manifest (must not be servable).
	_ = os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("nope"), 0o600)
	m := Manifest{
		Version:   "1.0.0",
		PublicKey: testPub,
		Assets: map[string]Asset{
			"linux-amd64": {Filename: bin, SHA256: "aa"},
		},
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, ManifestPath), append(b, '\n'), 0o644)
	return dir
}

func TestNewAndManifest(t *testing.T) {
	dir := buildDir(t)
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "1.0.0" {
		t.Fatalf("version = %q", m.Version)
	}
	if !strings.Contains(m.PublicKey, "21A03FBA54DCC8F5") {
		t.Fatalf("public key not served: %q", m.PublicKey)
	}
}

func TestNewMissingDir(t *testing.T) {
	if _, err := New(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error for missing dir")
	}
}

func TestNewMissingManifest(t *testing.T) {
	dir := t.TempDir()
	if _, err := New(dir); err == nil {
		t.Fatal("expected error for dir without release.json")
	}
}

func TestAssetPathAllowlist(t *testing.T) {
	dir := buildDir(t)
	s, _ := New(dir)

	cases := map[string]bool{
		"rmmway-agent-linux-amd64":          true,
		"rmmway-agent-linux-amd64.minisig":  true,
		"secret.txt":                        false, // on disk but not in manifest
		"..":                                false,
		"../secret.txt":                     false,
		"../..":                             false,
		"":                                  false,
		"rmmway-agent-darwin-amd64":         false, // not in manifest
		"rmmway-agent-linux-amd64.exe":      false,
		"rmmway-agent-linux-amd64.minisig2": false,
	}
	for name, wantOK := range cases {
		_, err := s.AssetPath(name)
		if (err == nil) != wantOK {
			t.Errorf("AssetPath(%q) err=%v, want ok=%v", name, err, wantOK)
		}
	}
	// The allowed binary resolves to a real path inside the dir.
	p, err := s.AssetPath("rmmway-agent-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, dir) {
		t.Fatalf("asset path escapes dir: %s", p)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("resolved path missing: %v", err)
	}
}
