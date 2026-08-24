package uplink

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"

	"github.com/welcometotheweb/rmmway/agent/internal/caps"
	"github.com/welcometotheweb/rmmway/agent/internal/exec"
)

// ---- test fixtures (mirror of the server's org root + capability Mint) ----

// newTestRoot mints a throwaway ECDSA P-256 self-signed CA (the shape of the
// server's org root): (rootPEM, signingKey).
func newTestRoot(t *testing.T) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Test Org Root CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key
}

func mintCap(t *testing.T, key *ecdsa.PrivateKey, devID, capName, cmdID string, ttl time.Duration) string {
	t.Helper()
	now := time.Now()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodES256, caps.Claims{
		Cap: capName, Cmd: cmdID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: devID, Issuer: caps.TokenIssuer, ID: cmdID,
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}).SignedString(key)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return tok
}

// runCommand drives one uplink session against the shared fakeBidi,
// delivers one command frame on the downlink, and collects the agent's
// CommandResult frames until the final status (or timeout).
func runCommand(t *testing.T, commander *Commander, cmd *agentv1.Command) []*agentv1.CommandResult {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fs := &fakeBidi{recvOut: make(chan *agentv1.StreamResponse, 16), recvErr: make(chan error, 1), ctx: ctx}
	u := New(&fakeStreamer{stream: fs}, "dev-abc", "jwt", Config{
		HeartbeatInterval: 10 * time.Millisecond,
		Logger:            quiet(),
	}, WithCommander(commander))
	go func() { _ = u.Run(ctx) }()

	// Deliver the command on the downlink.
	fs.recvOut <- &agentv1.StreamResponse{
		Payload: &agentv1.StreamResponse_Command{Command: cmd},
	}

	// Collect results (the agent's uplink frames) until a terminal status.
	var out []*agentv1.CommandResult
	scan := 0
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		fs.sentMu.Lock()
		for scan < len(fs.sent) {
			if res := fs.sent[scan].GetCommandResult(); res != nil {
				out = append(out, res)
			}
			scan++
		}
		fs.sentMu.Unlock()
		for _, r := range out {
			switch r.GetStatus() {
			case agentv1.CommandResult_SUCCEEDED, agentv1.CommandResult_FAILED,
				agentv1.CommandResult_REFUSED, agentv1.CommandResult_TIMED_OUT,
				agentv1.CommandResult_UNSUPPORTED:
				return out
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no terminal CommandResult (got %d frames so far)", len(out))
	return nil
}

func resultStatuses(t *testing.T, got []*agentv1.CommandResult) []agentv1.CommandResult_Status {
	t.Helper()
	sts := make([]agentv1.CommandResult_Status, 0, len(got))
	for _, r := range got {
		sts = append(sts, r.GetStatus())
	}
	return sts
}

// ---- tests ------------------------------------------------------------------

func TestCommandValidTokenExecutes(t *testing.T) {
	rootPEM, key := newTestRoot(t)
	v, err := caps.FromRootPEM(rootPEM, "dev-abc")
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	commander := &Commander{DevID: "dev-abc", Verifier: v, Exec: exec.Default()}

	cmd := &agentv1.Command{
		Id:         "cmd-1",
		IssuedAtMs: time.Now().UnixMilli(),
		Action: &agentv1.Command_RunScript{RunScript: &agentv1.RunScript{
			Lang:            "sh",
			ScriptB64:       base64.StdEncoding.EncodeToString([]byte("echo caps-ok\n")),
			CapabilityToken: mintCap(t, key, "dev-abc", caps.CapRunScript, "cmd-1", time.Minute),
		}},
	}
	got := runCommand(t, commander, cmd)
	sts := resultStatuses(t, got)
	if len(sts) != 2 || sts[0] != agentv1.CommandResult_RECEIVED || sts[1] != agentv1.CommandResult_SUCCEEDED {
		t.Fatalf("want [RECEIVED SUCCEEDED], got %v", sts)
	}
	if !strings.Contains(got[1].GetStdoutTail(), "caps-ok") {
		t.Fatalf("stdout tail missing marker: %q", got[1].GetStdoutTail())
	}
}

func TestCommandRefusedMisboundDevice(t *testing.T) {
	// THE DoD: a fully valid channel (the command arrived over an
	// authenticated stream — the mTLS equivalent) but a capability token
	// bound to a DIFFERENT device. The agent must refuse and NOT execute.
	rootPEM, key := newTestRoot(t)
	v, err := caps.FromRootPEM(rootPEM, "dev-abc")
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	ran := false
	ex := &exec.Executor{
		RunScript: func(ctx context.Context, lang string, script []byte, args []string, timeout time.Duration) (int, []byte, []byte, error) {
			ran = true
			return 0, []byte("should not run"), nil, nil
		},
		Reboot: func(ctx context.Context) error { ran = true; return nil },
	}
	commander := &Commander{DevID: "dev-abc", Verifier: v, Exec: ex}

	cmd := &agentv1.Command{
		Id: "cmd-2",
		Action: &agentv1.Command_RunScript{RunScript: &agentv1.RunScript{
			Lang: "sh",
			// Token is otherwise valid (same root, unexpired, right cap)
			// but bound to dev-elsewhere — a cross-device replay.
			ScriptB64:       base64.StdEncoding.EncodeToString([]byte("echo x")),
			CapabilityToken: mintCap(t, key, "dev-elsewhere", caps.CapRunScript, "cmd-2", time.Minute),
		}},
	}
	got := runCommand(t, commander, cmd)
	sts := resultStatuses(t, got)
	if len(sts) != 1 || sts[0] != agentv1.CommandResult_REFUSED {
		t.Fatalf("want [REFUSED], got %v", sts)
	}
	if !strings.Contains(got[0].GetError(), "bound to device") {
		t.Fatalf("refusal reason lost: %q", got[0].GetError())
	}
	if ran {
		t.Fatal("refused command was executed")
	}
}

func TestCommandRefusedMissingToken(t *testing.T) {
	rootPEM, _ := newTestRoot(t)
	v, err := caps.FromRootPEM(rootPEM, "dev-abc")
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	ran := false
	ex := &exec.Executor{
		RunScript: func(ctx context.Context, lang string, script []byte, args []string, timeout time.Duration) (int, []byte, []byte, error) {
			ran = true
			return 0, nil, nil, nil
		},
		Reboot: func(ctx context.Context) error { ran = true; return nil },
	}
	commander := &Commander{DevID: "dev-abc", Verifier: v, Exec: ex}

	cmd := &agentv1.Command{
		Id:     "cmd-3",
		Action: &agentv1.Command_RunScript{RunScript: &agentv1.RunScript{Lang: "sh", ScriptB64: base64.StdEncoding.EncodeToString([]byte("echo x"))}},
	}
	got := runCommand(t, commander, cmd)
	sts := resultStatuses(t, got)
	if len(sts) != 1 || sts[0] != agentv1.CommandResult_REFUSED {
		t.Fatalf("want [REFUSED], got %v", sts)
	}
	if ran {
		t.Fatal("tokenless command was executed")
	}
}

func TestCommandRefusedWrongCapability(t *testing.T) {
	rootPEM, key := newTestRoot(t)
	v, err := caps.FromRootPEM(rootPEM, "dev-abc")
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	ran := false
	ex := &exec.Executor{
		RunScript: func(ctx context.Context, lang string, script []byte, args []string, timeout time.Duration) (int, []byte, []byte, error) {
			ran = true
			return 0, nil, nil, nil
		},
		Reboot: func(ctx context.Context) error { ran = true; return nil },
	}
	commander := &Commander{DevID: "dev-abc", Verifier: v, Exec: ex}

	// A reboot command carrying a run_script capability token.
	cmd := &agentv1.Command{
		Id: "cmd-4",
		Action: &agentv1.Command_Reboot{Reboot: &agentv1.Reboot{
			CapabilityToken: mintCap(t, key, "dev-abc", caps.CapRunScript, "cmd-4", time.Minute),
		}},
	}
	got := runCommand(t, commander, cmd)
	sts := resultStatuses(t, got)
	if len(sts) != 1 || sts[0] != agentv1.CommandResult_REFUSED {
		t.Fatalf("want [REFUSED], got %v", sts)
	}
	if ran {
		t.Fatal("wrong-capability command was executed")
	}
}

func TestCommandRefusedExpired(t *testing.T) {
	rootPEM, key := newTestRoot(t)
	v, err := caps.FromRootPEM(rootPEM, "dev-abc")
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	ex := &exec.Executor{
		RunScript: func(ctx context.Context, lang string, script []byte, args []string, timeout time.Duration) (int, []byte, []byte, error) {
			return 0, nil, nil, nil
		},
		Reboot: func(ctx context.Context) error { return nil },
	}
	commander := &Commander{DevID: "dev-abc", Verifier: v, Exec: ex}

	cmd := &agentv1.Command{
		Id: "cmd-5",
		Action: &agentv1.Command_RunScript{RunScript: &agentv1.RunScript{
			Lang:            "sh",
			ScriptB64:       base64.StdEncoding.EncodeToString([]byte("echo x")),
			CapabilityToken: mintCap(t, key, "dev-abc", caps.CapRunScript, "cmd-5", -2*time.Minute),
		}},
	}
	got := runCommand(t, commander, cmd)
	sts := resultStatuses(t, got)
	if len(sts) != 1 || sts[0] != agentv1.CommandResult_REFUSED {
		t.Fatalf("want [REFUSED], got %v", sts)
	}
}

func TestCommandFailedScript(t *testing.T) {
	rootPEM, key := newTestRoot(t)
	v, err := caps.FromRootPEM(rootPEM, "dev-abc")
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	commander := &Commander{DevID: "dev-abc", Verifier: v, Exec: exec.Default()}

	cmd := &agentv1.Command{
		Id: "cmd-6",
		Action: &agentv1.Command_RunScript{RunScript: &agentv1.RunScript{
			Lang:            "sh",
			ScriptB64:       base64.StdEncoding.EncodeToString([]byte("echo oops >&2; exit 7")),
			CapabilityToken: mintCap(t, key, "dev-abc", caps.CapRunScript, "cmd-6", time.Minute),
		}},
	}
	got := runCommand(t, commander, cmd)
	sts := resultStatuses(t, got)
	if len(sts) != 2 || sts[0] != agentv1.CommandResult_RECEIVED || sts[1] != agentv1.CommandResult_FAILED {
		t.Fatalf("want [RECEIVED FAILED], got %v", sts)
	}
	if got[1].GetExitCode() != 7 {
		t.Fatalf("exit code: %d, want 7", got[1].GetExitCode())
	}
	if !strings.Contains(got[1].GetStderrTail(), "oops") {
		t.Fatalf("stderr tail missing: %q", got[1].GetStderrTail())
	}
}

func TestCommandLegacyLogOnly(t *testing.T) {
	// No Commander (legacy plain channel): the command is logged, NOT
	// executed, and no CommandResult frame is sent (pre-W3-3 behavior).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fs := &fakeBidi{recvOut: make(chan *agentv1.StreamResponse, 16), recvErr: make(chan error, 1), ctx: ctx}
	u := New(&fakeStreamer{stream: fs}, "dev-abc", "jwt", Config{
		HeartbeatInterval: 10 * time.Millisecond,
		Logger:            quiet(),
	})
	go func() { _ = u.Run(ctx) }()

	fs.recvOut <- &agentv1.StreamResponse{
		Payload: &agentv1.StreamResponse_Command{Command: &agentv1.Command{
			Id: "cmd-legacy",
			Action: &agentv1.Command_RunScript{RunScript: &agentv1.RunScript{
				Lang: "sh", ScriptB64: base64.StdEncoding.EncodeToString([]byte("echo x")),
			}},
		}},
	}
	// The command is handled (log-only); no CommandResult may be reported.
	time.Sleep(200 * time.Millisecond)
	fs.sentMu.Lock()
	for _, f := range fs.sent {
		if f.GetCommandResult() != nil {
			fs.sentMu.Unlock()
			t.Fatalf("legacy mode must not report CommandResults, got %+v", f.GetCommandResult())
		}
	}
	fs.sentMu.Unlock()
}
