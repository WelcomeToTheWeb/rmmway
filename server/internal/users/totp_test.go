package users

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix A test secret: the ASCII "12345678901234567890" (20
// bytes) in base32.
const appendixSecret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

// RFC 6238 appendix A, SHA-1 row. The spec publishes 8-digit values; the
// 6-digit rendering is the trailing six (what authenticator apps show).
var appendixA = []struct {
	t    int64 // unix seconds
	code string // 6 digits
}{
	{59, "287082"},
	{1111111109, "081804"},
	{1111111111, "050471"},
	{1234567890, "005924"},
	{2000000000, "279037"},
	{20000000000, "353130"},
}

func TestTotpAppendixAVectors(t *testing.T) {
	for _, v := range appendixA {
		got, err := TotpCode(appendixSecret, time.Unix(v.t, 0).UTC())
		if err != nil {
			t.Fatalf("TotpCode(t=%d): %v", v.t, err)
		}
		if got != v.code {
			t.Fatalf("TotpCode(t=%d): got %s, want %s (RFC 6238 appendix A)", v.t, got, v.code)
		}
	}
}

func TestTotpVerifyWindow(t *testing.T) {
	at := time.Unix(1234567890, 0).UTC()
	want, _ := TotpCode(appendixSecret, at)

	if !VerifyTotp(appendixSecret, want, at) {
		t.Fatal("current-step code rejected")
	}
	// ±1 step (clock drift) must verify; ±2 must not.
	earlier, _ := TotpCode(appendixSecret, at.Add(-TotpPeriodSeconds*time.Second))
	later, _ := TotpCode(appendixSecret, at.Add(TotpPeriodSeconds*time.Second))
	if !VerifyTotp(appendixSecret, earlier, at) || !VerifyTotp(appendixSecret, later, at) {
		t.Fatal("±1-step drift code rejected")
	}
	far, _ := TotpCode(appendixSecret, at.Add(-2*TotpPeriodSeconds*time.Second))
	if VerifyTotp(appendixSecret, far, at) {
		t.Fatal("2-step-old code accepted")
	}
	if VerifyTotp(appendixSecret, "", at) {
		t.Fatal("empty code accepted")
	}
	if VerifyTotp("!!!not-base32!!!", want, at) {
		t.Fatal("invalid secret accepted")
	}
	if VerifyTotp(appendixSecret, "000000", time.Unix(1, 0).UTC()) {
		t.Fatal("wrong-step code accepted")
	}
}

func TestTotpSecretAndBase32Helpers(t *testing.T) {
	secret, err := TotpSecret()
	if err != nil {
		t.Fatalf("TotpSecret: %v", err)
	}
	if len(secret) != 32 || strings.ContainsAny(secret, "= ") {
		t.Fatalf("secret shape: got %q (want 32 unpadded base32 chars)", secret)
	}
	if dec, err := base32.StdEncoding.DecodeString(secret); err != nil || len(dec) != 20 {
		t.Fatalf("secret decodes: %d bytes, %v", len(dec), err)
	}
	// A freshly minted secret verifies against its own code.
	now := time.Now()
	if code, err := TotpCode(secret, now); err != nil || !VerifyTotp(secret, code, now) {
		t.Fatalf("self-roundtrip: %q, %v", code, err)
	}

	// Normalization: lowercase, spaced, unpadded input == the minted form.
	variants := []string{
		strings.ToLower(appendixSecret),
		strings.Replace(appendixSecret, "G", " G", 1),
		appendixSecret + "==",
	}
	for _, v := range variants {
		if !VerifyTotp(v, "005924", time.Unix(1234567890, 0).UTC()) {
			t.Fatalf("normalized secret %q failed to verify", v)
		}
	}
	if !ValidBase32("") || !ValidBase32(appendixSecret) || ValidBase32("!!!") {
		t.Fatal("ValidBase32 contract broken")
	}
}

func TestTotpURI(t *testing.T) {
	uri := TotpURI("alice@example.com", appendixSecret)
	want := "otpauth://totp/rmmway:alice%40example.com?secret=" + appendixSecret +
		"&issuer=RMMWay&algorithm=SHA1&digits=6&period=30"
	if uri != want {
		t.Fatalf("URI:\n got %s\nwant %s", uri, want)
	}
}
