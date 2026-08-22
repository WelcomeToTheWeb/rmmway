// Package main is the RMMWay agent.
//
// W1-1: single static binary per OS, zero runtime deps. Build via
// `make agent` (or scripts/build-agent.sh) which cross-compiles
// linux/darwin/windows × amd64/arm64 as static binaries with the version
// stamped in.
//
// Subcommands:
//
//	rmmway-agent --version   print version
//	rmmway-agent ping [url]  check the server's /healthz
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/welcometotheweb/rmmway/agent/internal/collectors"
)

// Injected at build time: -ldflags "-X main.version=... -X main.commit=... -X main.date=..."
var (
	version = "0.0.0-dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "--version", "version":
		if len(args) > 1 && args[1] == "-v" {
			fmt.Printf("rmmway-agent %s (commit %s, built %s, %s/%s)\n", version, commit, date, runtime.GOOS, runtime.GOARCH)
			return
		}
		fmt.Printf("rmmway-agent %s\n", version)
	case "ping":
		server := "http://localhost:8080/healthz"
		if len(args) > 1 {
			server = args[1]
		}
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(server)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ping %s: %v\n", server, err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		fmt.Printf("%s -> %s\n%s\n", server, resp.Status, body)
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
			os.Exit(1)
		}
	case "collect":
		// W1-2: sample all five core metric families once, print as text.
		batch, err := collectors.NewCollector().Collect(context.Background())
		if err != nil && len(batch.Samples) == 0 {
			fmt.Fprintf(os.Stderr, "collect: %v\n", err)
			os.Exit(1)
		}
		for _, s := range batch.Samples {
			if s.Source != "" {
				fmt.Printf("%s[%s] = %g\n", s.Name, s.Source, s.Value)
			} else {
				fmt.Printf("%s = %g\n", s.Name, s.Value)
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "(partial: %v)\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "usage: rmmway-agent [--version|ping [server-url]|collect]\n")
		os.Exit(2)
	}
}
