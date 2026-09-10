package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The test trust anchor is a throwaway minisign keypair committed under
// testdata (NOT the W3-4 release key). fixture.bin is pre-signed with it;
// the signature was produced by tools/signer, the same code path CI uses to
// sign real releases, so this exercises genuine signer/verifier interop.

func testPub(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "minisign.pub"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func fixture(t *testing.T) (bin, sig []byte) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "fixture.bin"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := os.ReadFile(filepath.Join("testdata", "fixture.bin.minisig"))
	if err != nil {
		t.Fatal(err)
	}
	return b, s
}

func writeBin(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, content, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// flipOne flips a single byte in a copy of b.
func flipOne(b []byte) []byte {
	c := append([]byte(nil), b...)
	c[len(c)/2] ^= 0xff
	return c
}

// ---- pinned key -------------------------------------------------------------

// TestPinnedKeyIsTheW34ReleaseKey guards the trust anchor: the embedded key
// must be byte-identical to the repo's keys/minisign.pub. Rotating the
// release key without re-embedding it fails this test.
func TestPinnedKeyIsTheW34ReleaseKey(t *testing.T) {
	repo, err := os.ReadFile(filepath.Join("..", "..", "..", "keys", "minisign.pub"))
	if err != nil {
		t.Skipf("repo key not resolvable from this checkout: %v", err)
	}
	if strings.TrimSpace(PinnedPublicKey()) != strings.TrimSpace(string(repo)) {
		t.Fatalf("embedded pinned key != keys/minisign.pub\nembedded:\n%s\nrepo:\n%s",
			PinnedPublicKey(), repo)
	}
	if id, err := keyID(PinnedPublicKey()); err != nil {
		t.Fatalf("pinned key does not parse: %v", err)
	} else if id != "019BF5A0CA5040DD" {
		t.Fatalf("pinned key id = %s, want the W3-4 release key 019BF5A0CA5040DD", id)
	}
}

// TestPublicKeyOverride loads RMMWAY_UPDATE_PUBKEY-style overrides and
// errors on a missing file (a misconfigured trust anchor must not silently
// fall back to the embedded key).
func TestPublicKeyOverride(t *testing.T) {
	if got, err := PublicKey(""); err != nil || got != PinnedPublicKey() {
		t.Fatalf("empty override -> embedded key, got err=%v", err)
	}
	if _, err := PublicKey(filepath.Join("testdata", "minisign.pub")); err != nil {
		t.Fatalf("valid override path: %v", err)
	}
	if _, err := PublicKey(filepath.Join("testdata", "does-not-exist.pub")); err == nil {
		t.Fatal("missing override path must error")
	}
}

// ---- VerifySignature ---------------------------------------------------------

func TestVerifySignatureValid(t *testing.T) {
	bin, sig := fixture(t)
	dir := t.TempDir()
	b := writeBin(t, dir, "rmmway-agent", bin)
	_ = os.WriteFile(b+".minisig", sig, 0o644)

	comment, err := VerifySignature(testPub(t), b, b+".minisig")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !strings.Contains(comment, "rmmway release v2.0.0") {
		t.Fatalf("comment = %q", comment)
	}
}

func TestVerifySignatureTampered(t *testing.T) {
	bin, sig := fixture(t)
	dir := t.TempDir()
	// Flip one byte AFTER signing.
	b := writeBin(t, dir, "rmmway-agent", flipOne(bin))
	_ = os.WriteFile(b+".minisig", sig, 0o644)

	if _, err := VerifySignature(testPub(t), b, b+".minisig"); err == nil {
		t.Fatal("tampered binary verified — expected failure")
	}
}

func TestVerifySignatureWrongKey(t *testing.T) {
	bin, sig := fixture(t)
	dir := t.TempDir()
	b := writeBin(t, dir, "rmmway-agent", bin)
	_ = os.WriteFile(b+".minisig", sig, 0o644)

	// The W3-4 release key is a DIFFERENT key than the test key that signed.
	if _, err := VerifySignature(PinnedPublicKey(), b, b+".minisig"); err == nil {
		t.Fatal("binary signed by the test key verified against the W3-4 key — expected failure")
	}
}

func TestVerifySignatureMissing(t *testing.T) {
	bin, _ := fixture(t)
	dir := t.TempDir()
	b := writeBin(t, dir, "rmmway-agent", bin)
	// No .minisig written.
	if _, err := VerifySignature(testPub(t), b, b+".minisig"); err == nil {
		t.Fatal("unsigned binary verified — expected failure")
	}
}

func TestVerifySignatureMalformedPub(t *testing.T) {
	bin, sig := fixture(t)
	dir := t.TempDir()
	b := writeBin(t, dir, "rmmway-agent", bin)
	_ = os.WriteFile(b+".minisig", sig, 0o644)
	if _, err := VerifySignature("not-a-key", b, b+".minisig"); err == nil {
		t.Fatal("malformed public key accepted")
	}
}

// ---- VerifySHA256 -------------------------------------------------------------

func TestVerifySHA256(t *testing.T) {
	bin, _ := fixture(t)
	dir := t.TempDir()
	b := writeBin(t, dir, "f", bin)
	if err := VerifySHA256(b, sha256Hex(bin)); err != nil {
		t.Fatalf("matching sha: %v", err)
	}
	if err := VerifySHA256(b, strings.Repeat("0", 64)); err == nil {
		t.Fatal("wrong sha accepted")
	}
	if err := VerifySHA256(b, ""); err != nil {
		t.Fatal("empty sha must skip")
	}
}

// ---- version gate -------------------------------------------------------------

func TestNewer(t *testing.T) {
	cases := []struct {
		have, want string
		ok         bool
	}{
		{"1.0.0", "1.0.0", false},
		{"1.0.0", "1.0.1", true},
		{"1.0.0", "1.1.0", true},
		{"1.2.0", "2.0.0", true},
		{"1.0.0", "0.9.0", false}, // down: no
		{"1.1.0", "1.0.9", false}, // down: no
		{"v1.0.0", "v1.0.1", true},
		{"0.0.0-dev", "1.0.0", true},  // dev → release
		{"1.0.0", "0.0.0-dev", false}, // release → dev: no
		{"0.0.0-dev", "0.0.1-dev", true},
		{"", "1.0.0", true}, // no current version → anything
	}
	for _, c := range cases {
		if got := newer(c.have, c.want); got != c.ok {
			t.Errorf("newer(%q, %q) = %v, want %v", c.have, c.want, got, c.ok)
		}
	}
}

// ---- Updater end-to-end (in-process HTTP) -------------------------------------

// releaseRig serves a directory over httptest at the agent's release paths.
type releaseRig struct {
	ts  *httptest.Server
	dir string
	man Manifest
}

func newReleaseRig(t *testing.T) *releaseRig {
	t.Helper()
	dir := t.TempDir()
	rg := &releaseRig{dir: dir}
	rg.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/agent/releases/latest":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(rg.man)
		case strings.HasPrefix(r.URL.Path, "/agent/releases/latest/"):
			name := strings.TrimPrefix(r.URL.Path, "/agent/releases/latest/")
			p := filepath.Join(dir, name)
			if _, err := os.Stat(p); err != nil {
				http.NotFound(w, r)
				return
			}
			http.ServeFile(w, r, p)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(rg.ts.Close)
	return rg
}

func goosArch() string { return runtime.GOOS + "-" + runtime.GOARCH }

// publish drops a (possibly tampered) binary + optional signature into the
// served dir and points the manifest at it.
func (rg *releaseRig) publish(version, pubKey string, bin, sig []byte, withSig bool) string {
	name := "rmmway-agent-" + goosArch()
	_ = os.WriteFile(filepath.Join(rg.dir, name), bin, 0o755)
	if withSig {
		_ = os.WriteFile(filepath.Join(rg.dir, name+".minisig"), sig, 0o644)
	}
	rg.man = Manifest{
		Version:   version,
		PublicKey: pubKey,
		Assets:    map[string]Asset{goosArch(): {Filename: name, SHA256: sha256Hex(bin)}},
	}
	return name
}

func TestUpdaterAppliesValid(t *testing.T) {
	rg := newReleaseRig(t)
	bin, sig := fixture(t)
	rg.publish("2.0.0", testPub(t), bin, sig, true)

	dir := t.TempDir()
	current := writeBin(t, dir, "rmmway-agent", []byte("old-bytes"))

	installed, reexeced := 0, 0
	u := New(Config{
		BaseURL:        rg.ts.URL,
		CurrentVersion: "1.0.0",
		PublicKey:      testPub(t),
		ExecPath:       current,
		Install: func(newPath, newSig, cur string) error {
			installed++
			b, _ := os.ReadFile(newPath)
			return os.WriteFile(cur, b, 0o755)
		},
		Reexec: func() error { reexeced++; return nil },
	})
	res := u.Run(context.Background(), false, false)
	if res.Status != StatusApplied {
		t.Fatalf("status = %s (%v), want applied", res.Status, res.Err)
	}
	if installed != 1 || reexeced != 1 {
		t.Fatalf("installed=%d reexeced=%d, want 1/1", installed, reexeced)
	}
	if got, _ := os.ReadFile(current); string(got) != string(bin) {
		t.Fatalf("binary not replaced: %q", got)
	}
	if res.Version != "2.0.0" {
		t.Fatalf("version = %q", res.Version)
	}
}

func TestUpdaterRefusesTampered(t *testing.T) {
	rg := newReleaseRig(t)
	bin, sig := fixture(t)
	// Serve TAMPERED bytes with the ORIGINAL signature. The manifest sha
	// matches the tampered bytes, so ONLY the signature gate can catch it.
	rg.publish("2.0.0", testPub(t), flipOne(bin), sig, true)

	dir := t.TempDir()
	current := writeBin(t, dir, "rmmway-agent", []byte("old-bytes"))
	installed := 0
	u := New(Config{
		BaseURL:        rg.ts.URL,
		CurrentVersion: "1.0.0",
		PublicKey:      testPub(t),
		ExecPath:       current,
		Install:        func(n, s, c string) error { installed++; return nil },
		Reexec:         func() error { return nil },
	})
	res := u.Run(context.Background(), false, false)
	if res.Status != StatusRefused {
		t.Fatalf("status = %s, want refused", res.Status)
	}
	if installed != 0 {
		t.Fatalf("tampered release was installed %d times", installed)
	}
	if got, _ := os.ReadFile(current); string(got) != "old-bytes" {
		t.Fatalf("binary changed on refusal: %q", got)
	}
}

func TestUpdaterRefusesUnsigned(t *testing.T) {
	rg := newReleaseRig(t)
	bin, _ := fixture(t)
	rg.publish("2.0.0", testPub(t), bin, nil, false) // no .minisig

	dir := t.TempDir()
	current := writeBin(t, dir, "rmmway-agent", []byte("old-bytes"))
	installed := 0
	u := New(Config{
		BaseURL:        rg.ts.URL,
		CurrentVersion: "1.0.0",
		PublicKey:      testPub(t),
		ExecPath:       current,
		Install:        func(n, s, c string) error { installed++; return nil },
		Reexec:         func() error { return nil },
	})
	res := u.Run(context.Background(), false, false)
	if res.Status != StatusRefused {
		t.Fatalf("status = %s, want refused", res.Status)
	}
	if installed != 0 {
		t.Fatal("unsigned release was installed")
	}
	if !strings.Contains(res.Err.Error(), "unsigned") {
		t.Fatalf("refusal reason: %v", res.Err)
	}
}

func TestUpdaterRefusesWrongKey(t *testing.T) {
	rg := newReleaseRig(t)
	bin, sig := fixture(t)
	// Manifest names the W3-4 key, but the agent is pinned to the test key
	// (and the asset was signed by the test key). Publisher gate refuses.
	rg.publish("2.0.0", PinnedPublicKey(), bin, sig, true)

	dir := t.TempDir()
	current := writeBin(t, dir, "rmmway-agent", []byte("old-bytes"))
	u := New(Config{
		BaseURL:        rg.ts.URL,
		CurrentVersion: "1.0.0",
		PublicKey:      testPub(t), // agent trusts the TEST key
		ExecPath:       current,
		Install:        func(n, s, c string) error { return nil },
		Reexec:         func() error { return nil },
	})
	res := u.Run(context.Background(), false, false)
	if res.Status != StatusRefused {
		t.Fatalf("status = %s, want refused", res.Status)
	}
	if !strings.Contains(res.Err.Error(), "pinned") {
		t.Fatalf("refusal reason: %v", res.Err)
	}
}

func TestUpdaterUpToDate(t *testing.T) {
	rg := newReleaseRig(t)
	bin, sig := fixture(t)
	rg.publish("1.0.0", testPub(t), bin, sig, true)

	dir := t.TempDir()
	current := writeBin(t, dir, "rmmway-agent", []byte("same-bytes"))
	installed := 0
	u := New(Config{
		BaseURL:        rg.ts.URL,
		CurrentVersion: "1.0.0",
		PublicKey:      testPub(t),
		ExecPath:       current,
		Install:        func(n, s, c string) error { installed++; return nil },
		Reexec:         func() error { return nil },
	})
	res := u.Run(context.Background(), false, false)
	if res.Status != StatusUpToDate {
		t.Fatalf("status = %s, want up-to-date", res.Status)
	}
	if installed != 0 {
		t.Fatal("up-to-date triggered an install")
	}
}

func TestUpdaterNoRelease(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()
	u := New(Config{BaseURL: ts.URL, CurrentVersion: "1.0.0", PublicKey: testPub(t)})
	res := u.Run(context.Background(), false, false)
	if res.Status != StatusNoRelease {
		t.Fatalf("status = %s, want no-release", res.Status)
	}
}

func TestUpdaterCheckOnly(t *testing.T) {
	rg := newReleaseRig(t)
	bin, sig := fixture(t)
	rg.publish("2.0.0", testPub(t), bin, sig, true)

	dir := t.TempDir()
	current := writeBin(t, dir, "rmmway-agent", []byte("old-bytes"))
	installed := 0
	u := New(Config{
		BaseURL:        rg.ts.URL,
		CurrentVersion: "1.0.0",
		PublicKey:      testPub(t),
		ExecPath:       current,
		Install:        func(n, s, c string) error { installed++; return nil },
		Reexec:         func() error { return nil },
	})
	res := u.Run(context.Background(), true, false) // checkOnly
	if res.Status != StatusVerified {
		t.Fatalf("status = %s, want verified", res.Status)
	}
	if installed != 0 {
		t.Fatal("--check installed anyway")
	}
	if got, _ := os.ReadFile(current); string(got) != "old-bytes" {
		t.Fatal("--check modified the binary")
	}
}
