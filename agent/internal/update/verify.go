package update

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/jedisct1/go-minisign"
)

// readfile is a tiny indirection so tests can stub file reads if needed.
var readfile = os.ReadFile

// VerifySignature checks that binPath is signed (prehashed minisign) for
// the public key in pubKey (a minisign .pub file's contents). sigPath is
// the <binPath>.minisig file. It returns the signature's untrusted comment
// (the "rmmway release vX.Y.Z" tag CI stamps in) on success. Any mismatch —
// wrong key, tampered bytes, malformed signature — is an error.
func VerifySignature(pubKey, binPath, sigPath string) (comment string, err error) {
	pk, err := minisign.DecodePublicKey(pubKey)
	if err != nil {
		return "", fmt.Errorf("public key: %w", err)
	}
	sig, err := minisign.NewSignatureFromFile(sigPath)
	if err != nil {
		return "", fmt.Errorf("signature %s: %w", sigPath, err)
	}
	ok, err := pk.VerifyFromFile(binPath, sig)
	if err != nil {
		return "", fmt.Errorf("verify %s: %w", binPath, err)
	}
	if !ok {
		return "", fmt.Errorf("signature does not match %s (tampered or wrong key)", binPath)
	}
	return sig.UntrustedComment, nil
}

// VerifySHA256 checks a file's sha256 against want (hex). An empty want
// skips the check (the manifest may omit it; the minisign signature is the
// primary gate).
func VerifySHA256(path, want string) error {
	if want == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("sha256 mismatch: got %s want %s", got, want)
	}
	return nil
}
