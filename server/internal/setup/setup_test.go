package setup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/welcometotheweb/rmmway/server/internal/smtp"
)

// fakeReissuer records the ReissueRoot calls and returns the "new" root PEM.
type fakeReissuer struct {
	orgs   []string
	rootPEM []byte
}

func (f *fakeReissuer) ReissueRoot(_ context.Context, orgName string) error {
	f.orgs = append(f.orgs, orgName)
	f.rootPEM = []byte("new-root-" + orgName)
	return nil
}
func (f *fakeReissuer) RootCertPEM() []byte { return f.rootPEM }

type fakeDevices struct{ n int }

func (d fakeDevices) Count(context.Context) (int, error) { return d.n, nil }

func memService() (*Service, *fakeReissuer, *MemoryStore) {
	re := &fakeReissuer{rootPEM: []byte("boot-root")}
	st := NewMemoryStore()
	s := New(Config{Store: st, OrgCA: re, Devices: fakeDevices{0}})
	return s, re, st
}

func req() CompleteRequest {
	return CompleteRequest{
		AdminUser:     "rootadmin",
		AdminPassword: "correct-horse-battery",
		OrgName:       "Acme Corp",
		SMTP:          smtp.Config{Host: "127.0.0.1", Port: 2525, From: "ops@acme.test"},
	}
}

func TestCompleteFlow(t *testing.T) {
	s, re, _ := memService()
	ctx := context.Background()

	st, err := s.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Available != true || st.Setup != false {
		t.Fatalf("fresh server should be available+not-setup: %+v", st)
	}

	if err := s.Complete(ctx, req()); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if len(re.orgs) != 1 || re.orgs[0] != "Acme Corp" {
		t.Fatalf("org root should be re-issued under the wizard's org, got %v", re.orgs)
	}

	st, err = s.Status(ctx)
	if err != nil {
		t.Fatalf("status after: %v", err)
	}
	if st.Setup != true || st.OrgName != "Acme Corp" || st.AdminUser != "rootadmin" || !st.SMTPConfigured {
		t.Fatalf("post-setup status wrong: %+v", st)
	}

	// Subsequent "boot" (a fresh service over the same store) sees done=true.
	s2, _, _ := memService()
	_ = s2
	stored, _ := s.store.Load(ctx)
	if !stored.Done {
		t.Fatal("store should be done")
	}

	// Repeat complete is refused.
	if err := s.Complete(ctx, req()); !errors.Is(err, ErrAlreadyComplete) {
		t.Fatalf("second complete should be ErrAlreadyComplete, got %v", err)
	}
}

func TestCompleteValidation(t *testing.T) {
	s, _, _ := memService()
	ctx := context.Background()
	cases := []struct {
		name string
		mut  func(*CompleteRequest)
	}{
		{"empty user", func(r *CompleteRequest) { r.AdminUser = "" }},
		{"short user", func(r *CompleteRequest) { r.AdminUser = "ab" }},
		{"user with space", func(r *CompleteRequest) { r.AdminUser = "a b" }},
		{"short password", func(r *CompleteRequest) { r.AdminPassword = "short" }},
		{"empty org", func(r *CompleteRequest) { r.OrgName = "  " }},
		{"smtp bad from", func(r *CompleteRequest) { r.SMTP = smtp.Config{Host: "h", From: "nope"} }},
	}
	for _, c := range cases {
		r := req()
		c.mut(&r)
		if err := s.Complete(ctx, r); err == nil {
			t.Errorf("%s: expected validation error", c.name)
		}
	}
	// nothing persisted on validation failure
	st, _ := s.store.Load(ctx)
	if st.Done {
		t.Fatal("failed completes must not mark setup done")
	}
}

func TestCompleteDevicesGuard(t *testing.T) {
	re := &fakeReissuer{rootPEM: []byte("boot-root")}
	st := NewMemoryStore()
	s := New(Config{Store: st, OrgCA: re, Devices: fakeDevices{n: 3}})
	err := s.Complete(context.Background(), req())
	if !errors.Is(err, ErrDevicesEnrolled) {
		t.Fatalf("expected ErrDevicesEnrolled, got %v", err)
	}
	if len(re.orgs) != 0 {
		t.Fatal("re-issue must not happen when devices are enrolled")
	}
}

// A deployment that predates the wizard (devices already enrolled, no setup
// row) must report itself as set up — the UI skips the wizard and the
// env-admin login stays the way in. Completing is still refused because the
// root CA cannot be swapped out from under the pinned agents.
func TestStatusGrandfathersEnrolledDeployments(t *testing.T) {
	re := &fakeReissuer{rootPEM: []byte("boot-root")}
	st := NewMemoryStore()
	s := New(Config{Store: st, OrgCA: re, Devices: fakeDevices{n: 5}})
	stt, err := s.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !stt.Available || !stt.Setup || !stt.DevicesEnrolled {
		t.Fatalf("enrolled deployment should be available+setup+devices_enrolled: %+v", stt)
	}
	if err := s.Complete(context.Background(), req()); !errors.Is(err, ErrDevicesEnrolled) {
		t.Fatalf("complete on an enrolled deployment must be refused, got %v", err)
	}
	if len(re.orgs) != 0 {
		t.Fatal("root must not be swapped under enrolled devices")
	}
}

func TestOnReissuedHook(t *testing.T) {
	calls := 0
	re := &fakeReissuer{rootPEM: []byte("boot-root")}
	st := NewMemoryStore()
	s := New(Config{Store: st, OrgCA: re, Devices: fakeDevices{0}, OnReissued: func() { calls++ }})
	if err := s.Complete(context.Background(), req()); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if calls != 1 {
		t.Fatalf("OnReissued should fire exactly once, got %d", calls)
	}
}

func TestPasswordHash(t *testing.T) {
	salt := []byte("0123456789abcdef")
	h := HashPassword("hunter2!", salt)
	if len(h) != 32 {
		t.Fatalf("hash length %d, want 32", len(h))
	}
	if !VerifyPassword("hunter2!", salt, h) {
		t.Fatal("correct password should verify")
	}
	if VerifyPassword("hunter3!", salt, h) {
		t.Fatal("wrong password should not verify")
	}
	if VerifyPassword("hunter2!", []byte("other-salt-16byt"), h) {
		t.Fatal("different salt should not verify")
	}
}

func TestVerifyAdmin(t *testing.T) {
	st := NewMemoryStore()
	salt := []byte("0123456789abcdef")
	if err := st.Complete(context.Background(), req(), salt, HashPassword(req().AdminPassword, salt)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	ctx := context.Background()
	if !VerifyAdmin(ctx, st, "rootadmin", "correct-horse-battery") {
		t.Fatal("admin should verify")
	}
	if VerifyAdmin(ctx, st, "rootadmin", "wrong") {
		t.Fatal("wrong password should fail")
	}
	if VerifyAdmin(ctx, st, "nobody", "correct-horse-battery") {
		t.Fatal("unknown user should fail")
	}
}

func TestTestSMTPNotConfigured(t *testing.T) {
	s, _, _ := memService()
	if err := s.TestSMTP(context.Background(), smtp.Config{}, ""); err == nil {
		t.Fatal("test send with empty config should fail")
	}
	// A configured config with a failing sender returns the send error.
	s2 := New(Config{Store: NewMemoryStore(), OrgCA: &fakeReissuer{}, SendSMTP: func(context.Context, smtp.Config, string, string, string) error {
		return errors.New("dial refused")
	}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := s2.TestSMTP(ctx, smtp.Config{Host: "127.0.0.1", Port: 2525, From: "a@b.c"}, "")
	if err == nil || err.Error() != "dial refused" {
		t.Fatalf("want the injected send error, got %v", err)
	}
}
