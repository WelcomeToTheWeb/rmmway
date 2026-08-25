// Command publish-release assembles a releases directory for the agent's
// signed auto-update (W4-2) from the artifacts in agent/dist (the output of
// `make agent && make sign`). It scans for rmmway-agent-<goos>-<arch>[.exe]
// binaries, requires each to carry a minisign signature (its .minisig), and
// writes <dir>/release.json describing them (version, the W3-4 public key,
// per-asset sha256). Point the server at the result with
// RMMWAY_RELEASES_DIR=<dir> and the agents will pick it up.
//
// Usage:
//
//	go run ./cmd/publish-release -dir releases-local [-version v0.6.0]
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/welcometotheweb/rmmway/server/internal/releases"
)

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "publish-release: "+f+"\n", a...)
	os.Exit(1)
}

// repoRoot walks up from cwd to the dir containing agent/ and keys/.
func repoRoot() string {
	d, _ := os.Getwd()
	for i := 0; i < 8; i++ {
		if _, e1 := os.Stat(filepath.Join(d, "agent", "go.mod")); e1 == nil {
			if _, e2 := os.Stat(filepath.Join(d, "keys", "minisign.pub")); e2 == nil {
				return d
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	cwd, _ := os.Getwd()
	die("could not locate the repo root from %s", cwd)
	return ""
}

func main() {
	root := repoRoot()
	fs := flag.NewFlagSet("publish-release", flag.ExitOnError)
	dist := fs.String("dist", filepath.Join(root, "agent", "dist"), "agent build dir (agent/dist)")
	out := fs.String("dir", filepath.Join(root, "releases-local"), "output releases directory")
	version := fs.String("version", "", "release version (default: git describe)")
	key := fs.String("key", filepath.Join(root, "keys", "minisign.pub"), "minisign public key to embed")
	fs.Parse(os.Args[1:])

	if *version == "" {
		*version = gitDescribe(root)
	}
	pub, err := os.ReadFile(*key)
	if err != nil {
		die("read public key %s: %v", *key, err)
	}

	// Discover the signed agent binaries in dist.
	entries, err := os.ReadDir(*dist)
	if err != nil {
		die("read dist %s: %v", *dist, err)
	}
	files := map[string]string{}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasPrefix(n, "rmmway-agent-") {
			continue
		}
		if strings.HasSuffix(n, ".minisig") {
			continue // signature files, not assets
		}
		ga := strings.TrimPrefix(n, "rmmway-agent-")
		ga = strings.TrimSuffix(ga, ".exe")
		if strings.Contains(ga, ".") {
			continue // SBOMs (.cdx.json) and other non-binary artifacts
		}
		src := filepath.Join(*dist, n)
		// A release asset must be signed; skip (and report) unsigned ones.
		if _, err := os.Stat(src + ".minisig"); err != nil {
			fmt.Fprintf(os.Stderr, "publish-release: WARNING: %s has no .minisig — skipped (run `make sign`)\n", n)
			continue
		}
		files[ga] = src
		names = append(names, n)
	}
	if len(files) == 0 {
		die("no signed agent binaries found in %s (run: make agent && make sign)", *dist)
	}

	if err := releases.PublishDir(*out, *version, string(pub), files); err != nil {
		die("publish: %v", err)
	}
	sort.Strings(names)
	fmt.Printf("published %d signed release(s) %s to %s:\n", len(names), *version, *out)
	for _, n := range names {
		fmt.Printf("  - %s (+ %s.minisig)\n", n, n)
	}
	fmt.Printf("  - %s\n", releases.ManifestPath)
	fmt.Println()
	fmt.Println("serve it with:  RMMWAY_RELEASES_DIR=" + abs(*out) + "  (server env)")
	fmt.Println("agents then verify the W3-4 signature and auto-update; a manual pass is:")
	fmt.Println("  rmmway-agent update --server http://<server>")
}

func gitDescribe(root string) string {
	if b, err := exec.Command("git", "-C", root, "describe", "--tags", "--always", "--dirty").Output(); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	return "0.0.0-dev"
}

func abs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}
