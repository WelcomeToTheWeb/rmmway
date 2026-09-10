package setup

import (
	"context"
	"testing"
	"time"

	"github.com/welcometotheweb/rmmway/server/internal/users"
)

// fakeUsersAuth is the test UsersAuth: a map of username (lower-cased) →
// credential, plus a count that always reflects the map size.
type fakeUsersAuth struct {
	accounts map[string]UserCredential
}

func newFakeUsersAuth() *fakeUsersAuth {
	return &fakeUsersAuth{accounts: make(map[string]UserCredential)}
}

func (f *fakeUsersAuth) Count(context.Context) (int, error) { return len(f.accounts), nil }

func (f *fakeUsersAuth) Lookup(_ context.Context, username string) (UserCredential, bool, error) {
	// Case-insensitive, like the store's lower(username) index.
	for k, v := range f.accounts {
		if lowerEq(k, username) {
			return v, true, nil
		}
	}
	return UserCredential{}, false, nil
}

func lowerEq(a, b string) bool {
	la, lb := len(a), len(b)
	if la != lb {
		return false
	}
	for i := 0; i < la; i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func (f *fakeUsersAuth) add(username, role, password string, enrolled bool) {
	salt := []byte("test-salt-16byte")
	f.accounts[username] = UserCredential{
		AccountID: "usr-" + username, Username: username, Role: role, Enabled: true,
		Salt: salt, Hash: HashPassword(password, salt),
		TotpSecret: enrolledSecret(enrolled),
	}
}

func enrolledSecret(b bool) string {
	if b {
		return "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" // RFC 6238 appendix A
	}
	return ""
}

func codeAt(secret string, at time.Time) string {
	c, err := users.TotpCode(secret, at)
	if err != nil {
		panic(err)
	}
	return c
}

// memAdminService builds a Service whose admin_users holds wizardadmin/pw.
func memAdminService() *Service {
	ms := NewMemoryStore()
	salt := []byte("admin-salt-16by")
	hash := HashPassword("pw", salt)
	_ = ms.Complete(context.Background(), CompleteRequest{AdminUser: "wizardadmin", AdminPassword: "pw"}, salt, hash)
	return New(Config{Store: ms})
}

func TestAuthenticateUsersRowWins(t *testing.T) {
	s := memAdminService()
	ua := newFakeUsersAuth()
	ua.add("alice", "tech", "alice-pass", false)

	res, err := s.Authenticate(context.Background(), "Alice", "alice-pass", "", ua, envCheck("envuser", "envpass"))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	// Case-insensitive login, canonical (stored) username in the result.
	if res.Outcome != AuthOK || res.Role != "tech" || res.Username != "alice" || res.AccountID != "usr-alice" || res.Legacy {
		t.Fatalf("users-row login: got %+v", res)
	}

	// Wrong password → invalid, no fallback to any other table.
	res, _ = s.Authenticate(context.Background(), "alice", "nope", "", ua, envCheck("envuser", "envpass"))
	if res.Outcome != AuthInvalid {
		t.Fatalf("wrong password: got %+v", res)
	}
}

func TestAuthenticateMFAContract(t *testing.T) {
	s := memAdminService()
	ua := newFakeUsersAuth()
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	ua.add("bob", "viewer", "bob-pass", true)
	now := time.Now()

	// Enrolled + no code → mfa_required (the 401 challenge).
	res, err := s.Authenticate(context.Background(), "bob", "bob-pass", "", ua, envCheck("envuser", "envpass"))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if res.Outcome != AuthMFARequired || !res.TotpEnrolled {
		t.Fatalf("challenge: got %+v", res)
	}

	// Enrolled + right code → ok.
	res, _ = s.Authenticate(context.Background(), "bob", "bob-pass", codeAt(secret, now), ua, envCheck("envuser", "envpass"))
	if res.Outcome != AuthOK || res.Role != "viewer" || !res.TotpEnrolled {
		t.Fatalf("mfa login: got %+v", res)
	}

	// Enrolled + wrong code → invalid (NOT mfa_required — the challenge
	// was already answered).
	res, _ = s.Authenticate(context.Background(), "bob", "bob-pass", "000000", ua, envCheck("envuser", "envpass"))
	if res.Outcome != AuthInvalid {
		t.Fatalf("wrong code: got %+v", res)
	}
	_ = secret
}

func TestAuthenticateDisabledUser(t *testing.T) {
	s := memAdminService()
	ua := newFakeUsersAuth()
	ua.add("carol", "admin", "carol-pass", false)
	carol := ua.accounts["carol"]
	carol.Enabled = false
	ua.accounts["carol"] = carol

	res, _ := s.Authenticate(context.Background(), "carol", "carol-pass", "", ua, envCheck("envuser", "envpass"))
	if res.Outcome != AuthInvalid {
		t.Fatalf("disabled user: got %+v", res)
	}
}

func TestAuthenticateDBOnlyGating(t *testing.T) {
	env := envCheck("envuser", "envpass")
	s := memAdminService()

	// Users table EMPTY: the env pair is still the dev-mode bootstrap.
	empty := newFakeUsersAuth()
	res, err := s.Authenticate(context.Background(), "envuser", "envpass", "", empty, env)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if res.Outcome != AuthOK || !res.Legacy || res.Role != RoleAdmin {
		t.Fatalf("empty-users env bootstrap: got %+v", res)
	}
	// And the wizard admin_users row still works (step 2, no users row).
	res, _ = s.Authenticate(context.Background(), "wizardadmin", "pw", "", empty, env)
	if res.Outcome != AuthOK || !res.Legacy {
		t.Fatalf("empty-users wizard login: got %+v", res)
	}
	// Wrong username, nothing matches, but users empty → env still tried
	// (and fails) → invalid.
	res, _ = s.Authenticate(context.Background(), "nobody", "envpass", "", empty, env)
	if res.Outcome != AuthInvalid {
		t.Fatalf("unknown user: got %+v", res)
	}

	// Users table NON-EMPTY: login is DB-only — the env pair is dead even
	// for the exact env username.
	nonEmpty := newFakeUsersAuth()
	nonEmpty.add("alice", "tech", "alice-pass", false)
	res, _ = s.Authenticate(context.Background(), "envuser", "envpass", "", nonEmpty, env)
	if res.Outcome != AuthInvalid {
		t.Fatalf("DB-only: env pair must be refused once users exist, got %+v", res)
	}
	// ...but the wizard admin_users row still logs in (it is DB).
	res, _ = s.Authenticate(context.Background(), "wizardadmin", "pw", "", nonEmpty, env)
	if res.Outcome != AuthOK || !res.Legacy {
		t.Fatalf("DB-only wizard login: got %+v", res)
	}
}

func TestAuthenticateAdminUsersRowAuthoritative(t *testing.T) {
	s := memAdminService()
	empty := newFakeUsersAuth()
	env := envCheck("envuser", "envpass")

	// admin_users row exists with a DIFFERENT password than env: the row
	// wins (C #10a — a changed password can't be bypassed by env).
	salt := []byte("changed-salt-16b")
	if err := s.store.UpdatePassword(context.Background(), "wizardadmin", salt, HashPassword("changed", salt)); err != nil {
		t.Fatalf("update: %v", err)
	}
	res, _ := s.Authenticate(context.Background(), "wizardadmin", "pw", "", empty, env)
	if res.Outcome != AuthInvalid {
		t.Fatalf("stale env password must not bypass changed row, got %+v", res)
	}
	res, _ = s.Authenticate(context.Background(), "wizardadmin", "changed", "", empty, env)
	if res.Outcome != AuthOK {
		t.Fatalf("changed row password: got %+v", res)
	}
}

func TestAuthenticateInMemoryMode(t *testing.T) {
	// ua == nil (no PG users store): admin_users + env pair only.
	s := memAdminService()
	env := envCheck("envuser", "envpass")
	res, err := s.Authenticate(context.Background(), "envuser", "envpass", "", nil, env)
	if err != nil || res.Outcome != AuthOK || !res.Legacy {
		t.Fatalf("in-memory env login: got %+v, %v", res, err)
	}
	res, _ = s.Authenticate(context.Background(), "nobody", "x", "", nil, env)
	if res.Outcome != AuthInvalid {
		t.Fatalf("in-memory unknown: got %+v", res)
	}
}

func envCheck(user, pass string) EnvPairCheck {
	return func(u, p string) bool { return u == user && p == pass }
}
