// Command groups is the B-2 definition-of-done harness: "an operator can
// filter to a specific tag and execute a single command that fans out to
// all matched agents."
//
// It boots an IN-PROCESS rmmway server (fresh org root, capability issuer,
// plain + mTLS gRPC listeners, operator HTTP API) and three devices with
// real mTLS identities. Two of them run fake agents mirroring the real
// agent's W3-3 command path (verify the per-command capability token
// against the pinned org root before acting); the third stays OFFLINE (no
// stream). Then:
//
//  1. the operator tags the cohort through the API:
//     PATCH /api/devices/{id} (tag:web on all three, +tag:prod on A),
//     invalid tag shapes are 400, unknown devices 404.
//  2. ONE bulk command to the tag group:
//     POST /api/devices/bulk/commands {tag:"web", action:"run_script", …}
//     -> requested=3, pushed=2 (A + B, each with a per-device command id),
//     offline=[C]; BOTH agents verify their own capability token and run
//     exactly once -> SUCCEEDED.
//  3. bulk to a tag nobody carries -> 404.
//  4. bulk reboot to the group -> 403 (the session lacks rmmway.reboot;
//     nothing is dispatched).
//
// Usage: go run ./cmd/e2e/groups
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
	"net"
	"net/http"
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

type fakeAgent struct {
	devID string
	root  *x509.Certificate
	jwt   string
	ran   int
	mu    sync.Mutex
	ready chan struct{}
}

func (fa *fakeAgent) countRan() int {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	return fa.ran
}

func (fa *fakeAgent) run(ctx context.Context, conn *grpc.ClientConn) error {
	client := agentv1.NewAgentServiceClient(conn)
	mdCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+fa.jwt))
	stream, err := client.Stream(mdCtx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}
	if err := stream.Send(&agentv1.StreamRequest{
		Payload: &agentv1.StreamRequest_Heartbeat{Heartbeat: &agentv1.Heartbeat{TimestampMs: time.Now().UnixMilli()}},
	}); err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	_, _ = stream.Recv()
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
		if err := caps.Verify(token, fa.root, fa.devID, capName, cmd.GetId(), time.Now()); err != nil {
			info("agent %s: REFUSED %s (%s)", fa.devID, cmd.GetId(), err)
			_ = fa.sendResult(stream, cmd.GetId(), agentv1.CommandResult_REFUSED, err.Error())
			continue
		}
		_ = fa.sendResult(stream, cmd.GetId(), agentv1.CommandResult_RECEIVED, "")
		fa.mu.Lock()
		fa.ran++
		fa.mu.Unlock()
		info("agent %s: EXECUTED %s (capability token verified for THIS device)", fa.devID, cmd.GetId())
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
		JWTSecret: []byte("e2e-groups-secret"),
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
		JWTSecret:     []byte("e2e-groups-secret"),
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
// returning the device id, JWT, and the mTLS identity (leaf + key + root PEM).
func (r *rig) enrollDevice(ctx context.Context, opTok, hostname string) (devID, jwt string, leaf, key, rootPEM []byte) {
	var boot struct {
		BootstrapToken string `json:"bootstrap_token"`
		DeviceID       string `json:"device_id"`
	}
	req, _ := http.NewRequest("POST", r.httpAddr+"/admin/bootstrap", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+opTok)
	resp, err := http.DefaultClient.Do(req)
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
		AgentVersion:   "0.0.0-e2e-groups",
	})
	if err != nil {
		die("enroll: %v", err)
	}
	if en.GetLeafCertPem() == "" || en.GetLeafKeyPem() == "" || en.GetOrgRootCaPem() == "" {
		die("enroll: no mTLS identity issued")
	}
	return en.GetDeviceId(), en.GetJwt(), []byte(en.GetLeafCertPem()), []byte(en.GetLeafKeyPem()), []byte(en.GetOrgRootCaPem())
}

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

func rootCertFromPEM(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}

// ---- operator API helpers ---------------------------------------------------

func login(httpAddr, user, pass string) string {
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	resp, err := http.Post(httpAddr+"/api/login", "application/json", bytes.NewReader(body))
	if err != nil {
		die("login: %v", err)
	}
	var out struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != 200 || out.Token == "" {
		die("login: status %d, no token", resp.StatusCode)
	}
	return out.Token
}

// patchTags replaces a device's tag list (B-2) and returns (status, body).
func patchTags(httpAddr, token, devID string, tags []string) (int, map[string]any) {
	b, _ := json.Marshal(map[string]any{"tags": tags})
	req, _ := http.NewRequest("PATCH", httpAddr+"/api/devices/"+devID, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		die("patchTags: %v", err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	return resp.StatusCode, out
}

// bulk posts a group fan-out and returns (status, body).
func bulk(httpAddr, token string, body any) (int, map[string]any) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", httpAddr+"/api/devices/bulk/commands", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		die("bulk: %v", err)
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

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func waitReady(ready <-chan struct{}) {
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		die("agent stream not live after 5s")
	}
}

// ---- the harness ------------------------------------------------------------

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	step("boot in-process server (org root + capability issuer + plain & mTLS gRPC + operator API)")
	// Admin session holds ONLY run_script: the reboot fan-out must 403.
	r, stop := bootServer(30*time.Second, []string{caps.CapRunScript})
	defer stop()
	info("server up: plain=%s mtls=%s http=%s (admin caps [rmmway.run_script])",
		r.plain, r.mtls, r.httpAddr)

	opTok := login(r.httpAddr, "admin", "admin")
	info("operator session minted (caps: [rmmway.run_script])")

	step("enroll three devices; A + B stream over mTLS, C stays offline")
	enroll := func(hostname string) (devID, jwt string, leaf, key, rootPEM []byte) {
		return r.enrollDevice(ctx, opTok, hostname)
	}
	devA, jwtA, leafA, keyA, rootA := enroll("grp-e2e-a")
	rootCertA, err := rootCertFromPEM(rootA)
	if err != nil {
		die("parse root A: %v", err)
	}
	connA, err := mtlsConn(leafA, keyA, rootA, r.mtls)
	if err != nil {
		die("mtls dial A: %v", err)
	}
	defer connA.Close()
	agentA := &fakeAgent{devID: devA, root: rootCertA, jwt: jwtA, ready: make(chan struct{})}
	go func() { _ = agentA.run(ctx, connA) }()
	waitReady(agentA.ready)

	devB, jwtB, leafB, keyB, rootB := enroll("grp-e2e-b")
	rootCertB, err := rootCertFromPEM(rootB)
	if err != nil {
		die("parse root B: %v", err)
	}
	connB, err := mtlsConn(leafB, keyB, rootB, r.mtls)
	if err != nil {
		die("mtls dial B: %v", err)
	}
	defer connB.Close()
	agentB := &fakeAgent{devID: devB, root: rootCertB, jwt: jwtB, ready: make(chan struct{})}
	go func() { _ = agentB.run(ctx, connB) }()
	waitReady(agentB.ready)

	devC, _, _, _, _ := enroll("grp-e2e-c")
	info("devices: A=%s (online) B=%s (online) C=%s (OFFLINE, no stream)", devA, devB, devC)

	// ---- 1. operator tagging ------------------------------------------------
	var outB map[string]any
step("1. operator tags the cohort: PATCH /api/devices/{id} (web on all, +prod on A)")
	code, out := patchTags(r.httpAddr, opTok, devA, []string{"web", "prod"})
	if code != 200 {
		die("tag A: status %d (%v)", code, out)
	}
	code, outB = patchTags(r.httpAddr, opTok, devB, []string{"Web"}) // normalized to "web"
	if code != 200 {
		die("tag B: status %d (%v)", code, outB)
	}
	code, out = patchTags(r.httpAddr, opTok, devC, []string{"web"})
	if code != 200 {
		die("tag C: status %d (%v)", code, out)
	}
	info("PASS: tags persisted (B sent %q — the server normalizes case/whitespace/dupes): %v", []string{"Web"}, outB["device"])

	// The device list now carries the tags (the group source of truth).
	req, _ := http.NewRequest("GET", r.httpAddr+"/api/devices", nil)
	req.Header.Set("Authorization", "Bearer "+opTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		die("device list: %v", err)
	}
	var list []struct {
		ID   string   `json:"id"`
		Tags []string `json:"tags"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	var group []string
	for _, d := range list {
		for _, t := range d.Tags {
			if t == "web" {
				group = append(group, d.ID)
			}
		}
	}
	if len(group) != 3 {
		die("expected the tag:web group to hold all 3 devices, got %v", group)
	}
	info("PASS: GET /api/devices — tag:web group = %v", group)

	// Invalid tag shape -> 400 (never stored, never indexable).
	code, out = patchTags(r.httpAddr, opTok, devA, []string{"BAD TAG!"})
	if code != 400 {
		die("invalid tag: expected 400, got %d (%v)", code, out)
	}
	info("PASS: invalid tag shape rejected (400): %v", out["error"])

	// Unknown device -> 404.
	code, out = patchTags(r.httpAddr, opTok, "no-such-device", []string{"web"})
	if code != 404 {
		die("unknown device: expected 404, got %d (%v)", code, out)
	}
	info("PASS: unknown device rejected (404)")

	// ---- 2. THE DoD: ONE command fans out to the whole group ---------------
	step("2. ONE bulk command to tag:web -> A + B execute (per-device tokens), C reported offline")
	code, out = bulk(r.httpAddr, opTok, map[string]any{
		"action": "run_script", "lang": "sh", "script": b64("echo grp-b2"),
		"tag":    "web",
	})
	if code != 200 {
		die("bulk run_script: status %d (%v)", code, out)
	}
	requested, _ := out["requested"].(float64)
	if requested != 3 {
		die("expected requested=3, got %v (%v)", out["requested"], out)
	}
	pushed, _ := out["pushed"].([]any)
	if len(pushed) != 2 {
		die("expected 2 pushed, got %d (%v)", len(pushed), out)
	}
	offline, _ := out["offline"].([]any)
	if len(offline) != 1 {
		die("expected 1 offline (C), got %d (%v)", len(offline), out)
	}
	info("fan-out: requested=3 pushed=%d offline=%v — each push minted its OWN per-device capability token", len(pushed), offline)

	// Collect the per-device command ids from the response, then wait for
	// both agents' terminal results and prove both executed exactly once.
	cmdIDs := map[string]string{} // device_id -> command_id
	for _, p := range pushed {
		m, _ := p.(map[string]any)
		if id, _ := m["device_id"].(string); id != "" {
			cmdIDs[id], _ = m["command_id"].(string)
		}
	}
	if _, ok := cmdIDs[devA]; !ok {
		die("pushed list missing device A: %v", pushed)
	}
	if _, ok := cmdIDs[devB]; !ok {
		die("pushed list missing device B: %v", pushed)
	}
	resA := finalResult(r.svc, cmdIDs[devA])
	resB := finalResult(r.svc, cmdIDs[devB])
	if resA == nil || resA.GetStatus() != agentv1.CommandResult_SUCCEEDED {
		die("device A: expected SUCCEEDED, got %+v", resA)
	}
	if resB == nil || resB.GetStatus() != agentv1.CommandResult_SUCCEEDED {
		die("device B: expected SUCCEEDED, got %+v", resB)
	}
	if agentA.countRan() != 1 || agentB.countRan() != 1 {
		die("each agent must execute EXACTLY once (A=%d B=%d)", agentA.countRan(), agentB.countRan())
	}
	info("PASS: single command fanned out — A and B each verified their own token and executed once (SUCCEEDED); C (offline) was reported, not faked")

	// ---- 3. empty group ------------------------------------------------------
	step("3. bulk to a tag nobody carries -> 404")
	code, out = bulk(r.httpAddr, opTok, map[string]any{
		"action": "run_script", "lang": "sh", "script": b64("echo x"), "tag": "nosuchtag",
	})
	if code != 404 {
		die("expected 404, got %d (%v)", code, out)
	}
	info("PASS: no devices carry the tag -> 404: %v", out["error"])

	// ---- 4. capability gate --------------------------------------------------
	step("4. bulk REBOOT to the group -> 403 (session lacks rmmway.reboot; nothing dispatched)")
	code, out = bulk(r.httpAddr, opTok, map[string]any{"action": "reboot", "tag": "web"})
	if code != 403 {
		die("expected 403, got %d (%v)", code, out)
	}
	if agentA.countRan() != 1 || agentB.countRan() != 1 {
		die("403 must not dispatch anything (A=%d B=%d)", agentA.countRan(), agentB.countRan())
	}
	info("PASS: session without rmmway.reboot got 403: %v — the group is untouched", out["error"])

	step("PASS: B-2 DoD — the operator filters to a tag group and ONE command fans out to every matched agent, capability-gated, with per-device tokens")
}
