// Package main is the RMMWay agent (W0-1 scaffold).
//
// W0-1 only needs "a trivial Go server + React app build and run". The real
// agent (static binary, collectors, bootstrap, enrollment) is W1-1 → W1-4.
//
// For now: `rmmway-agent --version` prints the version; `rmmway-agent ping`
// checks reachability of the server's /healthz endpoint.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const version = "0.0.0-scaffold"

func main() {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "--version", "version":
		fmt.Printf("rmmway-agent %s (W0-1 scaffold)\n", version)
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
	default:
		fmt.Fprintf(os.Stderr, "usage: rmmway-agent [--version|ping [server-url]]\n")
		os.Exit(2)
	}
}
