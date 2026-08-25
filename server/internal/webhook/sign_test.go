package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	secret := []byte("shared-secret")
	body := []byte(`{"id":7,"category":"alert"}`)
	at := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	sig := Sign(secret, at, body)

	if !strings.HasPrefix(sig, "t=") || !strings.Contains(sig, ",v1=") {
		t.Fatalf("unexpected signature shape: %q", sig)
	}
	ok, err := Verify(secret, sig, body)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatalf("expected signature to verify, got false")
	}
}

func TestVerifyWrongSecret(t *testing.T) {
	at := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"id":1}`)
	sig := Sign([]byte("a"), at, body)
	if ok, _ := Verify([]byte("b"), sig, body); ok {
		t.Fatalf("signature with the wrong secret should not verify")
	}
}

func TestVerifyTamperedBody(t *testing.T) {
	at := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	secret := []byte("s")
	sig := Sign(secret, at, []byte(`{"id":1}`))
	if ok, _ := Verify(secret, sig, []byte(`{"id":2}`)); ok {
		t.Fatalf("tampered body should not verify")
	}
}

func TestVerifyWithin(t *testing.T) {
	secret := []byte("s")
	body := []byte(`{"id":1}`)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	// Fresh signature verifies within the window.
	fresh := Sign(secret, now, body)
	if ok, err := VerifyWithin(secret, fresh, body, now.Add(5*time.Second), time.Minute); err != nil || !ok {
		t.Fatalf("fresh signature should verify within window: ok=%v err=%v", ok, err)
	}
	// The same signature, checked 10 minutes later, is stale (replay).
	stale := Sign(secret, now, body)
	if ok, err := VerifyWithin(secret, stale, body, now.Add(10*time.Minute), time.Minute); err == nil && ok {
		t.Fatalf("stale signature should be rejected by the freshness window")
	}
}

func TestHMACMatchesReference(t *testing.T) {
	secret := []byte("key")
	at := time.Unix(1756178400, 0) // 2025-08-25T12:00:00Z
	body := []byte("payload")
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("1756178400.payload"))
	want := hex.EncodeToString(mac.Sum(nil))
	if got := HMAC(secret, at, body); got != want {
		t.Fatalf("HMAC = %q, want %q", got, want)
	}
}

func TestParseSigBareDigest(t *testing.T) {
	// A bare hex digest (no t=) still verifies (treated as ts=0).
	secret := []byte("k")
	at := time.Unix(0, 0)
	body := []byte(`x`)
	ps, err := ParseSig("v1=" + HMAC(secret, at, body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ps.V1 != HMAC(secret, at, body) {
		t.Fatalf("v1 mismatch")
	}
}

func TestCategoryForSubject(t *testing.T) {
	cases := map[string]string{
		"rmmway.events.alert":          CategoryAlert,
		"rmmway.events.device":         CategoryInventory,
		"rmmway.events.flow.trigger":   CategoryAutomation,
		"rmmway.events.flow.step":      CategoryAutomation,
		"rmmway.events.flow.notify":    CategoryAutomation,
		"rmmway.events.command.result": CategoryAutomation,
		"rmmway.events.something.else": CategoryOther,
	}
	for subj, want := range cases {
		if got := CategoryForSubject(subj); got != want {
			t.Errorf("CategoryForSubject(%q) = %q, want %q", subj, got, want)
		}
	}
}

func TestEnvelope(t *testing.T) {
	ev := Event{Seq: 42, Category: CategoryAlert, Type: "rmmway.events.alert", DeviceID: "dev-1",
		At: time.Unix(1, 0).UTC(), Data: []byte(`{"data":{"action":"fired"}}`)}
	env := ev.Envelope()
	if env.ID != 42 || env.Category != CategoryAlert || env.Type != "rmmway.events.alert" {
		t.Fatalf("bad envelope: %+v", env)
	}
	if env.Version != envelopeVersion || env.Source != "rmmway" {
		t.Fatalf("bad provenance: %+v", env)
	}
	if !strings.Contains(string(env.Event), `"action":"fired"`) {
		t.Fatalf("envelope should carry the raw event: %s", env.Event)
	}
}
