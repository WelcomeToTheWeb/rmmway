// Package main is the RMMWay agent.
//
// W1-1: single static binary per OS, zero runtime deps. Build via
// `make agent` (or scripts/build-agent.sh) which cross-compiles
// linux/darwin/windows × amd64/arm64 as static binaries with the version
// stamped in.
//
// Subcommands:
//
//	rmmway-agent --version   print version (-v for commit/goos details)
//	rmmway-agent ping [url]  check the server's /healthz
//	rmmway-agent collect     sample the five core metric families once
//	rmmway-agent run         service entrypoint (collect loop; W1-4 adds transport)
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/welcometotheweb/rmmway/agent/internal/collectors"
	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
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
	case "run":
		// W1-3: service entrypoint. For now it runs the collect loop on a
		// fixed cadence and logs each batch. W1-4 replaces the logging sink
		// with the gRPC enroll+Stream transport; the loop shape stays.
		runLoop(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "usage: rmmway-agent [--version|ping [server-url]|collect|run [--config <path>]]\n")
		os.Exit(2)
	}
}

// runLoop is the W1-3 service entrypoint: sample the core metrics on a fixed
// cadence and log each batch. W1-4 swaps the logging sink for the gRPC
// enroll+Stream transport without changing the loop's rhythm.
func runLoop(args []string) {
	cfgPath := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" && i+1 < len(args) {
			cfgPath = args[i+1]
			i++
		}
	}
	if cfgPath != "" {
		if _, err := os.Stat(cfgPath); err != nil {
			fmt.Fprintf(os.Stderr, "run: cannot read config %s: %v\n", cfgPath, err)
			os.Exit(1)
		}
		fmt.Printf("rmmway-agent %s: starting (config %s)\n", version, cfgPath)
	} else {
		fmt.Printf("rmmway-agent %s: starting (no config)\n", version)
	}

	interval := 30 * time.Second
	if v := os.Getenv("RMMWAY_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			interval = d
		}
	}

	coll := collectors.NewCollector()
	for {
		batch, err := coll.Collect(context.Background())
		logBatch(batch, err)
		time.Sleep(interval)
	}
}

// logBatch renders one MetricBatch as a single human/log line.
func logBatch(batch *agentv1.MetricBatch, err error) {
	if batch == nil || len(batch.Samples) == 0 {
		fmt.Printf("collect: %v\n", err)
		return
	}
	parts := make([]string, 0, len(batch.Samples))
	for _, s := range batch.Samples {
		if s.Source != "" {
			parts = append(parts, fmt.Sprintf("%s[%s]=%g", s.Name, s.Source, s.Value))
		} else {
			parts = append(parts, fmt.Sprintf("%s=%g", s.Name, s.Value))
		}
	}
	line := strings.Join(parts, " ")
	if err != nil {
		line += fmt.Sprintf("  (partial: %v)", err)
	}
	fmt.Printf("%s\n", line)
}
