// Package webhook exposes the RMMWay NATS event bus as signed HTTP
// webhooks + a live Server-Sent-Events stream (W6-2).
//
// It is a thin, Postgres-backed consumer of the event bus: every event that
// arrives is (1) journaled to an append-only table with a monotonic sequence,
// (2) fanned out to the live SSE subscribers, and (3) delivered to each
// user-defined endpoint that is subscribed to the event's category.
//
// Deliveries are HMAC-SHA256-signed (the endpoint verifies the signature with
// the shared secret) and at-least-once: an endpoint's delivery cursor
// (`last_seq`) only advances on a 2xx, so a flaky or downed receiver is
// re-driven from its cursor by the sweep — that is both the retry and the
// replay. Each event carries its sequence both as the payload `id` and in an
// `X-RMMway-Id` header so a receiver can dedupe the at-least-once redeliveries.

package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SignHeader is the HTTP header that carries the signature. The value is the
// canonical "t=<unix-seconds>,v1=<hex>" form (Stripe-style) so a receiver can
// parse the timestamp and the digest in one place.
const SignHeader = "X-RMMway-Signature"

// IdHeader carries the journal sequence (the dedupe / replay cursor).
const IdHeader = "X-RMMway-Id"

// EventHeader carries the event's concrete type (the bus subject).
const EventHeader = "X-RMMway-Event"

// TimestampHeader is a human-readable UTC timestamp of when the delivery was
// made (the signature's own `t=` is the machine-checkable one).
const TimestampHeader = "X-RMMway-Timestamp"

// signPayload returns the exact byte string the HMAC is computed over:
// "<unix-seconds>.<body>". The timestamp is bound into the signature so a
// captured (body, signature) pair cannot be replayed outside the freshness
// window without being detected.
func signPayload(at time.Time, body []byte) []byte {
	return []byte(strconv.FormatInt(at.Unix(), 10) + "." + string(body))
}

// HMAC returns hex(HMAC-SHA256(secret, "<unix>.<body>")).
func HMAC(secret []byte, at time.Time, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(signPayload(at, body))
	return hex.EncodeToString(mac.Sum(nil))
}

// Sign returns the full header value "t=<unix>,v1=<hex>".
func Sign(secret []byte, at time.Time, body []byte) string {
	return "t=" + strconv.FormatInt(at.Unix(), 10) + ",v1=" + HMAC(secret, at, body)
}

// parsedSig is a decoded "t=..,v1=.." header.
type parsedSig struct {
	Timestamp int64
	V1        string
}

// ParseSig decodes a SignHeader value. It accepts the canonical "t=..,v1=.."
// form and, for convenience, a bare hex digest (treated as unsigned-time).
func ParseSig(v string) (parsedSig, error) {
	var out parsedSig
	v = strings.TrimSpace(v)
	if v == "" {
		return out, fmt.Errorf("empty signature")
	}
	parts := strings.Split(v, ",")
	sawV1 := false
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "t=") {
			n, err := strconv.ParseInt(strings.TrimPrefix(p, "t="), 10, 64)
			if err != nil {
				return out, fmt.Errorf("bad timestamp %q: %w", p, err)
			}
			out.Timestamp = n
		} else if strings.HasPrefix(p, "v1=") {
			out.V1 = strings.TrimPrefix(p, "v1=")
			sawV1 = true
		} else if !sawV1 && !strings.Contains(p, "=") {
			// A bare hex digest with no key.
			out.V1 = p
			sawV1 = true
		} else {
			return out, fmt.Errorf("unrecognized signature token %q", p)
		}
	}
	if !sawV1 {
		return out, fmt.Errorf("signature missing v1 digest")
	}
	return out, nil
}

// Verify checks the signature header against the shared secret and body. The
// timestamp, if present, is reconstructed from the header so the same
// "<unix>.<body>" string the signer used is re-signed and compared in
// constant time.
func Verify(secret []byte, header string, body []byte) (bool, error) {
	ps, err := ParseSig(header)
	if err != nil {
		return false, err
	}
	at := time.Unix(ps.Timestamp, 0)
	want := HMAC(secret, at, body)
	return subtle.ConstantTimeCompare([]byte(want), []byte(ps.V1)) == 1, nil
}

// VerifyWithin is Verify plus a freshness (anti-replay) check: the signed
// timestamp must be within window of now. A receiver uses this to reject a
// signature that is valid but stale (a replayed capture). window <= 0 means
// "no freshness bound" (signature only).
func VerifyWithin(secret []byte, header string, body []byte, now time.Time, window time.Duration) (bool, error) {
	ok, err := Verify(secret, header, body)
	if err != nil || !ok {
		return ok, err
	}
	if window <= 0 {
		return true, nil
	}
	ps, _ := ParseSig(header)
	dt := now.Sub(time.Unix(ps.Timestamp, 0))
	if dt < 0 {
		dt = -dt
	}
	return dt <= window, nil
}
