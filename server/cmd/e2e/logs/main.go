// Command logs is the W6-1 definition-of-done harness:
//
//	"agent log lines queryable in Loki; the RMM surfaces recent indexed
//	events per device."
//
// It is a REAL full-stack run (no fake agents, no fake Loki):
//
//   - a scratch Timescale database (migrated, 7 migrations),
//   - the real gRPC ingest (JWT auth) with the Postgres log store,
//   - the real operator HTTP API (GET /admin/devices/{id}/events),
//   - the REAL agent binary: it enrolls, tails its own structured
//     (JSON-lines) log, and ships each batch BOTH to a REAL Loki
//     (docker-compose service, :3100) and to the server over the
//     uplink (indexed per device),
//   - a dispatched command that generates a distinctive agent log line.
//
// Assertions:
//  1. the agent's log lines are queryable in Loki (query_range by
//     device_id label) — including the command-receipt line,
//  2. the RMM surfaces the SAME events per device via
//     GET /admin/devices/{id}/events (newest first, level filter works),
//  3. the /api mirror is auth-gated (401 without an operator token).
//
// Usage: RMMWAY_TEST_PG_DSN=... RMMWAY_LOKI_URL=http://localhost:3100
//
//	go run ./cmd/e2e/logs
//
// (make logs-e2e)
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/httpapi"
	"github.com/welcometotheweb/rmmway/server/internal/ingest"
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

func die(f string, a ...any) {
	fmt.Printf("FAIL: "+f+"\n", a...)
	os.Exit(1)
}

var stepName = "(init)"

func step(name string) {
	stepName = name
	fmt.Printf("\n== %s ==\n", name)
}
func info(f string, a ...any) { fmt.Printf("[%s] %s\n", stepName, fmt.Sprintf(f, a...)) }
func check(cond bool, f string, a ...any) {
	if !cond {
		die(f, a...)
	}
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	dsn := os.Getenv("RMMWAY_TEST_PG_DSN")
	if dsn == "" {
		dsn = "postgres://rmmway:rmmway@localhost:5432/rmmway?sslmode=disable"
	}
	lokiURL := os.Getenv("RMMWAY_LOKI_URL")
	if lokiURL == "" {
		lokiURL = "http://localhost:3100"
	}
	// ---- scratch Postgres ------------------------------------------------
	u, err := url.Parse(dsn)
	if err != nil {
		die("parse dsn: %v", err)
	}
	admin, err := pgxpool.New(ctx, u.String())
	if err != nil {
		die("admin pool: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(ctx); err != nil {
		die("postgres not reachable (%v) — run `make up`", err)
	}
	dbName := "rmmway_logs_e2e_" + time.Now().Format("20060102150405")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+dbName); err != nil {
		die("create scratch db: %v", err)
	}
	defer func() {
		ctxC, cancelC := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelC()
		_, _ = admin.Exec(ctxC, `DROP DATABASE IF EXISTS `+dbName)
	}()
	u.Path = "/" + dbName
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		die("scratch pool: %v", err)
	}
	defer pool.Close()

	step("migrate")
	if n, err := store.Migrate(ctx, pool, "migrations"); err != nil {
		die("migrate: %v (n=%d)", err, n)
	} else if n != 8 {
		die("expected 8 migrations, got %d", n)
	}
	info("8 migrations applied to scratch db %s", dbName)

	// ---- in-process server: real ingest + real operator API --------------
	step("boot in-process server (ingest + operator API)")
	logStore := store.NewPostgresLogStore(pool)
	devices := store.NewPostgresDevices(pool)
	svc := ingest.NewService(ingest.Config{
		JWTSecret: []byte("e2e-logs-secret"),
		Logs:      logStore,
	}, store.NewMemoryMetricsSink(10000), devices)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		die("listen: %v", err)
	}
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(svc.JWTInterceptor))
	agentv1.RegisterAgentServiceServer(grpcServer, svc)
	go func() { _ = grpcServer.Serve(lis) }()
	defer grpcServer.Stop()
	grpcAddr := lis.Addr().String()
	info("ingest gRPC on %s (JWT + Postgres log store)", grpcAddr)

	mux := http.NewServeMux()
	apiSrv := httpapi.New(httpapi.Config{
		Devices:       devices,
		JWTSecret:     []byte("e2e-logs-secret"),
		AdminUser:     "admin",
		AdminPassword: "admin",
		MintBootstrap: svc.MintBootstrapToken,
		LogEvents: func(deviceID string, limit int, level string) ([]store.LogEvent, error) {
			return logStore.Recent(context.Background(), deviceID, limit, level)
		},
	})
	apiSrv.Register(mux)
	httpLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		die("http listen: %v", err)
	}
	go func() { _ = http.Serve(httpLis, mux) }()
	httpAddr := "http://" + httpLis.Addr().String()
	info("operator API on %s", httpAddr)
	// C1: the operator routes are JWT-gated — log in once for a token.
	opTok := loginOp(ctx, httpAddr)

	// ---- build the REAL agent binary --------------------------------------
	step("build real agent binary")
	tmp := mustTempDir()
	defer os.RemoveAll(tmp)
	agentDir := filepath.Join(repoRoot(), "agent")
	build := exec.Command("go", "build", "-o", filepath.Join(tmp, "rmmway-agent"), "./cmd/agent")
	build.Dir = agentDir
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		die("agent build: %v", err)
	}
	info("agent binary built (%s)", filepath.Join(tmp, "rmmway-agent"))

	// ---- mint a bootstrap token and run the real agent --------------------
	step("enroll + run the real agent (Loki + uplink shipping)")
	token, devID := svc.MintBootstrapToken()
	info("bootstrap token minted; device will be %s", devID)

	agentLog := filepath.Join(tmp, "agent-stdout.txt")
	af, err := os.Create(agentLog)
	if err != nil {
		die("agent log: %v", err)
	}
	cmd := exec.Command(filepath.Join(tmp, "rmmway-agent"), "run")
	cmd.Dir = tmp
	cmd.Env = append(os.Environ(),
		"RMMWAY_SERVER="+httpAddr,
		"RMMWAY_GRPC_ADDR="+grpcAddr,
		"RMMWAY_BOOTSTRAP_TOKEN="+token,
		"RMMWAY_IDENTITY="+filepath.Join(tmp, "agent-identity.json"),
		"RMMWAY_LOG_FILE="+filepath.Join(tmp, "agent.jsonl"),
		"RMMWAY_LOKI_URL="+lokiURL,
	)
	cmd.Stdout = af
	cmd.Stderr = af
	if err := cmd.Start(); err != nil {
		die("agent start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// Wait until the agent is ready (it logs "agent ready" — which is also
	// the first line the shipper will tail).
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(agentLog); bytes.Contains(b, []byte("agent ready")) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if b, _ := os.ReadFile(agentLog); !bytes.Contains(b, []byte("agent ready")) {
		die("agent did not become ready; log:\n%s", mustRead(agentLog))
	}
	info("agent enrolled as %s and is running (json log %s)", devID, filepath.Join(tmp, "agent.jsonl"))

	// ---- generate a distinctive agent log line: dispatch a command --------
	step("dispatch a command (agent logs its receipt)")
	cmdID, err := svc.Dispatcher().Dispatch(devID, &agentv1.Command_RunScript{
		RunScript: &agentv1.RunScript{
			Lang:      "sh",
			ScriptB64: base64.StdEncoding.EncodeToString([]byte("echo w61-e2e")),
		},
	})
	if err != nil {
		die("dispatch: %v", err)
	}
	info("command %s pushed to the live stream", cmdID)

	// The agent (plain channel, no pinned org root) logs the receipt:
	// "command received (no capability enforcement — legacy channel)".
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(filepath.Join(tmp, "agent.jsonl")); bytes.Contains(b, []byte(cmdID)) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if b, _ := os.ReadFile(filepath.Join(tmp, "agent.jsonl")); !bytes.Contains(b, []byte(cmdID)) {
		die("agent json log never recorded the command receipt (%s); log:\n%s",
			cmdID, mustRead(filepath.Join(tmp, "agent.jsonl")))
	}
	info("agent recorded the command receipt in its structured log")

	// ---- DoD #1: agent log lines are queryable in LOKI --------------------
	step("query Loki for the agent's lines (device_id label)")
	type lokiResp struct {
		Data struct {
			Result []struct {
				Stream map[string]string `json:"stream"`
				Values [][]string        `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	waitLoki := func() (lines []string, err error) {
		q := url.Values{}
		q.Set("query", `{device_id="`+devID+`",job="rmmway-agent"}`)
		q.Set("limit", "200")
		endpoint := lokiURL + "/loki/api/v1/query_range?" + q.Encode()
		cctx, ccancel := context.WithTimeout(ctx, 10*time.Second)
		defer ccancel()
		resp, err := getWithCtx(cctx, endpoint)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("loki status %d: %s", resp.StatusCode, truncate(string(body), 200))
		}
		var lr lokiResp
		if err := json.Unmarshal(body, &lr); err != nil {
			return nil, err
		}
		for _, r := range lr.Data.Result {
			for _, v := range r.Values {
				if len(v) >= 2 {
					lines = append(lines, v[1])
				}
			}
		}
		return lines, nil
	}

	// Wait for the shipper (2s flush) to push BOTH the ready line and the
	// command receipt into Loki.
	deadline = time.Now().Add(45 * time.Second)
	var lokiLines []string
	var lokiErr error
	for time.Now().Before(deadline) {
		lokiLines, lokiErr = waitLoki()
		if lokiErr == nil && containsAll(lokiLines, "agent ready", cmdID) {
			break
		}
		time.Sleep(1 * time.Second)
	}
	check(lokiErr == nil && len(lokiLines) > 0,
		"loki query returned no lines (err=%v) — is Loki up? (`make up`, RMMWAY_LOKI_URL=%s)", lokiErr, lokiURL)
	check(containsAll(lokiLines, "agent ready", cmdID),
		"loki lines missing expected content; got %d lines:\n%s", len(lokiLines), strings.Join(lokiLines, "\n"))
	info("LOKI: %d lines for %s (labels device_id/job/level) — includes 'agent ready' and the command receipt",
		len(lokiLines), devID)

	// ---- DoD #2: the RMM surfaces recent indexed events per device -------
	step("RMM: GET /admin/devices/{id}/events (the Timescale copy)")
	type eventsResp struct {
		DeviceID string           `json:"device_id"`
		Events   []store.LogEvent `json:"events"`
	}
	fetchEvents := func() (*eventsResp, error) {
		cctx, ccancel := context.WithTimeout(ctx, 10*time.Second)
		defer ccancel()
		resp, err := getBearerCtx(cctx, httpAddr+"/admin/devices/"+devID+"/events?limit=100", opTok)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(body), 200))
		}
		var er eventsResp
		if err := json.Unmarshal(body, &er); err != nil {
			return nil, err
		}
		return &er, nil
	}
	deadline = time.Now().Add(30 * time.Second)
	var er *eventsResp
	for time.Now().Before(deadline) {
		er, err = fetchEvents()
		if err == nil && len(er.Events) >= 2 && containsMsgs(er.Events, "agent ready", "command received") {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	check(err == nil, "fetch events: %v", err)
	check(len(er.Events) >= 2, "indexed events = %d, want >= 2 (ready + command receipt)", len(er.Events))
	check(containsMsgs(er.Events, "agent ready", "command received"),
		"indexed events missing expected msgs:\n%+v", er.Events)
	// Newest first: the command receipt (latest) must precede "agent ready".
	iReady, iCmd := -1, -1
	for i, e := range er.Events {
		if e.Msg == "agent ready" {
			iReady = i
		}
		if strings.Contains(e.Msg, "command received") {
			iCmd = i
		}
	}
	check(iCmd != -1 && iReady != -1 && iCmd < iReady,
		"events not newest-first (cmd@%d ready@%d)", iCmd, iReady)
	info("RMM: %d indexed events for %s (newest first; command receipt before 'agent ready')", len(er.Events), devID)

	// The SAME events live in both places: every indexed line's message
	// must also be present in the Loki result (cross-store consistency).
	lokiAll := strings.Join(lokiLines, "\n")
	for _, e := range er.Events {
		check(strings.Contains(lokiAll, e.Msg),
			"indexed event %q not found in Loki lines (stores disagree)", e.Msg)
	}
	info("consistency: every RMM-indexed event is also queryable in Loki")

	// Level filter works server-side.
	warnResp, err := fetchWarnEvents(httpAddr, opTok, devID)
	check(err == nil, "warn filter fetch: %v", err)
	for _, e := range warnResp.Events {
		check(strings.EqualFold(e.Level, "warn"), "warn filter leaked level %q", e.Level)
	}
	info("RMM: level=warn filter returned %d event(s), all warn", len(warnResp.Events))

	// ---- DoD #3: the /api mirror is auth-gated ----------------------------
	step("auth gate: /api/devices/{id}/events")
	resp, err := http.Get(httpAddr + "/api/devices/" + devID + "/events")
	check(err == nil, "api fetch: %v", err)
	resp.Body.Close()
	check(resp.StatusCode == http.StatusUnauthorized,
		"/api without operator token: status %d, want 401", resp.StatusCode)
	info("/api mirror is auth-gated (401 without an operator token)")

	step("PASS")
	fmt.Println("W6-1 DoD met: the REAL agent's structured log lines are queryable in Loki")
	fmt.Println("(device_id/job/level labels) AND the RMM surfaces the same events per device")
	fmt.Println("(Timescale log_events, newest first, level filter, auth-gated /api).")
}

// getWithCtx performs a GET bound to ctx.
func getWithCtx(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

// getBearerCtx is getWithCtx with an Authorization header; token=="" stays unauthed.
func getBearerCtx(ctx context.Context, url, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return http.DefaultClient.Do(req)
}

// loginOp hits the OPEN POST /api/login with the env admin and returns the
// short-lived operator JWT the C1-gated /admin/* + /api/* routes require.
func loginOp(ctx context.Context, base string) string {
	payload := `{"username":"admin","password":"admin"}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/login", strings.NewReader(payload))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		die("operator login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		die("operator login = %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Token == "" {
		die("operator login: no token in response: %v", err)
	}
	return out.Token
}

// repoRoot walks up from cwd to the directory containing agent/go.mod
// (go run is invoked from the server module dir).
func repoRoot() string {
	d, err := os.Getwd()
	if err != nil {
		die("getwd: %v", err)
	}
	start := d
	for i := 0; i < 6; i++ {
		if _, e1 := os.Stat(filepath.Join(d, "agent", "go.mod")); e1 == nil {
			return d
		}
		d = filepath.Dir(d)
	}
	die("could not locate repo root from %s", start)
	return ""
}

// ---- helpers ----------------------------------------------------------------

func mustTempDir() string {
	d, err := os.MkdirTemp("", "rmmway-logs-e2e-*")
	if err != nil {
		die("tempdir: %v", err)
	}
	return d
}

func mustRead(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return "<unreadable: " + err.Error() + ">"
	}
	return truncate(string(b), 4000)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func containsAll(hay []string, needles ...string) bool {
	all := strings.Join(hay, "\n")
	for _, n := range needles {
		if !strings.Contains(all, n) {
			return false
		}
	}
	return true
}

func containsMsgs(evs []store.LogEvent, needles ...string) bool {
	for _, n := range needles {
		found := false
		for _, e := range evs {
			if strings.Contains(e.Msg, n) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func fetchWarnEvents(base, token, devID string) (*struct {
	DeviceID string           `json:"device_id"`
	Events   []store.LogEvent `json:"events"`
}, error) {
	resp, err := getBearerCtx(context.Background(), base+"/admin/devices/"+devID+"/events?level=warn", token)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var er struct {
		DeviceID string           `json:"device_id"`
		Events   []store.LogEvent `json:"events"`
	}
	if err := json.Unmarshal(body, &er); err != nil {
		return nil, err
	}
	return &er, nil
}
