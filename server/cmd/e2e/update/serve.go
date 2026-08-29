package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/welcometotheweb/rmmway/server/internal/httpapi"
	"github.com/welcometotheweb/rmmway/server/internal/releases"
)

// releaseServer is a live in-process rmmway HTTP server wired with a
// releases directory — the exact surface the agent's auto-update talks to.
type releaseServer struct {
	URL  string
	stop func()
}

func (s *releaseServer) Close() { s.stop() }

func serveReleases(relDir string) *releaseServer {
	rel, err := releases.New(relDir)
	if err != nil {
		die("releases.New: %v", err)
	}
	api := httpapi.New(httpapi.Config{Releases: rel})
	mux := http.NewServeMux()
	api.Register(mux)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		die("listen: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(lis) }()
	return &releaseServer{
		URL:  "http://" + lis.Addr().String(),
		stop: func() { _ = srv.Close() },
	}
}

// publish writes a releases dir: it copies src (+ its .minisig) under the
// canonical asset name and writes release.json (version, publicKey, sha256).
func publish(dir, version, publicKey, goosArch, src string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	name := "rmmway-agent-" + goosArch
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	dest := filepath.Join(dir, name)
	if err := os.WriteFile(dest, b, 0o755); err != nil {
		return err
	}
	if sig, err := os.ReadFile(src + ".minisig"); err == nil {
		if err := os.WriteFile(dest+".minisig", sig, 0o644); err != nil {
			return err
		}
	}
	sum := sha256.Sum256(b)
	m := releases.Manifest{
		Version:   version,
		PublicKey: publicKey,
		Assets: map[string]releases.Asset{
			goosArch: {Filename: name, SHA256: hex.EncodeToString(sum[:])},
		},
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, releases.ManifestPath), append(out, '\n'), 0o644)
}

// patchManifestSHA rewrites the sha256 of the goosArch asset in release.json
// (used to make the checksum pass on a tampered build so the SIGNATURE gate
// is the thing under test).
func patchManifestSHA(dir, goosArch, newSHA string) error {
	p := filepath.Join(dir, releases.ManifestPath)
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var m releases.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	a, ok := m.Assets[goosArch]
	if !ok {
		return fmt.Errorf("no asset for %s in manifest", goosArch)
	}
	a.SHA256 = newSHA
	m.Assets[goosArch] = a
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(out, '\n'), 0o644)
}
