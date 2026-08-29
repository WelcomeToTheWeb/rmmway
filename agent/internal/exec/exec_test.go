package exec

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRunScriptShEcho(t *testing.T) {
	e := Default()
	exit, out, stderr, err := e.RunScript(context.Background(), "sh", []byte("echo caps-ok\n"), nil, time.Second*5)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if exit != 0 {
		t.Fatalf("exit: %d (stderr %s)", exit, stderr)
	}
	if !strings.Contains(string(out), "caps-ok") {
		t.Fatalf("stdout missing marker: %q", out)
	}
}

func TestRunScriptArgs(t *testing.T) {
	e := Default()
	exit, out, _, err := e.RunScript(context.Background(), "sh",
		[]byte("echo \"$1-$2\""), []string{"a", "b"}, 5*time.Second)
	if err != nil || exit != 0 {
		t.Fatalf("run: exit=%d err=%v", exit, err)
	}
	if !strings.Contains(string(out), "a-b") {
		t.Fatalf("args not passed: %q", out)
	}
}

func TestRunScriptNonZeroExit(t *testing.T) {
	e := Default()
	exit, _, stderr, err := e.RunScript(context.Background(), "sh", []byte("echo bad >&2; exit 3"), nil, 5*time.Second)
	if err != nil {
		t.Fatalf("clean non-zero exit must not be an err: %v", err)
	}
	if exit != 3 {
		t.Fatalf("exit: %d, want 3", exit)
	}
	if !strings.Contains(string(stderr), "bad") {
		t.Fatalf("stderr tail missing: %q", stderr)
	}
}

func TestRunScriptTimeout(t *testing.T) {
	e := Default()
	start := time.Now()
	_, _, _, err := e.RunScript(context.Background(), "sh", []byte("sleep 5"), nil, 300*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("timeout not reported")
	}
	if !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("want deadline error, got: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("timeout took too long: %s", elapsed)
	}
}

func TestRunScriptUnknownLang(t *testing.T) {
	e := Default()
	if _, _, _, err := e.RunScript(context.Background(), "bat", []byte("@echo hi"), nil, time.Second); err == nil {
		t.Fatal("unknown lang accepted")
	}
}

func TestRebootStub(t *testing.T) {
	e := Default()
	called := 0
	e.Reboot = func(ctx context.Context) error { called++; return nil }
	if err := e.Reboot(context.Background()); err != nil || called != 1 {
		t.Fatalf("stub reboot: called=%d err=%v", called, err)
	}
}

// TestDefaultRebootWiring is OS-gated: on a CI-less linux box the real
// reboot hook must at least RESOLVE its mechanism (we don't want to reboot
// the test host, so we only assert the resolver path is wired).
func TestDefaultRebootWiring(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	e := Default()
	if e.Reboot == nil || e.RunScript == nil {
		t.Fatal("Default() left a hook nil")
	}
}
