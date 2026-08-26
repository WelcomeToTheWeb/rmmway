// Command adddevice is the "Add a device" E2E (simplify the agent install to
// add devices).
//
// It proves the WHOLE new flow on a real in-process server + the REAL agent
// binary, self-contained (no backing services):
//
//  1. MINT — the operator "Add a device" action (POST /api/bootstrap) is
//     auth-gated (401 without a token, 200 with one); the open
//     /admin/bootstrap still works for machine callers.
//  2. ENROLL over the operator's HTTPS origin (POST /agent/enroll) — the
//     architectural fix. A fresh agent has no mTLS material yet, so it proves
//     the one-time bootstrap token HERE instead of over the internal-only
//     plain gRPC bootstrap port. The minted identity (device_id + agent JWT)
//     and the org-root mTLS leaf are verified (the leaf is genuinely signed by
//     the returned org root).
//  3. ONE-TIME — the same token cannot enroll twice (a cloned agent with a
//     stolen token gets a 403, not a second identity).
//  4. REAL AGENT — the built agent binary runs with the PLAIN gRPC bootstrap
//     port pointed at a DEAD address, so it is FORCED to enroll over the
//     operator origin (HTTP). It then opens its mTLS uplink (port 50052 model)
//     and the device goes online — proving a remote machine needs only the
//     operator origin + the mTLS gRPC port open, not the internal 50051.
//
// Usage: cd server && go run ./cmd/e2e/adddevice
package main

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/ca"
	"github.com/welcometotheweb/rmmway/server/internal/caps"
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

// repoRoot walks up from cwd to the directory containing agent/ + server/.
func repoRoot() string {
	d, err := os.Getwd()
	if err != nil {
		die("cwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, e1 := os.Stat(filepath.Join(d, "agent", "go.mod")); e1 == nil {
			if _, e2 := os.Stat(filepath.Join(d, "server", "go.mod")); e2 == nil {
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

// ---- in-process server ------------------------------------------------------

type srv struct {
	httpAddr string
	grpcAddr string
	mtlsAddr string
	devices  *store.MemoryDeviceStore
	stop     func()
}

// startServer boots a real in-process RMMWay server: the operator HTTP API
// (with /agent/enroll + /api/bootstrap), the plain gRPC bootstrap listener, and
// the mTLS gRPC agent listener — all on loopback ephemeral ports, in-memory.
func startServer() *srv {
	devices := store.NewMemoryDeviceStore()
	metrics := store.NewMemoryMetricsSink(100000)

	caMgr, err := ca.NewManager(ca.NewMemoryOrgStore(), 0)
	if err != nil {
		die("org CA: %v", err)
	}
	capsIssuer := caps.NewIssuer(caMgr.Root(), 10*time.Minute)
	svc := ingest.NewService(ingest.Config{
		JWTSecret:            []byte("adddevice-e2e-secret"),
		OrgCA:                caMgr,
		Caps:                 capsIssuer,
		DefaultHeartbeatIntS: 1,
		DefaultMetricIntS:    1,
	}, metrics, devices)

	jwtSecret := []byte("adddevice-e2e-secret")

	// Plain gRPC bootstrap listener (50051 model).
	plainLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		die("plain grpc listen: %v", err)
	}
	plainSrv := grpc.NewServer(grpc.UnaryInterceptor(svc.JWTInterceptor))
	agentv1.RegisterAgentServiceServer(plainSrv, svc)
	go func() { _ = plainSrv.Serve(plainLis) }()

	// mTLS gRPC agent listener (50052 model) — requires an org-root leaf.
	mtlsCfg, err := caMgr.TLSConfig([]string{"127.0.0.1", "localhost"})
	if err != nil {
		die("mtls cfg: %v", err)
	}
	mtlsLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		die("mtls listen: %v", err)
	}
	mtlsSrv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(mtlsCfg)),
		grpc.UnaryInterceptor(svc.JWTInterceptor),
	)
	agentv1.RegisterAgentServiceServer(mtlsSrv, svc)
	go func() { _ = mtlsSrv.Serve(mtlsLis) }()

	// Operator HTTP API (with the new /agent/enroll + /api/bootstrap).
	apiSrv := httpapi.New(httpapi.Config{
		Devices:       devices,
		JWTSecret:     jwtSecret,
		AdminUser:     "admin",
		AdminPassword: "e2e-pass",
		MintBootstrap: svc.MintBootstrapToken,
		Enroll:        svc.Enroll,
	})
	mux := http.NewServeMux()
	apiSrv.Register(mux)
	httpLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		die("http listen: %v", err)
	}
	httpSrv := &http.Server{Handler: mux}
	go func() { _ = httpSrv.Serve(httpLis) }()

	return &srv{
		httpAddr: "http://" + httpLis.Addr().String(),
		grpcAddr: plainLis.Addr().String(),
		mtlsAddr: mtlsLis.Addr().String(),
		devices:  devices,
		stop: func() {
			_ = httpSrv.Close()
			plainSrv.Stop()
			mtlsSrv.Stop()
		},
	}
}

// ---- HTTP helpers -----------------------------------------------------------

func doJSON(method, url string, body any, token string) (int, any) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		die("req %s %s: %v", method, url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		die("do %s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	var out any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func login(s *srv) string {
	code, out := doJSON(http.MethodPost, s.httpAddr+"/api/login",
		map[string]string{"username": "admin", "password": "e2e-pass"}, "")
	check(code == http.StatusOK, "login = %d, want 200: %v", code, out)
	tok := mustStr(out, "token")
	check(tok != "", "login returned no token: %v", out)
	return tok
}

// parsePEMCert pulls the first CERTIFICATE/PRIVATE-less PEM block from a PEM
// blob and parses it as an x509 cert.
func parseCert(pemBytes []byte) *x509.Certificate {
	for {
		var block *pem.Block
		block, pemBytes = pem.Decode(pemBytes)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				die("parse cert: %v", err)
			}
			return cert
		}
	}
	die("no CERTIFICATE block in PEM")
	return nil
}

// runAgent runs the agent binary `run`, streaming its combined output into
// buf (guarded), and returns the *exec.Cmd so the caller can wait/kill.
func runAgent(agentBin string, env []string) (*exec.Cmd, *sync.Mutex, *bytes.Buffer) {
	cmd := exec.Command(agentBin, "run")
	cmd.Env = append(os.Environ(), env...)
	var buf bytes.Buffer
	var mu sync.Mutex
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		die("start agent: %v", err)
	}
	return cmd, &mu, &buf
}

func bufContains(mu *sync.Mutex, buf *bytes.Buffer, sub string) bool {
	mu.Lock()
	defer mu.Unlock()
	return strings.Contains(buf.String(), sub)
}

func main() {
	root := repoRoot()
	work, err := os.MkdirTemp("", "rmmway-adddevice-e2e-")
	if err != nil {
		die("temp: %v", err)
	}
	defer os.RemoveAll(work)

	fmt.Println("================================================================")
	fmt.Println(" ADD A DEVICE E2E — the simplified agent install, end to end")
	fmt.Println("================================================================")

	step("0. build the real agent binary (host GOOS/GOARCH)")
	agentBin := filepath.Join(work, "rmmway-agent")
	buildCmd := exec.Command("go", "build", "-o", agentBin, "./cmd/agent")
	buildCmd.Dir = filepath.Join(root, "agent")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		die("build agent: %v\n%s", err, out)
	}
	info("built %s", filepath.Base(agentBin))

	s := startServer()
	defer s.stop()
	info("in-process server: http=%s grpc(plain)=%s grpc(mTLS)=%s", s.httpAddr, s.grpcAddr, s.mtlsAddr)

	operator := login(s)
	info("operator token minted (admin/e2e-pass)")

	step("1. MINT — /api/bootstrap is auth-gated; /admin/bootstrap stays open")
	// No token -> 401.
	code, _ := doJSON(http.MethodPost, s.httpAddr+"/api/bootstrap", map[string]any{}, "")
	check(code == http.StatusUnauthorized, "/api/bootstrap no token = %d, want 401", code)
	// Authed -> 200 with a token + pre-allocated device id.
	code, out := doJSON(http.MethodPost, s.httpAddr+"/api/bootstrap", map[string]any{}, operator)
	check(code == http.StatusOK, "/api/bootstrap authed = %d, want 200: %v", code, out)
	token := mustStr(out, "bootstrap_token")
	prealloc := mustStr(out, "device_id")
	check(token != "" && prealloc != "", "mint missing fields: %v", out)
	info("minted bootstrap token=%s (pre-alloc device %s)", short(token), prealloc)
	// The open /admin/bootstrap still works (machine callers / e2e).
	code, aout := doJSON(http.MethodPost, s.httpAddr+"/admin/bootstrap", map[string]any{}, "")
	check(code == http.StatusOK, "/admin/bootstrap = %d, want 200", code)
	check(mustStr(aout, "bootstrap_token") != "", "admin bootstrap missing token: %v", aout)
	info("open /admin/bootstrap still mints (machine callers)")

	step("2. ENROLL over the operator origin (POST /agent/enroll)")
	code, eout := doJSON(http.MethodPost, s.httpAddr+"/agent/enroll", map[string]any{
		"bootstrap_token": token,
		"hostname":        "demo-host",
		"os":              "linux",
		"arch":            "amd64",
		"agent_version":   "0.1.0",
		"interfaces":      []string{"eth0:10.0.0.5/24"},
	}, "")
	check(code == http.StatusOK, "enroll = %d, want 200: %v", code, eout)
	devID := mustStr(eout, "device_id")
	check(devID == prealloc, "enrolled device %q != pre-allocated %q (token must bind to it)", devID, prealloc)
	leaf := mustStr(eout, "leaf_cert_pem")
	rootCA := mustStr(eout, "org_root_ca_pem")
	check(mustStr(eout, "jwt") != "", "enroll returned no jwt")
	check(leaf != "" && rootCA != "", "enroll returned no mTLS PEMs: %v", eout)
	info("enrolled %s over HTTP (jwt + mTLS leaf + org root)", devID)

	// The leaf must genuinely be signed by the org root the agent pins.
	leafCert := parseCert([]byte(leaf))
	rootCert := parseCert([]byte(rootCA))
	if err := leafCert.CheckSignatureFrom(rootCert); err != nil {
		die("leaf NOT signed by the returned org root: %v", err)
	}
	info("leaf verified: signed by the returned org root (the trust anchor the agent pins)")

	// The device must be in the (auth-gated) device list, online.
	code, dout := doJSON(http.MethodGet, s.httpAddr+"/api/devices", nil, operator)
	check(code == http.StatusOK, "device list = %d, want 200", code)
	list, _ := dout.([]any)
	found := false
	for _, d := range list {
		m, _ := d.(map[string]any)
		if m["id"] == devID {
			found = true
			check(m["online"] == true, "enrolled device not online: %v", m)
		}
	}
	check(found, "enrolled device %s not in the device list (%d devices)", devID, len(list))
	info("device %s is present + online in the operator list", devID)

	step("3. ONE-TIME — the same token cannot enroll twice")
	code, r2 := doJSON(http.MethodPost, s.httpAddr+"/agent/enroll", map[string]any{
		"bootstrap_token": token,
		"hostname":        "clone-host",
		"os":              "linux",
		"arch":            "amd64",
	}, "")
	check(code == http.StatusForbidden, "re-enroll with same token = %d, want 403: %v", code, r2)
	info("a cloned agent reusing the token is refused (403) — no second identity")

	step("4. REAL AGENT — forced to enroll over HTTP (plain gRPC pointed at a DEAD port)")
	// Mint a fresh token for the real agent, then run the built binary with:
	//   - RMMWAY_SERVER      -> the operator origin (HTTP enroll works here)
	//   - RMMWAY_GRPC_ADDR   -> 127.0.0.1:1 (DEAD): the plain gRPC bootstrap
	//                           channel CANNOT be used, so the agent MUST have
	//                           enrolled over the operator origin.
	//   - RMMWAY_GRPC_MTLS_ADDR -> the mTLS listener (the 50052 model).
	code, gout := doJSON(http.MethodPost, s.httpAddr+"/api/bootstrap", map[string]any{}, operator)
	check(code == http.StatusOK, "agent mint = %d, want 200", code)
	agentToken := mustStr(gout, "bootstrap_token")
	agentPrealloc := mustStr(gout, "device_id")

	agentEnv := []string{
		"RMMWAY_SERVER=" + s.httpAddr,
		"RMMWAY_BOOTSTRAP_TOKEN=" + agentToken,
		"RMMWAY_GRPC_ADDR=127.0.0.1:1", // DEAD — forces the HTTP enroll path
		"RMMWAY_GRPC_MTLS_ADDR=" + s.mtlsAddr,
		"RMMWAY_IDENTITY=" + filepath.Join(work, "agent-identity.json"),
		"RMMWAY_LOG_FILE=" + filepath.Join(work, "agent.jsonl"),
		"RMMWAY_AUTO_UPDATE=off",
	}
	cmd, mu, buf := runAgent(agentBin, agentEnv)
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
	}()
	// Wait for the agent to report its mTLS uplink is running.
	waitUntil(20*time.Second, func() bool { return bufContains(mu, buf, "uplink running") },
		"agent mTLS uplink to be running")
	check(bufContains(mu, buf, "(mTLS)"), "agent did not report the mTLS uplink: %s", snap(mu, buf))
	check(bufContains(mu, buf, "via the operator origin (HTTP)") || bufContains(mu, buf, "operator origin (HTTP)"),
		"agent did not report HTTP enroll: %s", snap(mu, buf))
	info("agent enrolled over the operator origin (HTTP) — the dead plain-gRPC port was not used")

	// The real agent's device (its own minted pre-allocated id) is online.
	waitUntil(10*time.Second, func() bool {
		_, dout := doJSON(http.MethodGet, s.httpAddr+"/api/devices", nil, operator)
		list, _ := dout.([]any)
		for _, d := range list {
			m, _ := d.(map[string]any)
			if m["id"] == agentPrealloc && m["online"] == true {
				return true
			}
		}
		return false
	}, "real agent's device to be online")
	_, dout = doJSON(http.MethodGet, s.httpAddr+"/api/devices", nil, operator)
	list, _ = dout.([]any)
	agentOnline := false
	for _, d := range list {
		m, _ := d.(map[string]any)
		if m["id"] == agentPrealloc && m["online"] == true {
			agentOnline = true
		}
	}
	check(agentOnline, "real agent's device %s not online (devices: %d)", agentPrealloc, len(list))
	info("real agent %s is online over the mTLS uplink", agentPrealloc)

	step("PASS")
	fmt.Println("================================================================")
	fmt.Println(" ADD A DEVICE DoD MET:")
	fmt.Println("   (1) the operator mints a token from the UI action (auth-gated)")
	fmt.Println("   (2) a fresh agent enrolls over the operator's HTTPS origin")
	fmt.Println("       (leaf signed by the pinned org root) — token is one-time")
	fmt.Println("   (3) the REAL agent, with the plain gRPC port DEAD, still")
	fmt.Println("       enrolls (via HTTP) and comes online over the mTLS port")
	fmt.Println("   => a remote machine needs only the operator origin + the")
	fmt.Println("      mTLS gRPC port open (50051 stays internal).")
	fmt.Println("================================================================")
}

// waitUntil polls cond every 50ms until it is true or the deadline passes.
func waitUntil(d time.Duration, cond func() bool, what string) {
	t0 := time.Now()
	for time.Since(t0) < d {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	die("timed out (%s) waiting for: %s", d, what)
}

func mustStr(v any, k string) string {
	m, _ := v.(map[string]any)
	s, _ := m[k].(string)
	return s
}
func short(s string) string {
	if len(s) > 14 {
		return s[:11] + "…"
	}
	return s
}
func snap(mu *sync.Mutex, buf *bytes.Buffer) string {
	mu.Lock()
	defer mu.Unlock()
	s := buf.String()
	if len(s) > 800 {
		return s[len(s)-800:]
	}
	return s
}
