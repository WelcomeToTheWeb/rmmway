// Command caps is the W3-3 definition-of-done harness: "a command requiring
// a capability the agent lacks is refused even with a valid mTLS channel."
//
// It boots an IN-PROCESS rmmway server (fresh org root, capability issuer,
// plain + mTLS gRPC listeners, operator HTTP API) and two devices that each
// hold a real mTLS identity issued by that org root. The devices' fake
// agents mirror the real agent's W3-3 behavior: verify the command's
// capability token against the pinned org root (signature + device +
// capability + expiry) before acting, and report CommandResults
// (RECEIVED / final). Then:
//
//  1. run_script to A via the operator API (session holds the capability)
//     -> token verifies -> EXECUTED, SUCCEEDED recorded.
//  2. reboot to A -> the operator session LACKS rmmway.reboot -> 403,
//     nothing dispatched (the human-layer session/capability gate).
//  3. reboot to B with a token minted for device A (misbound — a
//     cross-device replay) over B's fully valid mTLS channel -> REFUSED,
//     NOT executed (THE DoD).
//  4. run_script to B with NO token -> REFUSED.
//  5. run_script to B with an EXPIRED token -> REFUSED.
//
// Usage: go run ./cmd/e2e/caps
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/ca"
	"github.com/welcometotheweb/rmmway/server/internal/caps"
	"github.com/welcometotheweb/rmmway/server/internal/httpapi"
	"github.com/welcometotheweb/rmmway/server/internal/ingest"
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

func die(f string, a ...any) {
	fmt.Printf("FAIL: "+f+"\n", a...)
	step("FAIL")
}

var stepName = "(init)"

func step(name string) {
	stepName = name
	fmt.Printf("\n== %s ==\n", name)
}
func info(f string, a ...any) { fmt.Printf("[%s] %s\n", stepName, fmt.Sprintf(f, a...)) }

// ---- fake agent (mirrors the real agent's W3-3 command path) ---------------

// fakeAgent is one device's W3-3 command handler over a live mTLS stream:
// verify the capability token against the pinned org root, act only inside
// the minted scope, report CommandResults. (The real agent binary does the
// same with agent/internal/{caps,exec,uplink}; its verifier is unit-tested
// there — this fake uses the server-side reference Verify for the wire.)
type fakeAgent struct {
	devID    string
	rootCert *x509.Certificate
	jwt      string
	ran      int
	ranMu    sync.Mutex
	// ready closes after the first heartbeat ack (stream registered for
	// dispatch on the server side).
	ready chan struct{}
}

func (fa *fakeAgent) countRan() int {
	fa.ranMu.Lock()
	defer fa.ranMu.Unlock()
	return fa.ran
}

// run drives the stream until ctx is done.
func (fa *fakeAgent) run(ctx context.Context, conn *grpc.ClientConn) error {
	client := agentv1.NewAgentServiceClient(conn)
	mdCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+fa.jwt))
	stream, err := client.Stream(mdCtx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}
	// Heartbeat (registers the stream for dispatch on the server).
	if err := stream.Send(&agentv1.StreamRequest{
		Payload: &agentv1.StreamRequest_Heartbeat{Heartbeat: &agentv1.Heartbeat{TimestampMs: time.Now().UnixMilli()}},
	}); err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	_, _ = stream.Recv() // the ack (server registered the stream)
	close(fa.ready)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		resp, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("recv: %w", err)
		}
		cmd := resp.GetCommand()
		if cmd == nil {
			continue
		}
		info("agent %s: command %s arrived (%s)", fa.devID, cmd.GetId(), actionName(cmd.GetAction()))
		capName, ok := caps.ForAction(cmd.GetAction())
		if !ok {
			_ = fa.sendResult(stream, cmd.GetId(), agentv1.CommandResult_UNSUPPORTED, "unsupported action")
			continue
		}
		token := ""
		switch a := cmd.GetAction().(type) {
		case *agentv1.Command_RunScript:
			token = a.RunScript.GetCapabilityToken()
		case *agentv1.Command_Reboot:
			token = a.Reboot.GetCapabilityToken()
		}
		if err := caps.Verify(token, fa.rootCert, fa.devID, capName, cmd.GetId(), time.Now()); err != nil {
			info("agent %s: REFUSED %s (%s)", fa.devID, cmd.GetId(), err)
			_ = fa.sendResult(stream, cmd.GetId(), agentv1.CommandResult_REFUSED, err.Error())
			continue
		}
		_ = fa.sendResult(stream, cmd.GetId(), agentv1.CommandResult_RECEIVED, "")
		fa.ranMu.Lock()
		fa.ran++
		fa.ranMu.Unlock()
		info("agent %s: EXECUTED %s (capability verified)", fa.devID, cmd.GetId())
		_ = fa.sendResult(stream, cmd.GetId(), agentv1.CommandResult_SUCCEEDED, "executed by fake agent")
	}
}

func (fa *fakeAgent) sendResult(stream agentv1.AgentService_StreamClient, cmdID string, st agentv1.CommandResult_Status, errMsg string) error {
	return stream.Send(&agentv1.StreamRequest{
		Payload: &agentv1.StreamRequest_CommandResult{CommandResult: &agentv1.CommandResult{
			CommandId:     cmdID,
			Status:        st,
			Error:         errMsg,
			CompletedAtMs: time.Now().UnixMilli(),
		}},
	})
}

func actionName(a any) string {
	switch a := a.(type) {
	case *agentv1.Command_RunScript:
		return "run_script(" + a.RunScript.GetLang() + ")"
	case *agentv1.Command_Reboot:
		return "reboot"
	default:
		return fmt.Sprintf("%T", a)
	}
}

// ---- in-process server ------------------------------------------------------

type rig struct {
	svc      *ingest.Service
	caMgr    *ca.Manager
	issuer   *caps.Issuer
	rootCert *x509.Certificate
	httpAddr string
	plain    string
	mtls     string
}

func bootServer(ttl time.Duration, adminCaps []string) (*rig, func()) {
	caMgr, err := ca.NewManager(ca.NewMemoryOrgStore(), time.Hour)
	if err != nil {
		die("org CA: %v", err)
	}
	issuer := caps.NewIssuer(caMgr.Root(), ttl)
	devices := store.NewMemoryDeviceStore()
	svc := ingest.NewService(ingest.Config{
		JWTSecret: []byte("e2e-caps-secret"),
		OrgCA:     caMgr,
		Caps:      issuer,
	}, store.NewMemoryMetricsSink(10000), devices)

	plainLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		die("plain listen: %v", err)
	}
	plainSrv := grpc.NewServer(grpc.UnaryInterceptor(svc.JWTInterceptor))
	agentv1.RegisterAgentServiceServer(plainSrv, svc)
	go func() { _ = plainSrv.Serve(plainLis) }()

	mtlsCfg, err := caMgr.TLSConfig([]string{"localhost", "127.0.0.1"})
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

	apiSrv := httpapi.New(httpapi.Config{
		Devices:       devices,
		JWTSecret:     []byte("e2e-caps-secret"),
		AdminUser:     "admin",
		AdminPassword: "admin",
		MintBootstrap: svc.MintBootstrapToken,
		Dispatch:      svc.Dispatcher().Dispatch,
		CommandState: func(deviceID string) ([]*agentv1.Command, []*agentv1.CommandResult) {
			return svc.Dispatcher().PendingFor(deviceID), svc.Dispatcher().ResultsFor(deviceID)
		},
		AdminCaps: adminCaps,
	})
	mux := http.NewServeMux()
	apiSrv.Register(mux)
	httpLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		die("http listen: %v", err)
	}
	go func() { _ = http.Serve(httpLis, mux) }()

	return &rig{
		svc:      svc,
		caMgr:    caMgr,
		issuer:   issuer,
		rootCert: caMgr.Root().Cert(),
		httpAddr: "http://" + httpLis.Addr().String(),
		plain:    plainLis.Addr().String(),
		mtls:     mtlsLis.Addr().String(),
	}, func() {
		plainSrv.Stop()
		mtlsSrv.Stop()
	}
}

// enrollDevice mints a bootstrap token + enrolls over the PLAIN port,
// returning the issued mTLS identity (leaf + key + root PEM) and JWT.
func (r *rig) enrollDevice(ctx context.Context, hostname string) (devID, jwt string, leaf, key, rootPEM []byte) {
	var boot struct {
		BootstrapToken string `json:"bootstrap_token"`
		DeviceID       string `json:"device_id"`
	}
	resp, err := http.Post(r.httpAddr+"/admin/bootstrap", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		die("bootstrap: %v", err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&boot)
	resp.Body.Close()
	if boot.BootstrapToken == "" {
		die("bootstrap: empty token")
	}
	conn, err := grpc.NewClient(r.plain, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		die("dial plain: %v", err)
	}
	defer conn.Close()
	en, err := agentv1.NewAgentServiceClient(conn).Enroll(ctx, &agentv1.EnrollRequest{
		BootstrapToken: boot.BootstrapToken,
		Hostname:       hostname,
		Os:             "linux",
		Arch:           "amd64",
		AgentVersion:   "0.0.0-e2e-caps",
	})
	if err != nil {
		die("enroll: %v", err)
	}
	if en.GetLeafCertPem() == "" || en.GetLeafKeyPem() == "" || en.GetOrgRootCaPem() == "" {
		die("enroll: no mTLS identity issued (server has no org CA?)")
	}
	return en.GetDeviceId(), en.GetJwt(), []byte(en.GetLeafCertPem()), []byte(en.GetLeafKeyPem()), []byte(en.GetOrgRootCaPem())
}

// mtlsConn dials the mTLS port presenting the device's leaf, trusting only
// the org root (exactly what the real agent does).
func mtlsConn(leaf, key, rootPEM []byte, addr string) (*grpc.ClientConn, error) {
	kp, err := tls.X509KeyPair(leaf, key)
	if err != nil {
		return nil, fmt.Errorf("leaf keypair: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootPEM) {
		return nil, fmt.Errorf("no cert in org root PEM")
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{kp},
		RootCAs:      roots,
		ServerName:   "127.0.0.1",
	}
	return grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
}

// rootCertFromPEM parses a root cert PEM.
func rootCertFromPEM(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}

// login returns the operator session token.
func login(httpAddr, user, pass string) string {
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	resp, err := http.Post(httpAddr+"/api/login", "application/json", bytes.NewReader(body))
	if err != nil {
		die("login: %v", err)
	}
	var out struct {
		Token        string   `json:"token"`
		Capabilities []string `json:"capabilities"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != 200 || out.Token == "" {
		die("login: status %d, no token", resp.StatusCode)
	}
	return out.Token
}

// dispatch posts a command via the operator API and returns (status, body).
func dispatch(httpAddr, token, devID string, body any) (int, map[string]any) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", httpAddr+"/api/devices/"+devID+"/commands", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		die("dispatch: %v", err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	return resp.StatusCode, out
}

// finalResult polls the dispatcher for cmdID's final (terminal) result.
func finalResult(svc *ingest.Service, cmdID string) *agentv1.CommandResult {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if res, ok := svc.Dispatcher().Result(cmdID); ok && isTerminal(res.GetStatus()) {
			return res
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

func isTerminal(st agentv1.CommandResult_Status) bool {
	switch st {
	case agentv1.CommandResult_SUCCEEDED, agentv1.CommandResult_FAILED,
		agentv1.CommandResult_TIMED_OUT, agentv1.CommandResult_REFUSED,
		agentv1.CommandResult_UNSUPPORTED:
		return true
	}
	return false
}

// b64 is a base64 std helper for the script payloads.
func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// ---- the harness ------------------------------------------------------------

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	step("boot in-process server (org root + capability issuer + plain & mTLS gRPC + operator API)")
	// Admin session holds ONLY run_script: reboot is a higher-impact
	// capability the session lacks (the 403 gate under test).
	r, stop := bootServer(10*time.Second, []string{caps.CapRunScript})
	defer stop()
	info("server up: plain=%s mtls=%s http=%s (cap TTL %s, admin caps [rmmway.run_script])",
		r.plain, r.mtls, r.httpAddr, r.issuer.TTL())

	step("enroll device A + open its mTLS stream (valid mTLS channel)")
	devA, jwtA, leafA, keyA, rootA := r.enrollDevice(ctx, "caps-e2e-a")
	rootCertA, err := rootCertFromPEM(rootA)
	if err != nil {
		die("parse root A: %v", err)
	}
	connA, err := mtlsConn(leafA, keyA, rootA, r.mtls)
	if err != nil {
		die("mtls dial A: %v", err)
	}
	defer connA.Close()
	agentA := &fakeAgent{devID: devA, rootCert: rootCertA, jwt: jwtA, ready: make(chan struct{})}
	go func() {
		if err := agentA.run(ctx, connA); err != nil && ctx.Err() == nil {
			info("agent A stream ended: %v", err)
		}
	}()
	// Wait until A's stream is live (heartbeat acked = registered).
	waitReady(agentA.ready)
	info("device %s streaming over mTLS (leaf verified by the org root at the handshake)", devA)

	step("enroll device B + open its mTLS stream (another valid mTLS channel)")
	devB, jwtB, leafB, keyB, rootB := r.enrollDevice(ctx, "caps-e2e-b")
	rootCertB, err := rootCertFromPEM(rootB)
	if err != nil {
		die("parse root B: %v", err)
	}
	connB, err := mtlsConn(leafB, keyB, rootB, r.mtls)
	if err != nil {
		die("mtls dial B: %v", err)
	}
	defer connB.Close()
	agentB := &fakeAgent{devID: devB, rootCert: rootCertB, jwt: jwtB, ready: make(chan struct{})}
	go func() {
		if err := agentB.run(ctx, connB); err != nil && ctx.Err() == nil {
			info("agent B stream ended: %v", err)
		}
	}()
	waitReady(agentB.ready)
	info("device %s streaming over mTLS", devB)

	opTok := login(r.httpAddr, "admin", "admin")
	info("operator session minted (caps: [rmmway.run_script])")

	// ---- 1. happy path: in-scope command executes -------------------------
	step("1. run_script to A (session holds the capability) -> verified + EXECUTED + SUCCEEDED")
	code, out := dispatch(r.httpAddr, opTok, devA, map[string]any{
		"action": "run_script", "lang": "sh", "script": b64("echo caps-ok"),
	})
	if code != 200 {
		die("dispatch run_script: status %d (%v)", code, out)
	}
	cmdA, _ := out["command_id"].(string)
	res := finalResult(r.svc, cmdA)
	if res == nil || res.GetStatus() != agentv1.CommandResult_SUCCEEDED {
		die("expected SUCCEEDED result, got %+v", res)
	}
	if agentA.countRan() != 1 {
		die("expected exactly 1 execution on A, got %d", agentA.countRan())
	}
	info("PASS: token verified by agent A (org root, device, capability, expiry) -> executed, SUCCEEDED recorded (cmd %s)", cmdA)

	// The audit endpoint serves the recorded state (status is the proto
	// enum ordinal in JSON; SUCCEEDED == 3).
	resp, err := http.Get(r.httpAddr + "/admin/devices/" + devA + "/commands")
	if err != nil {
		die("commands audit: %v", err)
	}
	audit, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		die("audit endpoint: status %d, body %s", resp.StatusCode, audit)
	}
	var auditOut struct {
		DeviceID string `json:"device_id"`
		Pending  []any  `json:"pending"`
		Results  []struct {
			CommandID string                       `json:"command_id"`
			Status    agentv1.CommandResult_Status `json:"status"`
		} `json:"results"`
	}
	if err := json.Unmarshal(audit, &auditOut); err != nil {
		die("audit endpoint: bad JSON %v (%s)", err, audit)
	}
	found := false
	for _, rr := range auditOut.Results {
		if rr.CommandID == cmdA && rr.Status == agentv1.CommandResult_SUCCEEDED {
			found = true
		}
	}
	if !found {
		die("audit endpoint: no SUCCEEDED result for %s (body %s)", cmdA, audit)
	}
	if len(auditOut.Pending) != 0 {
		die("audit endpoint: command still pending: %s", audit)
	}
	info("PASS: GET /admin/devices/%s/commands shows the SUCCEEDED result for %s", devA, cmdA)

	// ---- 2. operator session gate: 403 without the capability ------------
	step("2. reboot to A -> 403 (session lacks rmmway.reboot; nothing dispatched)")
	code, out = dispatch(r.httpAddr, opTok, devA, map[string]any{"action": "reboot"})
	if code != 403 {
		die("expected 403, got %d (%v)", code, out)
	}
	if agentA.countRan() != 1 {
		die("403 path must not execute, A ran %d times", agentA.countRan())
	}
	info("PASS: session without rmmway.reboot got 403: %v", out["error"])

	// ---- 3. THE DoD: misbound token over a fully valid mTLS channel -------
	step("3. reboot to B with a token minted for device A (cross-device replay) -> REFUSED, NOT executed")
	misbound, err := r.issuer.Mint(devA, caps.CapReboot, "cmd-replay")
	if err != nil {
		die("mint misbound: %v", err)
	}
	cmdB, err := r.svc.Dispatcher().DispatchWith(devB, misbound, &agentv1.Command_Reboot{Reboot: &agentv1.Reboot{DelayS: 0}})
	if err != nil {
		die("dispatch misbound: %v", err)
	}
	res = finalResult(r.svc, cmdB)
	if res == nil || res.GetStatus() != agentv1.CommandResult_REFUSED {
		die("expected REFUSED, got %+v", res)
	}
	if !strings.Contains(res.GetError(), "bound to device") {
		die("refusal reason: %q", res.GetError())
	}
	if agentB.countRan() != 0 {
		die("refused command was executed %d times on B", agentB.countRan())
	}
	info("PASS: valid mTLS channel + validly-signed token bound to the WRONG device -> REFUSED (%q), B did not act", res.GetError())

	// ---- 4. missing token --------------------------------------------------
	step("4. run_script to B with NO token -> REFUSED")
	cmdB2, err := r.svc.Dispatcher().DispatchWith(devB, "", &agentv1.Command_RunScript{
		RunScript: &agentv1.RunScript{Lang: "sh", ScriptB64: b64("echo x")},
	})
	if err != nil {
		die("dispatch missing: %v", err)
	}
	res = finalResult(r.svc, cmdB2)
	if res == nil || res.GetStatus() != agentv1.CommandResult_REFUSED {
		die("expected REFUSED (missing token), got %+v", res)
	}
	info("PASS: tokenless command on a capability-enabled estate -> REFUSED (%q)", res.GetError())

	// ---- 5. expired token --------------------------------------------------
	step("5. run_script to B with an EXPIRED token -> REFUSED")
	// Expired PAST the 10s leeway: mint with the issuer clock shifted 30s
	// into the past and a 15s TTL -> exp is 15s ago.
	short := caps.NewIssuer(r.caMgr.Root(), 15*time.Second).WithNow(func() time.Time {
		return time.Now().Add(-30 * time.Second)
	})
	expired, err := short.Mint(devB, caps.CapRunScript, "cmd-expired")
	if err != nil {
		die("mint expired: %v", err)
	}
	cmdB3, err := r.svc.Dispatcher().DispatchWith(devB, expired, &agentv1.Command_RunScript{
		RunScript: &agentv1.RunScript{Lang: "sh", ScriptB64: b64("echo x")},
	})
	if err != nil {
		die("dispatch expired: %v", err)
	}
	res = finalResult(r.svc, cmdB3)
	if res == nil || res.GetStatus() != agentv1.CommandResult_REFUSED {
		die("expected REFUSED (expired), got %+v", res)
	}
	info("PASS: expired capability token -> REFUSED (%q)", res.GetError())

	step("PASS: W3-3 DoD — per-action capability tokens gate every command; a valid mTLS channel alone cannot make an agent act beyond its minted scope")
}

// waitReady blocks until the fake agent's stream is registered for dispatch
// (its first heartbeat was acked) or the deadline passes.
func waitReady(ready <-chan struct{}) {
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		die("agent stream not live after 5s")
	}
}
