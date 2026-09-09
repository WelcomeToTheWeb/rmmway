// Package users implements the gap #3 operator-identity layer (wave 2,
// lane B): RFC 6238 TOTP MFA, the RBAC session JWT (the legacy
// ingest.OperatorJWT contract extended with role/username claims), and the
// middleware that resolves a bearer token into a scoped Session.
package users

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"
)

// TOTP parameters: SHA-1, 30s period, 6 digits — the Google Authenticator /
// FreeOTP default, so any standard authenticator app can enroll.
const (
	TotpPeriodSeconds = 30
	TotpDigits        = 6
)

// TotpSecret mints a new enrollment secret: 20 random bytes, base32
// (RFC 4648 std alphabet, uppercase, no padding).
func TotpSecret() (string, error) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base32.StdEncoding.EncodeToString(raw), nil
}

// NormalizeBase32 upper-cases, strips whitespace and any padding, and
// re-pads so user-typed secrets (from the QR-scan-free manual-entry path)
// verify the same as the minted form.
func NormalizeBase32(s string) string {
	s = strings.TrimRight(strings.ToUpper(strings.ReplaceAll(s, " ", "")), "=")
	if pad := len(s) % 8; pad != 0 {
		s += strings.Repeat("=", 8-pad)
	}
	return s
}

// ValidBase32 reports whether s decodes from base32 (empty is valid — it
// means "not enrolled" and is the stored default).
func ValidBase32(s string) bool {
	if s == "" {
		return true
	}
	_, err := base32.StdEncoding.DecodeString(NormalizeBase32(s))
	return err == nil
}

// totpCode computes the HOTP value for (secret, counter) — RFC 4226
// dynamic truncation, rendered as a fixed-width digit string.
func totpCode(secret string, counter int64) (string, error) {
	dec, err := base32.StdEncoding.DecodeString(NormalizeBase32(secret))
	if err != nil {
		return "", fmt.Errorf("invalid totp secret: %w", err)
	}
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(counter))
	mac := hmac.New(sha1.New, dec)
	mac.Write(buf)
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	v := int64(sum[offset]&0x7f)<<24 |
		int64(sum[offset+1])<<16 |
		int64(sum[offset+2])<<8 |
		int64(sum[offset+3])
	mod := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(TotpDigits)), nil)
	v %= mod.Int64()
	return fmt.Sprintf("%0*d", TotpDigits, v), nil
}

// TotpCode is the TOTP value valid at time t (RFC 6238 counter =
// unix / 30). Used by tests and the Users UI's "show current code" helper.
func TotpCode(secret string, at time.Time) (string, error) {
	return totpCode(secret, at.Unix()/TotpPeriodSeconds)
}

// TotpCodeNow is the code valid right now (tests, UI debug helper).
func TotpCodeNow(secret string) string {
	c, err := TotpCode(secret, time.Now())
	if err != nil {
		return ""
	}
	return c
}

// VerifyTotp checks code against the 30s step containing at, tolerating one
// step on either side (clock drift; 90s total window). The comparison is
// constant-time over the rendered digit strings.
func VerifyTotp(secret, code string, at time.Time) bool {
	if code == "" {
		return false
	}
	counter := at.Unix() / TotpPeriodSeconds
	for _, c := range []int64{counter - 1, counter, counter + 1} {
		want, err := totpCode(secret, c)
		if err != nil {
			return false
		}
		if hmac.Equal([]byte(code), []byte(want)) {
			return true
		}
	}
	return false
}

// TotpURI is the otpauth:// provisioning URI (RFC 6238 §4) — the Users UI
// renders it as a QR code; manual entry uses the raw secret.
func TotpURI(username, secret string) string {
	return "otpauth://totp/rmmway:" + url.QueryEscape(username) +
		"?secret=" + secret +
		"&issuer=RMMWay" +
		"&algorithm=SHA1" +
		"&digits=" + itoa(TotpDigits) +
		"&period=" + itoa(TotpPeriodSeconds)
}

func itoa(n int) string {
	return fmt.Sprint(n)
}
