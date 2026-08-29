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
//	rmmway-agent update [--check]  W4-2: verify the server's latest release
//	                               signature; if valid, install + re-exec
//
// W1-3 service entrypoint (run) and W1-4 enrollment share one config surface:
// environment variables (set by the systemd EnvironmentFile the installer
// writes) take precedence, with an optional --config KEY=VALUE file as a
// fallback for manual / non-systemd runs.
package main

import (
	"context"
	"errors"
	"flag"
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
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/welcometotheweb/rmmway/agent/internal/caps"
	"github.com/welcometotheweb/rmmway/agent/internal/collectors"
	"github.com/welcometotheweb/rmmway/agent/internal/enroll"
	"github.com/welcometotheweb/rmmway/agent/internal/exec"
	"github.com/welcometotheweb/rmmway/agent/internal/logship"
	"github.com/welcometotheweb/rmmway/agent/internal/rotate"
	"github.com/welcometotheweb/rmmway/agent/internal/secure"
	"github.com/welcometotheweb/rmmway/agent/internal/update"
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
const defaultGRPCMTLSPort = "50052" // W3-1: the mTLS agent channel

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
	case "update":
		updateCommand(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "usage: rmmway-agent [--version|ping [server-url]|collect|status|run|update] [--config <path>]\n")
		os.Exit(2)
	}
}

// agentConfig is the merged runtime configuration for run/status.
type agentConfig struct {
	Server         string
	BootstrapToken string
	GRPCAddr       string // plain (bootstrap) channel
	GRPCMTLSAddr   string // W3-1: the mTLS channel (post-enroll)
	IdentityPath   string
	ConfigPath     string // the --config file, if one was given (L3 strip)
}

// loadConfig merges environment (primary — the systemd EnvironmentFile the
// installer writes) with an optional KEY=VALUE config file (fallback for
// manual / non-systemd runs). Env wins on any conflict.
func loadConfig(cfgPath string) agentConfig {
	cfg := agentConfig{
		Server:         os.Getenv("RMMWAY_SERVER"),
		BootstrapToken: os.Getenv("RMMWAY_BOOTSTRAP_TOKEN"),
		GRPCAddr:       os.Getenv("RMMWAY_GRPC_ADDR"),
		GRPCMTLSAddr:   os.Getenv("RMMWAY_GRPC_MTLS_ADDR"),
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
			case "RMMWAY_GRPC_MTLS_ADDR":
				if cfg.GRPCMTLSAddr == "" {
					cfg.GRPCMTLSAddr = v
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

// grpcTarget resolves a channel's gRPC endpoint: an explicit override wins;
// otherwise derive the host from the server URL and use defaultPort.
//
// M6: the server URL's port is the operator's HTTP(S) origin, NOT a gRPC
// port — reusing it (e.g. https://rmm.example.com:8443) dialed the wrong
// port. Only an explicit override carries a meaningful port of its own.
func grpcTarget(server, explicit, defaultPort string) string {
	if explicit != "" {
		return explicit
	}
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return server // already a bare host:port
	}
	return net.JoinHostPort(u.Hostname(), defaultPort)
}

// mtlsHost strips host:port down to the host (for the cert's ServerName).
func mtlsHost(target string) string {
	h, _, err := net.SplitHostPort(target)
	if err != nil || h == "" {
		return target
	}
	return h
}

// stripBootstrapTokenFromConfig removes the spent RMMWAY_BOOTSTRAP_TOKEN
// line from the agent's --config file after a successful enrollment (L3).
// A used one-time credential sitting on every endpoint is a habit, not a
// feature. The token may legitimately be carried by environment instead
// (systemd EnvironmentFile / installer-managed env block), so an unchanged
// file is fine when it holds no token line. Failure is non-fatal — the
// identity file, not the config, is what authorizes reconnection.
func stripBootstrapTokenFromConfig(cfgPath string, log *slog.Logger) {
	if cfgPath == "" {
		return
	}
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return // loadConfig already failed the run on an unreadable file
	}
	orig := string(b)
	kept := make([]string, 0, 8)
	stripped := false
	for _, line := range strings.Split(orig, "\n") {
		k, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && k == "RMMWAY_BOOTSTRAP_TOKEN" {
			stripped = true
			continue
		}
		kept = append(kept, line)
	}
	if !stripped {
		return
	}
	out := strings.Join(kept, "\n")
	if strings.HasSuffix(orig, "\n") {
		out += "\n"
	}
	if werr := os.WriteFile(cfgPath, []byte(out), 0o600); werr != nil {
		log.Warn("strip bootstrap token from config", "file", cfgPath, "err", werr)
		return
	}
	log.Info("bootstrap token removed from config (one-time token consumed)", "file", cfgPath)
}

// setupLogging builds the agent's logger: always stderr, and (W6-1) TEE'd into
// a JSON-lines file when one can be opened. RMMWAY_LOG_FILE overrides the path;
// the default is agent.jsonl next to the persisted identity. Returns the logger,
// the JSONL handler (nil when the file couldn't be opened — shipping disabled),
// and the resolved log path.
func setupLogging(cfg agentConfig) (*slog.Logger, *logship.JSONLHandler, string) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logFile := os.Getenv("RMMWAY_LOG_FILE")
	if logFile == "" {
		logFile = filepath.Join(filepath.Dir(cfg.IdentityPath), "agent.jsonl")
	}
	var jsonl *logship.JSONLHandler
	if h, jerr := logship.NewJSONLFile(logFile, slog.LevelInfo); jerr != nil {
		fmt.Fprintf(os.Stderr, "run: structured log file %s: %v (log shipping disabled)\n", logFile, jerr)
	} else {
		jsonl = h
		log = slog.New(logship.Tee(
			slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}),
			jsonl,
		))
	}
	return log, jsonl, logFile
}

// runCommand is the W1-3/W1-4/W3-1/W3-2 service entrypoint. It loads config,
// sets up logging, then runs the agent either as a proper Windows service
// (when launched by the Service Control Manager — the install.ps1 `sc create`
// path) or in the foreground (a terminal / manual `run`).
func runCommand(args []string) {
	cfgPath := configPath(args)
	cfg := loadConfig(cfgPath)
	cfg.ConfigPath = cfgPath
	if cfg.Server == "" {
		fmt.Fprintln(os.Stderr, "run: no RMMWAY_SERVER configured (set it in the config or environment)")
		os.Exit(1)
	}
	log, jsonl, logFile := setupLogging(cfg)

	if isWindowsService() {
		// Launched by the SCM: block until told to stop. This reports the
		// SERVICE_RUNNING handshake to the SCM — a plain console loop never
		// does, which is why `net start` used to time out with
		// ERROR_SERVICE_TIMEOUT (1053).
		if err := runWindowsService(cfg, log, jsonl, logFile); err != nil {
			log.Error("service stopped", "err", err)
		}
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := runAgent(ctx, log, cfg, jsonl, logFile); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	log.Info("agent stopped")
}

// runAgent performs one full agent pass: apply any staged update, enroll (or
// reuse the persisted identity), pick the channel, and run the authenticated
// metric/heartbeat uplink until ctx is done. It returns nil on a clean stop
// (ctx canceled) and a non-nil error when the pass must be reported or retried
// (enroll, dial, or uplink failure). Both the foreground path and the Windows
// service loop call it.
func runAgent(ctx context.Context, log *slog.Logger, cfg agentConfig, jsonl *logship.JSONLHandler, logFile string) error {
	// W4-2: install an update staged by a previous pass (on Windows the
	// in-use .exe can't be replaced in place, so it is staged as
	// <exe>.pending). Re-verify the signature before touching the binary.
	if exe, err := os.Executable(); err == nil {
		if pub, perr := update.PublicKey(os.Getenv("RMMWAY_UPDATE_PUBKEY")); perr == nil {
			if err := update.ApplyPending(exe, pub, func(msg string, a ...any) {
				log.Info(msg, a...)
			}); err != nil {
				log.Warn("pending update not applied", "err", err)
			}
		} else {
			log.Warn("pending update check skipped (bad RMMWAY_UPDATE_PUBKEY)", "err", perr)
		}
	}

	// 1. Bootstrap channel: plain transport (the agent has no mTLS material
	// yet). Used for the one-time Enroll on first boot.
	plainTarget := grpcTarget(cfg.Server, cfg.GRPCAddr, defaultGRPCPort)
	plainConn, err := grpc.NewClient(plainTarget, grpc.WithTransportCredentials(secure.Insecure()))
	if err != nil {
		return fmt.Errorf("run: dial %s: %w", plainTarget, err)
	}
	plainClient := agentv1.NewAgentServiceClient(plainConn)

	store := enroll.NewStore(cfg.IdentityPath)
	agent := enroll.New(plainClient, store, enroll.Gather(version), cfg.BootstrapToken,
		enroll.WithLogf(func(format string, a ...any) {
			log.Info(fmt.Sprintf(format, a...))
		}),
		// First contact goes over the operator's HTTPS origin (POST
		// /agent/enroll) so a remote agent needs only that origin + the mTLS
		// gRPC port open — not the internal-only plain gRPC bootstrap port.
		// The plain gRPC enroller above stays as the fallback (dev / split
		// deployments) on a transient HTTP failure.
		enroll.WithHTTPEenroller(&enroll.HTTPEnroller{BaseURL: cfg.Server}))

	res, err := agent.EnsureEnrolled(ctx)
	if err != nil {
		plainConn.Close()
		return fmt.Errorf("run: enroll: %w", err)
	}
	devID, jwt := res.BearerMetadata()

	// H3: the device JWT (720h lifetime) is renewed by the server
	// piggybacked on a heartbeat ack once less than a quarter of it
	// remains. The uplink's WithJWTChangeHook fires with the fresh token,
	// and the mTLS unary interceptor below must see it too — so the live
	// token lives in one mutex-guarded cell shared by both. Every rotation
	// is persisted into the identity file (the H2 mirror: a restart mid-
	// window reuses the renewed token, not the stale one from enroll).
	var jwtMu sync.Mutex
	liveJWT := jwt
	getJWT := func() string {
		jwtMu.Lock()
		defer jwtMu.Unlock()
		return liveJWT
	}
	setJWT := func(tok string) {
		if tok == "" {
			return
		}
		jwtMu.Lock()
		changed := liveJWT != tok
		if changed {
			liveJWT = tok
			// Identity is shared with the rotator's persist callback;
			// serialize the JWT field behind the same mutex it holds.
			res.Identity.JWT = tok
		}
		jwtMu.Unlock()
		if !changed {
			return
		}
		log.Info("device JWT renewed", "note", "persisting + shared with mTLS unary interceptor")
		if serr := store.Save(res.Identity); serr != nil {
			log.Warn("persist renewed JWT", "err", serr)
		}
	}

	// L3: the bootstrap token is spent now; strip it from the --config
	// file so a used one-time credential doesn't sit on the endpoint.
	stripBootstrapTokenFromConfig(cfg.ConfigPath, log)

	log.Info("agent ready", "device", devID, "reused_persisted", !res.Enrolled)

	// 2. Channel selection. With a persisted mTLS identity, the long-lived
	// uplink runs on the mTLS channel (leaf presented, org root pinned);
	// otherwise (server predates W3-1 / in-memory mode) it stays plain.
	var client agentv1.AgentServiceClient
	target := plainTarget
	channel := "plain"
	if res.Identity.TLS != nil && res.Identity.TLS.Valid() {
		mtlsTarget := grpcTarget(cfg.Server, cfg.GRPCMTLSAddr, defaultGRPCMTLSPort)
		creds, cerr := secure.New(res.Identity.TLS).TransportCredentials(mtlsHost(mtlsTarget))
		if cerr != nil {
			plainConn.Close()
			return fmt.Errorf("run: mTLS credentials: %w", cerr)
		}
		mtlsConn, merr := grpc.NewClient(mtlsTarget,
			grpc.WithTransportCredentials(creds),
			// Every unary RPC on the mTLS channel (RefreshLeaf, and any
			// future device RPC) must carry the device JWT — the server's
			// interceptor is auth-gated and does not see the Stream's
			// per-call metadata. Stream attaches its own via the caller's
			// context, so this only ever FILLS an unset header.
			grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
				if md, _ := metadata.FromOutgoingContext(ctx); len(md.Get("authorization")) == 0 {
					ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+getJWT())
				}
				return invoker(ctx, method, req, reply, cc, opts...)
			}),
		)
		if merr != nil {
			plainConn.Close()
			return fmt.Errorf("run: dial mTLS %s: %w", mtlsTarget, merr)
		}
		client = agentv1.NewAgentServiceClient(mtlsConn)
		target, channel = mtlsTarget, "mTLS"
		// W3-2: background leaf rotation. Refreshes the device's leaf via the
		// RefreshLeaf RPC — over THIS mTLS connection, presenting the still-
		// valid current leaf — and swaps the fresh pair into the persisted
		// identity (in memory + disk). The uplink's own channel re-reads the
		// leaf at each handshake, so the next reconnect presents the new
		// cert; no downtime.
		go func() {
			rotCfg := rotate.Config{Logger: log}
			// RMMWAY_ROTATE_AFTER (duration, e.g. 45s) forces the first
			// rotation that long after start — the e2e milestone sets it so
			// a ~1h leaf is observed rotating live in seconds.
			if ra := os.Getenv("RMMWAY_ROTATE_AFTER"); ra != "" {
				if d, perr := time.ParseDuration(ra); perr == nil {
					rotCfg.RotateAfter = d
				} else {
					log.Warn("RMMWAY_ROTATE_AFTER is not a duration; ignoring", "value", ra)
				}
			}
			rot := rotate.New(client, res.Identity.TLS, devID, res.Identity.Hostname,
				rotCfg,
				// Take jwtMu: Save marshals the whole Identity, whose JWT
				// field the uplink's renewal hook also writes (H3).
				rotate.WithPersist(func() error {
					jwtMu.Lock()
					defer jwtMu.Unlock()
					return store.Save(res.Identity)
				}),
			)
			if rerr := rot.Run(ctx); rerr != nil && !errors.Is(rerr, context.Canceled) {
				log.Warn("rotator stopped", "err", rerr)
			}
		}()
	} else {
		client = plainClient
	}
	// The bootstrap channel is only needed for Enroll; close it once the
	// uplink is on its own channel (on the plain channel it IS the uplink
	// connection, so keep it alive).
	if channel != "plain" {
		plainConn.Close()
	}

	// W3-3: command execution with per-action capability tokens. On the
	// mTLS channel the agent has pinned the org root (from enroll), so it
	// verifies every dispatched command's capability token against the same
	// trust anchor that makes its channel valid — and refuses (REFUSED) any
	// command outside the minted scope. Without a pinned root (legacy plain
	// channel) the agent keeps the pre-W3-3 log-only behavior.
	var commander *uplink.Commander
	if id := res.Identity; id.TLS != nil && id.TLS.Valid() {
		if v, verr := caps.FromRootPEM([]byte(id.TLS.OrgRootPEM), devID); verr != nil {
			log.Warn("capability verification disabled (no valid org root)", "err", verr)
		} else {
			commander = &uplink.Commander{DevID: devID, Verifier: v, Exec: exec.Default(), Logger: log}
			log.Info("command execution enabled", "note", "capability tokens verified against the pinned org root")
		}
	}

	coll := collectors.NewCollector()
	u := uplink.New(client, devID, jwt, uplink.Config{
		HeartbeatInterval: 30 * time.Second,
		Logger:            log,
	},
		uplink.WithCollector(coll.Collect),
		uplink.WithCommander(commander),
		uplink.WithJWTChangeHook(setJWT),
	)

	// W6-1: ship the structured log events (the tail of the JSON-lines file
	// above) two ways: to Loki over its HTTP push API (RMMWAY_LOKI_URL,
	// e.g. http://localhost:3100) and to the server over the uplink (where
	// they are indexed per device in Timescale for the RMM). Delivery is
	// at-least-once; both sinks dedup by entry id.
	if jsonl != nil {
		sc := logship.Config{
			FilePath: logFile,
			DeviceID: devID,
			// Ship from the START of the file on first boot: the enroll /
			// "agent ready" lines are logged BEFORE the shipper starts and
			// must not be lost. On restart the persisted offset state
			// (<path>.shipoffset) resumes where it left off, and entry ids
			// make any re-send a no-op.
			StartAtEnd:    false,
			FlushInterval: 2 * time.Second,
			PollInterval:  500 * time.Millisecond,
			Logger:        log,
			Uplink: func(ctx context.Context, entries []logship.Entry) error {
				batch := &agentv1.LogBatch{Entries: make([]*agentv1.LogEntry, 0, len(entries))}
				for _, e := range entries {
					batch.Entries = append(batch.Entries, &agentv1.LogEntry{
						Id: e.ID, TimestampMs: e.Time.UnixMilli(), Level: e.Level,
						Msg: e.Msg, Attrs: e.Attrs,
					})
				}
				return u.PushLogs(ctx, batch)
			},
		}
		lokiURL := os.Getenv("RMMWAY_LOKI_URL")
		if lokiURL != "" && lokiURL != "off" {
			sc.Loki = logship.NewLokiClient(lokiURL, devID, nil)
		}
		ship, serr := logship.New(sc)
		if serr != nil {
			log.Warn("logship: start", "err", serr)
		} else {
			log.Info("log shipping enabled",
				"file", logFile, "loki", lokiURL, "uplink_indexed", true)
			go func() {
				if rerr := ship.Run(ctx); rerr != nil && !errors.Is(rerr, context.Canceled) {
					log.Warn("logship stopped", "err", rerr)
				}
				_ = ship.Close()
			}()
		}
	}

	// W4-2: signed auto-update. Periodically checks the server for a newer
	// release; a validly signed one is installed + re-exec'd, a tampered or
	// unsigned one is refused (logged, never installed). RMMWAY_AUTO_UPDATE
	// =off disables; RMMWAY_UPDATE_INTERVAL sets the cadence (default 15m).
	go autoUpdate(ctx, log, cfg.Server)

	fmt.Printf("rmmway-agent %s: connected to %s (%s) as device %s; uplink running\n", version, target, channel, devID)
	if err := u.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("run: uplink exited: %w", err)
	}
	return nil
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
	if id.TLS != nil && id.TLS.Valid() {
		fmt.Printf("channel:  mTLS (leaf cert + org root persisted)\n")
	} else {
		fmt.Printf("channel:  plain gRPC (no mTLS identity yet)\n")
	}
	if cfg.Server != "" {
		fmt.Printf("server:   %s\n", cfg.Server)
	}
}

// updateCommand is the W4-2 entrypoint: fetch the server's latest release,
// verify its signature against the agent's pinned minisign key, and — only
// if it verifies — install the new binary and re-exec. A tampered or
// unsigned release is refused and the running binary is left untouched.
//
//	update            verify + install + re-exec
//	update --check    verify only (download + signature, no install)
//	update --no-restart  install but don't re-exec (test / service managers)
func updateCommand(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	checkOnly := fs.Bool("check", false, "verify the latest release but do not install it")
	noRestart := fs.Bool("no-restart", false, "install but do not re-exec the agent")
	serverFlag := fs.String("server", "", "server base URL (default: RMMWAY_SERVER)")
	// --config is handled by loadConfig, not this flag set: strip it out so
	// flag.Parse doesn't choke on an unknown flag.
	cfgPath := configPath(args)
	fsArgs := make([]string, 0, len(args))
	skip := false
	for _, a := range args {
		if skip {
			skip = false
			continue
		}
		if a == "--config" {
			skip = true
			continue
		}
		fsArgs = append(fsArgs, a)
	}
	fs.Parse(fsArgs)

	cfg := loadConfig(cfgPath)
	base := *serverFlag
	if base == "" {
		base = cfg.Server
	}
	if base == "" {
		fmt.Fprintln(os.Stderr, "update: no server (set --server or RMMWAY_SERVER)")
		os.Exit(1)
	}
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}

	pub, err := update.PublicKey(os.Getenv("RMMWAY_UPDATE_PUBKEY"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: pinned key: %v\n", err)
		os.Exit(1)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	u := update.New(update.Config{
		BaseURL:        base,
		CurrentVersion: version,
		PublicKey:      pub,
		Logger:         log,
	})
	res := u.Run(ctx, *checkOnly, *noRestart)

	// Human-readable summary; exit 0 for applied/verified/up-to-date/
	// no-release, 1 for refused/error (so scripts and service units can
	// react to a bad publish).
	switch res.Status {
	case update.StatusApplied:
		extra := ""
		if res.Err != nil {
			extra = " (" + res.Err.Error() + ")"
		}
		fmt.Printf("update: applied %s%s — restarting into the new binary\n", res.Version, extra)
		return
	case update.StatusVerified:
		fmt.Printf("update: latest %s verifies OK (%s) — not installed (--check)\n", res.Version, res.Comment)
		return
	case update.StatusUpToDate:
		fmt.Printf("update: already on %s\n", res.Version)
		return
	case update.StatusNoRelease:
		fmt.Println("update: server has no release for this platform yet")
		return
	default:
		fmt.Fprintf(os.Stderr, "update: %s: %v\n", res.Status, res.Err)
		os.Exit(1)
	}
}

// autoUpdate is the W4-2 background loop in `run`: on a cadence it checks
// the server for a newer release and, if the signature verifies, installs
// it and re-execs (the re-exec replaces this process, so the goroutine is
// not reached again). Refused / transient failures are logged and retried
// on the next tick — a bad publish never takes the agent down.
func autoUpdate(ctx context.Context, log *slog.Logger, server string) {
	if v := os.Getenv("RMMWAY_AUTO_UPDATE"); v == "off" || v == "0" {
		log.Debug("auto-update disabled (RMMWAY_AUTO_UPDATE)")
		return
	}
	interval := 15 * time.Minute
	if v := os.Getenv("RMMWAY_UPDATE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			interval = d
		} else {
			log.Warn("bad RMMWAY_UPDATE_INTERVAL, using 15m", "value", v)
		}
	}
	base := server
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	pub, err := update.PublicKey(os.Getenv("RMMWAY_UPDATE_PUBKEY"))
	if err != nil {
		log.Warn("auto-update off (bad RMMWAY_UPDATE_PUBKEY)", "err", err)
		return
	}
	u := update.New(update.Config{BaseURL: base, CurrentVersion: version, PublicKey: pub, Logger: log})

	log.Info("auto-update armed", "interval", interval.String(), "pinned_key", update.KeyID(pub))
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		res := u.Run(cctx, false, false)
		cancel()
		switch res.Status {
		case update.StatusApplied:
			log.Info("auto-update applied", "version", res.Version)
			return // re-exec (or staged restart) takes over from here
		case update.StatusRefused:
			log.Warn("auto-update refused, kept current binary", "version", res.Version, "reason", res.Err)
		case update.StatusError:
			log.Warn("auto-update check failed", "err", res.Err)
			// up-to-date / no-release: quiet by design
		}
	}
}
