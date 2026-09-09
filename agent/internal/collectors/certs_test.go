package collectors

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// writeTestCert writes a self-signed cert with the given validity to dir.
func writeTestCert(t *testing.T, dir, name string, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
}

// certFamilies isolates the cert.days_to_expiry family, keyed by source path.
func certFamilies(batch *agentv1.MetricBatch) map[string]float64 {
	out := map[string]float64{}
	for _, s := range batch.Samples {
		if s.Name == "cert.days_to_expiry" {
			out[s.Source] = s.Value
		}
	}
	return out
}

func TestCertExpiryFamily(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeTestCert(t, dir, "soon.pem", now.Add(48*time.Hour))     // 2 days
	writeTestCert(t, dir, "long.pem", now.Add(370*24*time.Hour)) // ~370 days
	writeTestCert(t, dir, "expired.pem", now.Add(-24*time.Hour)) // -1 day
	if err := os.WriteFile(filepath.Join(dir, "notacert.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A private key file: PEM but not a CERTIFICATE block — must be skipped.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewCollectorWithSamplers(Samplers{
		CertDirs: []string{dir},
		Now:      func() time.Time { return now },
	}).(*defaultCollector)
	batch, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := certFamilies(batch)
	if len(got) != 3 {
		t.Fatalf("expected 3 cert samples (key + non-PEM skipped), got %v", got)
	}
	soon := filepath.Join(dir, "soon.pem")
	long := filepath.Join(dir, "long.pem")
	exp := filepath.Join(dir, "expired.pem")
	if v := got[soon]; v < 1.9 || v > 2.1 {
		t.Errorf("soon: got %v want ~2", v)
	}
	if v := got[long]; v < 369 || v > 371 {
		t.Errorf("long: got %v want ~370", v)
	}
	if v := got[exp]; v > -0.9 || v < -1.1 {
		t.Errorf("expired: got %v want ~-1", v)
	}
}

// TestCertScanCap: the cap bounds inspected files; scan order is
// alphabetical so the survivors are deterministic (a.pem..c.pem, not d/e).
func TestCertScanCap(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i := 0; i < 5; i++ {
		writeTestCert(t, dir, string(rune('a'+i))+".pem", now.Add(30*24*time.Hour))
	}
	c := NewCollectorWithSamplers(Samplers{
		CertDirs: []string{dir},
		CertCap:  3,
		Now:      func() time.Time { return now },
	}).(*defaultCollector)
	batch, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := certFamilies(batch)
	if len(got) != 3 {
		t.Fatalf("cap=3 must yield 3 samples, got %d", len(got))
	}
	if _, ok := got[filepath.Join(dir, "d.pem")]; ok {
		t.Errorf("d.pem must not be scanned under cap=3: %v", got)
	}
	if _, ok := got[filepath.Join(dir, "e.pem")]; ok {
		t.Errorf("e.pem must not be scanned under cap=3: %v", got)
	}
}

func TestCertAbsentDirIsSilent(t *testing.T) {
	c := NewCollectorWithSamplers(Samplers{
		CertDirs: []string{filepath.Join(t.TempDir(), "nope")},
	}).(*defaultCollector)
	batch, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("absent dir must not fail the collect: %v", err)
	}
	if got := certFamilies(batch); len(got) != 0 {
		t.Fatalf("absent dir must ship nothing: %v", got)
	}
}

func TestCertParseErrorIsPartial(t *testing.T) {
	dir := t.TempDir()
	// A truncated CERTIFICATE PEM block: parses as PEM, fails X.509.
	bad := "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"
	if err := os.WriteFile(filepath.Join(dir, "bad.pem"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	writeTestCert(t, dir, "good.pem", now.Add(30*24*time.Hour))
	c := NewCollectorWithSamplers(Samplers{
		CertDirs: []string{dir},
		Now:      func() time.Time { return now },
	}).(*defaultCollector)
	batch, err := c.Collect(context.Background())
	if err == nil {
		t.Fatal("expected a partial error for an unparseable certificate")
	}
	// The good cert must still ship despite the bad one.
	if got := certFamilies(batch); len(got) != 1 {
		t.Fatalf("good cert must still ship: %v", got)
	}
}

func TestParseCertDirsAndCap(t *testing.T) {
	if got := parseCertDirs(""); len(got) != 1 || got[0] != defaultCertDirs {
		t.Errorf("default dirs: %v", got)
	}
	if got := parseCertDirs(" /a , /b ,, "); len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Errorf("split: %v", got)
	}
	if got := parseCertDirs(" , "); len(got) != 1 || got[0] != defaultCertDirs {
		t.Errorf("whitespace-only falls back to default: %v", got)
	}
	if parseCertScanCap("") != defaultCertScanCap {
		t.Error("default cap")
	}
	if parseCertScanCap("5") != 5 {
		t.Error("explicit cap")
	}
	if parseCertScanCap("junk") != defaultCertScanCap {
		t.Error("invalid cap falls back to default")
	}
	if parseCertScanCap("99999") != maxCertScanCap {
		t.Error("cap is clamped to maxCertScanCap")
	}
}
