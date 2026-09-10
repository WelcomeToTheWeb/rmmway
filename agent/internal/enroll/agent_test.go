package enroll

import (
	"context"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// fakeEnroller is a scripted Enroller that counts calls so tests can assert
// "no re-enroll on restart".
type fakeEnroller struct {
	calls   int
	devID   string
	jwt     string
	err     error
	gotHost string
}

func (f *fakeEnroller) Enroll(_ context.Context, in *agentv1.EnrollRequest, _ ...grpc.CallOption) (*agentv1.EnrollResponse, error) {
	f.calls++
	f.gotHost = in.GetHostname()
	if f.err != nil {
		return nil, f.err
	}
	return &agentv1.EnrollResponse{DeviceId: f.devID, Jwt: f.jwt, HeartbeatIntervalS: 30, MetricIntervalS: 30}, nil
}

var _ Enroller = (*fakeEnroller)(nil)

// Compile-time proof the real generated client interface also satisfies Enroller.
var _ Enroller = (agentv1.AgentServiceClient)(nil)

func tmpStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(filepath.Join(t.TempDir(), "agent.identity"))
}

func TestFirstBootEnrollsAndPersists(t *testing.T) {
	fake := &fakeEnroller{devID: "dev-abc123", jwt: "tok-xyz"}
	a := New(fake, tmpStore(t), Facts{Hostname: "fileserver-01", OS: "linux", Arch: "amd64", AgentVersion: "1.2.3"}, "bt-123")

	res, err := a.EnsureEnrolled(context.Background())
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if !res.Enrolled {
		t.Fatal("expected Enrolled=true on first boot")
	}
	if res.Identity.DeviceID != "dev-abc123" || res.Identity.JWT != "tok-xyz" {
		t.Fatalf("bad identity: %+v", res.Identity)
	}
	if fake.calls != 1 {
		t.Fatalf("expected 1 enroll call, got %d", fake.calls)
	}
	if fake.gotHost != "fileserver-01" {
		t.Fatalf("host not reported: %q", fake.gotHost)
	}
	// identity persisted to disk
	if id, err := a.store.Load(); err != nil || id == nil || id.DeviceID != "dev-abc123" {
		t.Fatalf("identity not persisted: %v / %+v", err, id)
	}
}

func TestRestartDoesNotReEnroll(t *testing.T) {
	store := tmpStore(t)
	// Simulate a prior boot that already enrolled + persisted.
	if err := store.Save(&Identity{DeviceID: "dev-persisted", JWT: "tok-old"}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeEnroller{devID: "dev-should-not-appear", jwt: "tok-new"}
	a := New(fake, store, Facts{Hostname: "x", OS: "linux", Arch: "amd64"}, "bt-unused")

	res, err := a.EnsureEnrolled(context.Background())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if res.Enrolled {
		t.Fatal("must NOT report a fresh enroll on restart")
	}
	if fake.calls != 0 {
		t.Fatalf("enroll was called %d times on restart (want 0)", fake.calls)
	}
	if res.Identity.DeviceID != "dev-persisted" {
		t.Fatalf("restarted agent lost its identity: %q", res.Identity.DeviceID)
	}
}

func TestNoIdentityAndNoTokenErrors(t *testing.T) {
	fake := &fakeEnroller{}
	a := New(fake, tmpStore(t), Facts{Hostname: "x"}, "")
	if _, err := a.EnsureEnrolled(context.Background()); err == nil {
		t.Fatal("expected an error when there is no identity and no bootstrap token")
	}
	if fake.calls != 0 {
		t.Fatal("must not call enroll without a token")
	}
}

func TestEnrollFailureDoesNotPersist(t *testing.T) {
	fake := &fakeEnroller{err: status.Error(codes.PermissionDenied, "unknown or already-used bootstrap token")}
	store := tmpStore(t)
	a := New(fake, store, Facts{Hostname: "x"}, "bt-bad")

	if _, err := a.EnsureEnrolled(context.Background()); err == nil {
		t.Fatal("expected enroll error to surface")
	}
	// Nothing may have been persisted on a failed enroll.
	if id, _ := store.Load(); id != nil {
		t.Fatalf("failed enroll must not persist an identity, got %+v", id)
	}
}

func TestBearerMetadata(t *testing.T) {
	res := &Result{Identity: &Identity{DeviceID: "dev-1", JWT: "tok"}}
	dev, tok := res.BearerMetadata()
	if dev != "dev-1" || tok != "tok" {
		t.Fatalf("bad bearer metadata: %q %q", dev, tok)
	}
	if _, _ = (&Result{}).BearerMetadata(); true {
		// empty result -> empty strings, no panic
	}
}
