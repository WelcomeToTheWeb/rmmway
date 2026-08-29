// Package update implements the agent's signed auto-update (W4-2): the
// agent checks the server's release signature BEFORE installing an update,
// so a tampered or unsigned build is refused and the running binary is
// left untouched.
//
// Trust model. Releases are signed with a minisign (Ed25519/Blake2b-512,
// prehashed) secret key — the same W3-4 release key that signs every
// artifact in CI. The agent pins the corresponding PUBLIC key in this
// binary (minisign.pub, embedded below). An update is applied only if:
//
//  1. the manifest the server serves names a public key that is IDENTICAL
//     to the pinned one (the server cannot swap in a publisher it doesn't
//     control), AND
//  2. the downloaded binary verifies against that key's signature.
//
// The secret key is never shipped to the agent; only the public half is.
// Key rotation = re-embed the new public key + rebuild/redeploy the agent
// (see the "Release signing" section of the README). The pinned key can be
// overridden at runtime with RMMWAY_UPDATE_PUBKEY (a minisign .pub file),
// which is also the path the e2e/CI harnesses take to exercise the flow
// with a throwaway key.
package update

import _ "embed"

//go:embed minisign.pub
var embeddedPub string

// PinnedPublicKey is the minisign public key the agent trusts for release
// signatures. It is the W3-4 release key (id 019BF5A0CA5040DD).
func PinnedPublicKey() string { return embeddedPub }

// PublicKey resolves the key the agent should treat as pinned: the
// RMMWAY_UPDATE_PUBKEY override (a path to a minisign .pub file) wins,
// otherwise the embedded key. An unreadable override is an error — a
// misconfigured trust anchor must not silently fall back to another key.
func PublicKey(overridePath string) (string, error) {
	if overridePath == "" {
		return embeddedPub, nil
	}
	b, err := readfile(overridePath)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
