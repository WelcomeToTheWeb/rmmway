// Package main is the RMMWay agent.
//
// W1-1: single static binary per OS, zero runtime deps. Build via
// `make agent` (or scripts/build-agent.sh) which cross-compiles
// linux/darwin/windows × amd64/arm64 as static binaries with the version
// stamped in.
//
// Subcommands:
//
//	rmmway-agent --version         print version (-v for commit/goos details)
//	rmmway-agent ping [url]        check the server's /healthz
//	rmmway-agent collect           sample the five core metric families once
//	rmmway-agent status [--config] show the persisted enrollment identity
//	rmmway-agent run [--config]    service entrypoint: enroll + authenticated
//	                               metric/heartbeat uplink (W1-4)
//
// W1-3 service entrypoint (run) and W1-4 enrollment share one config surface:
// environment variables (set by the systemd EnvironmentFile the installer
// writes) take precedence, with an optional --config KEY=VALUE file as a
// fallback for manual / non-systemd runs.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/welcometotheweb/rmmway/agent/internal/collectors"
	"github.com/welcometotheweb/rmmway/agent/internal/enroll"
	"github.com/welcometotheweb/rmmway/agent/internal/uplink"
	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// Injected at build time: -ldflags "-X main.version=... -X main.commit=... -X main.date=..."
var (
	version = "0.0.0-dev"
	commit  = "none"
	date    = "unknown"
)

const defaultGRPCPort = "50051"

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
	case "status":
		statusCommand(args[1:])
	case "run":
		runCommand(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "usage: rmmway-agent [--version|ping [server-url]|collect|status|run] [--config <path>]\n")
		os.Exit(2)
	}
}

// agentConfig is the merged runtime configuration for run/status.
type agentConfig struct {
	Server         string
	BootstrapToken string
	GRPCAddr       string
	IdentityPath   string
}

// loadConfig merges environment (primary — the systemd EnvironmentFile the
// installer writes) with an optional KEY=VALUE config file (fallback for
// manual / non-systemd runs). Env wins on any conflict.
func loadConfig(cfgPath string) agentConfig {
	cfg := agentConfig{
		Server:         os.Getenv("RMMWAY_SERVER"),
		BootstrapToken: os.Getenv("RMMWAY_BOOTSTRAP_TOKEN"),
		GRPCAddr:       os.Getenv("RMMWAY_GRPC_ADDR"),
		IdentityPath:   os.Getenv("RMMWAY_IDENTITY"),
	}
	if cfgPath != "" {
		b, err := os.ReadFile(cfgPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot read config %s: %v\n", cfgPath, err)
			os.Exit(1)
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			switch k {
			case "RMMWAY_SERVER":
				if cfg.Server == "" {
					cfg.Server = v
				}
			case "RMMWAY_BOOTSTRAP_TOKEN":
				if cfg.BootstrapToken == "" {
					cfg.BootstrapToken = v
				}
			case "RMMWAY_GRPC_ADDR":
				if cfg.GRPCAddr == "" {
					cfg.GRPCAddr = v
				}
			}
		}
	}
	if cfg.IdentityPath == "" {
		if cfgPath != "" {
			cfg.IdentityPath = filepath.Join(filepath.Dir(cfgPath), "agent-identity.json")
		} else if home, err := os.UserHomeDir(); err == nil {
			cfg.IdentityPath = filepath.Join(home, ".rmmway", "agent-identity.json")
		}
	}
	return cfg
}

// configPath extracts --config <path> from args (empty if absent).
func configPath(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// grpcTarget resolves the gRPC endpoint: an explicit RMMWAY_GRPC_ADDR (host:port)
// wins; otherwise derive the host from the server URL and default to port 50051.
func grpcTarget(server, explicit string) string {
	if explicit != "" {
		return explicit
	}
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return server // already a bare host:port
	}
	port := u.Port()
	if port == "" {
		port = defaultGRPCPort
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// runCommand is the W1-3/W1-4 service entrypoint: connect, enroll on first
// boot (reusing the persisted identity thereafter), then run the
// authenticated heartbeat/metric uplink until signalled.
func runCommand(args []string) {
	cfg := loadConfig(configPath(args))
	if cfg.Server == "" {
		fmt.Fprintln(os.Stderr, "run: no RMMWAY_SERVER configured (set it in the config or environment)")
		os.Exit(1)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	target := grpcTarget(cfg.Server, cfg.GRPCAddr)
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "run: dial %s: %v\n", target, err)
		os.Exit(1)
	}
	defer conn.Close()
	client := agentv1.NewAgentServiceClient(conn)

	store := enroll.NewStore(cfg.IdentityPath)
	agent := enroll.New(client, store, enroll.Gather(version), cfg.BootstrapToken,
		enroll.WithLogf(func(format string, a ...any) {
			log.Info(fmt.Sprintf(format, a...))
		}))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	res, err := agent.EnsureEnrolled(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "run: enroll: %v\n", err)
		os.Exit(1)
	}
	devID, jwt := res.BearerMetadata()
	log.Info("agent ready", "device", devID, "reused_persisted", !res.Enrolled)

	coll := collectors.NewCollector()
	u := uplink.New(client, devID, jwt, uplink.Config{
		HeartbeatInterval: 30 * time.Second,
		Logger:            log,
	}, uplink.WithCollector(coll.Collect))

	fmt.Printf("rmmway-agent %s: connected to %s as device %s; uplink running\n", version, target, devID)
	if err := u.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "run: uplink exited: %v\n", err)
		os.Exit(1)
	}
	log.Info("agent stopped")
}

// statusCommand reports the persisted enrollment identity (W1-4) without
// contacting the server — handy for verifying an install or a restart.
func statusCommand(args []string) {
	cfg := loadConfig(configPath(args))
	store := enroll.NewStore(cfg.IdentityPath)
	id, err := store.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "status: %v\n", err)
		os.Exit(1)
	}
	if id == nil {
		fmt.Printf("not enrolled (identity file %s does not exist yet)\n", store.Path())
		return
	}
	fmt.Printf("device:   %s\n", id.DeviceID)
	fmt.Printf("hostname: %s\n", id.Hostname)
	fmt.Printf("enrolled: %s\n", time.UnixMilli(id.EnrolledAt).UTC().Format(time.RFC3339))
	fmt.Printf("identity: %s\n", store.Path())
	if cfg.Server != "" {
		fmt.Printf("server:   %s\n", cfg.Server)
	}
}
