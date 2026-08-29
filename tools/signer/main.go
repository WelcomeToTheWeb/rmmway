// Command rmmway-signer signs and verifies RMMWay release artifacts with
// minisign (W3-4). It is a thin CLI over github.com/jedisct1/go-minisign,
// which also provides the format interop guarantee: keys and signatures
// produced here are readable by the reference minisign(1) CLI (and vice
// versa — the library round-trips real CLI fixtures).
//
// Usage:
//
//	rmmway-signer keygen -dir keys -pass <pwd> [-force]
//	rmmway-signer sign   -k keys/minisign.key -pass <pwd> -c "rmmway release v0.4.0" <files...>
//	rmmway-signer verify -p keys/minisign.pub <files...>
//
// The passphrase may be given with -pass or via the MINISIGN_PASS
// environment variable (CI: GitHub secret). Signatures are written next to
// each file as <file>.minisig using the prehashed (Blake2b-512, "ED")
// variant so large binaries are streamed, not loaded into memory.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jedisct1/go-minisign"
	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/scrypt"
)

const (
	sigAlg = "Ed"
	kdfAlg = "Sc"
	chkAlg = "B2"
	// scrypt parameters for key generation: libsodium's "sensitive"
	// interactive limits — the same `minisign -G` in the C reference uses.
	// (Decryption reads these back from the key file, so any stored limits
	// work; these only affect how long keygen takes.)
	kdfOpsLimit = 33554432   // 1<<25
	kdfMemLimit = 1073741824 // 1<<30 (1 GiB)
	streamLen   = 104
	secretLen   = 158
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = cmdKeygen(os.Args[2:])
	case "sign":
		err = cmdSign(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: rmmway-signer <keygen|sign|verify> [flags]")
}

// generateKeyPair builds a minisign key pair in the C reference file
// formats. The secret key is scrypt-encrypted under pass.
func generateKeyPair(pass string) (seckey, pubkey []byte, err error) {
	var keyId [8]byte
	if _, err = rand.Read(keyId[:]); err != nil {
		return nil, nil, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}

	// Checksum: BLAKE2b-256("Ed" || keyId || 64-byte secret) — the same
	// layout the C reference's seckey_compute_chk uses.
	chk := blake2b256(append(append([]byte(sigAlg), keyId[:]...), priv...))

	var salt [32]byte
	if _, err = rand.Read(salt[:]); err != nil {
		return nil, nil, err
	}
	N, r, p, err := scryptParamsFromLimits(kdfOpsLimit, kdfMemLimit)
	if err != nil {
		return nil, nil, err
	}
	stream, err := scrypt.Key([]byte(pass), salt[:], N, r, p, streamLen)
	if err != nil {
		return nil, nil, err
	}
	encKeyId := xor(keyId[:], stream[0:8])
	encSecret := xor(priv, stream[8:72])
	encChk := xor(chk, stream[72:104])
	for i := range stream {
		stream[i] = 0
	}

	var buf [secretLen]byte
	copy(buf[0:2], sigAlg)
	copy(buf[2:4], kdfAlg)
	copy(buf[4:6], chkAlg)
	copy(buf[6:38], salt[:])
	binary.LittleEndian.PutUint64(buf[38:46], kdfOpsLimit)
	binary.LittleEndian.PutUint64(buf[46:54], kdfMemLimit)
	copy(buf[54:62], encKeyId)
	copy(buf[62:126], encSecret)
	copy(buf[126:158], encChk)

	var pubBuf [42]byte
	copy(pubBuf[0:2], sigAlg)
	copy(pubBuf[2:10], keyId[:])
	copy(pubBuf[10:42], pub)

	seckey = []byte("untrusted comment: minisign encrypted secret key\n" +
		base64.StdEncoding.EncodeToString(buf[:]) + "\n")
	pubkey = []byte(fmt.Sprintf("untrusted comment: minisign public key %016X\n", le64(keyId)) +
		base64.StdEncoding.EncodeToString(pubBuf[:]) + "\n")
	return seckey, pubkey, nil
}

// signOne signs file (prehashed mode) and writes <file>.minisig.
func signOne(keyPath, pass, file, comment string) error {
	sk, err := minisign.NewPrivateKeyFromFile(keyPath)
	if err != nil {
		return err
	}
	if err := sk.Decrypt(pass); err != nil {
		return err
	}
	defer sk.Wipe()

	untrusted := "signature from minisign secret key"
	if comment != "" {
		untrusted += "; " + comment
	}
	sig, err := sk.SignFile(file, minisign.SignOptions{
		Hashed:           true,
		UntrustedComment: untrusted,
	})
	if err != nil {
		return err
	}
	return os.WriteFile(file+".minisig", sig.Encode(), 0o644)
}

// verifyOne checks <file>.minisig against the public key.
func verifyOne(pubPath, file string) (string, error) {
	pk, err := minisign.NewPublicKeyFromFile(pubPath)
	if err != nil {
		return "", err
	}
	sig, err := minisign.NewSignatureFromFile(file + ".minisig")
	if err != nil {
		return "", err
	}
	ok, err := pk.VerifyFromFile(file, sig)
	if err != nil || !ok {
		return "", fmt.Errorf("signature invalid: %w", err)
	}
	return sig.UntrustedComment, nil
}

func cmdKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	dir := fs.String("dir", "keys", "output directory")
	pass := fs.String("pass", os.Getenv("MINISIGN_PASS"), "passphrase (or $MINISIGN_PASS)")
	force := fs.Bool("force", false, "overwrite existing key files")
	fs.Parse(args)

	if *pass == "" {
		return fmt.Errorf("passphrase required (-pass or $MINISIGN_PASS)")
	}
	keyPath := filepath.Join(*dir, "minisign.key")
	pubPath := filepath.Join(*dir, "minisign.pub")
	if !*force {
		for _, p := range []string{keyPath, pubPath} {
			if _, err := os.Stat(p); err == nil {
				return fmt.Errorf("%s already exists (use -force to overwrite)", p)
			}
		}
	}

	seckey, pubkey, err := generateKeyPair(*pass)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, seckey, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(pubPath, pubkey, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (0600 — keep secret) and %s\n", keyPath, pubPath)
	fmt.Printf("verify files signed with this key using:\n  rmmway-signer verify -p %s <files...>\n", pubPath)
	return nil
}

func cmdSign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	keyPath := fs.String("k", "keys/minisign.key", "secret key file")
	pass := fs.String("pass", os.Getenv("MINISIGN_PASS"), "passphrase (or $MINISIGN_PASS)")
	comment := fs.String("c", "", "extra untrusted comment, e.g. the release version")
	fs.Parse(args)
	files := fs.Args()
	if len(files) == 0 {
		return fmt.Errorf("no files to sign")
	}
	if *pass == "" {
		return fmt.Errorf("passphrase required (-pass or $MINISIGN_PASS)")
	}

	sk, err := minisign.NewPrivateKeyFromFile(*keyPath)
	if err != nil {
		return err
	}
	if err := sk.Decrypt(*pass); err != nil {
		return err
	}
	defer sk.Wipe()

	for _, f := range files {
		if err := signOne(*keyPath, *pass, f, *comment); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		fmt.Printf("signed %s -> %s.minisig (key %016X)\n", f, f, le64(sk.KeyId))
	}
	return nil
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	pubPath := fs.String("p", "keys/minisign.pub", "public key file")
	fs.Parse(args)
	files := fs.Args()
	if len(files) == 0 {
		return fmt.Errorf("no files to verify")
	}

	failed := false
	for _, f := range files {
		if comment, err := verifyOne(*pubPath, f); err != nil {
			fmt.Printf("FAIL  %s (%v)\n", f, err)
			failed = true
		} else {
			fmt.Printf("OK    %s (%s)\n", f, comment)
		}
	}
	if failed {
		return fmt.Errorf("one or more signatures failed to verify")
	}
	return nil
}

// --- helpers ---------------------------------------------------------------

func blake2b256(b []byte) []byte {
	h, _ := blake2b.New256(nil)
	h.Write(b)
	return h.Sum(nil)
}

func xor(a, b []byte) []byte {
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}

func le64(k [8]byte) uint64 { return binary.LittleEndian.Uint64(k[:]) }

// scryptParamsFromLimits is ported verbatim from go-minisign (unexported
// there) so keys generated here use parameters the library — and the C
// reference, whose pickparams logic this mirrors — derive identically from
// the opslimit/memlimit stored in the key file.
func scryptParamsFromLimits(opslimit, memlimit uint64) (N, r, p int, err error) {
	if opslimit < 32768 {
		opslimit = 32768
	}
	r = 8
	pick := func(maxN uint64) int {
		ln := 1
		for ln < 63 && uint64(1)<<ln <= maxN/2 {
			ln++
		}
		return ln
	}
	var ln int
	if opslimit < memlimit/32 {
		ln = pick(opslimit / uint64(r*4))
		p = 1
	} else {
		ln = pick(memlimit / uint64(r*128))
		maxrp := (opslimit / 4) / (uint64(1) << ln)
		if maxrp > 0x3fffffff {
			maxrp = 0x3fffffff
		}
		p = int(maxrp / uint64(r))
	}
	if ln >= 63 {
		return 0, 0, 0, fmt.Errorf("invalid scrypt parameters")
	}
	N = 1 << ln
	return N, r, p, nil
}
