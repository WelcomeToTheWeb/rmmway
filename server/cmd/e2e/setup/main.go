// Command setup is the A-2 definition-of-done harness: "a fresh database
// triggers the setup flow; subsequent boots bypass it."
//
// It runs the WHOLE real server stack in-process against a scratch
// Timescale database and drives the first-boot flow through the REAL
// operator HTTP surface:
//
//  1. FRESH: GET /api/setup/status -> setup=false (the wizard triggers);
//  2. the wizard completes in one POST: the initial root admin is minted,
//     the org root CA is re-issued under the operator's org name, and the
//     SMTP outbox config is persisted;
//  3. the SMTP outbox genuinely sends (an in-process sink captures the
//     verification mail);
//  4. login now works with the minted credentials (and the env fallback
//     still does); the password is never returned by the API;
//  5. the re-issued root is live: a leaf signed by the NEW root passes a
//     real mTLS handshake, a leaf from the BOOT root no longer does (the
//     listener's trust pool updated in place — no restart);
//  6. SUBSEQUENT BOOT: a brand-new server process over the SAME database
//     reads setup=true, restores the SAME re-issued root (no second
//     re-issue), and still authenticates the wizard's admin;
//  7. guard: a database that already has enrolled devices (a deployment that
//     predates the wizard) reports itself as set up — the UI skips the
//     wizard — and a direct completion is still refused (409): the org CA
//     can't be swapped out from under the agents that pinned it.
//
// Usage: RMMWAY_PG_DSN=... go run ./cmd/e2e/setup
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/welcometotheweb/rmmway/server/internal/ca"
	"github.com/welcometotheweb/rmmway/server/internal/caps"
	"github.com/welcometotheweb/rmmway/server/internal/httpapi"
	"github.com/welcometotheweb/rmmway/server/internal/setup"
	smtpoutbox "github.com/welcometotheweb/rmmway/server/internal/smtp"
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

// deviceCounter adapts store.DeviceStore to setup.DeviceCounter (as main.go
// does).
type deviceCounter struct{ s store.DeviceStore }

func (d deviceCounter) Count(ctx context.Context) (int, error) {
	list, err := d.s.List(ctx)
	if err != nil {
		return 0, err
	}
	return len(list), nil
}

// bootServer wires ONE in-process server (the unit a "boot" instantiates):
// org CA from the shared PG store, caps issuer, PG device store, the setup
// service, and the real httpapi on an httptest server.
func bootServer(pool *pgxpool.Pool, adminUser, adminPass string) (apiBase string, caMgr *ca.Manager, issuer *caps.Issuer, svc *setup.Service, cleanup func()) {
	caMgr, err := ca.NewManager(ca.NewPostgresOrgStore(pool), time.Hour)
	if err != nil {
		die("org CA: %v", err)
	}
	issuer = caps.NewIssuer(caMgr.Root(), 10*time.Minute)
	devices := store.NewPostgresDevices(pool)
	svc = setup.New(setup.Config{
		Store:   setup.NewPostgresStore(pool),
		OrgCA:   caMgr,
		Devices: deviceCounter{s: devices},
		OnReissued: func() {
			issuer.ReplaceRoot(caMgr.Root())
		},
	})
	apiSrv := httpapi.New(httpapi.Config{
		Devices:       devices,
		JWTSecret:     []byte("e2e-setup-secret"),
		AdminUser:     adminUser,
		AdminPassword: adminPass,
		Setup:         svc,
	})
	mux := http.NewServeMux()
	apiSrv.Register(mux)
	ts := httptest.NewServer(mux)
	return ts.URL, caMgr, issuer, svc, func() { ts.Close() }
}

// ---- HTTP helpers -----------------------------------------------------------

type setupStatus struct {
	Available       bool   `json:"available"`
	Setup           bool   `json:"setup"`
	OrgName         string `json:"org_name"`
	AdminUser       string `json:"admin_user"`
	SMTPConfigured  bool   `json:"smtp_configured"`
	DevicesEnrolled bool   `json:"devices_enrolled"`
}

func getJSON(ctx context.Context, base, path string, out any) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 {
		_ = json.Unmarshal(body, out)
	}
	return resp.StatusCode, body, nil
}

func postJSON(ctx context.Context, base, path string, in any) (int, []byte, error) {
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, strings.NewReader(string(b)))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, nil
}

func login(ctx context.Context, base, user, pass string) (int, string) {
	st, body, err := postJSON(ctx, base, "/api/login", map[string]string{"username": user, "password": pass})
	if err != nil {
		die("login request: %v", err)
	}
	var out struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(body, &out)
	return st, out.Token
}

func parseCert(pemBytes []byte) *x509.Certificate {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		die("no PEM block in cert")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		die("parse cert: %v", err)
	}
	return c
}

// mtlsHandshakeOK runs a REAL TLS client handshake against a listener built
// from the manager's TLSConfig, using the given client leaf + pinned root.
func mtlsHandshake(ctx context.Context, caMgr *ca.Manager, leafCert, leafKey, pinnedRootPEM []byte) error {
	tlsCfg, err := caMgr.TLSConfig([]string{"localhost"})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	tlsLn := tls.NewListener(ln, tlsCfg)
	defer tlsLn.Close()
	go func() {
		for {
			c, err := tlsLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 8)
				_, _ = c.Read(buf)
				_ = c.Close()
			}(c)
		}
	}()
	kp, err := tls.X509KeyPair(leafCert, leafKey)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pinnedRootPEM)
	nc, err := net.DialTimeout("tcp", ln.Addr().String(), 3*time.Second)
	if err != nil {
		return err
	}
	defer nc.Close()
	conn := tls.Client(nc, &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{kp},
		RootCAs:      pool,
		ServerName:   "localhost",
	})
	return conn.Handshake()
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	dsn := os.Getenv("RMMWAY_PG_DSN")
	if dsn == "" {
		dsn = "postgres://rmmway:rmmway@localhost:5432/rmmway?sslmode=disable"
	}

	// ---- scratch Postgres --------------------------------------------------
	step("scratch postgres + migrate (fresh database)")
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
		die("postgres not reachable: %v", err)
	}
	dbName := "rmmway_setup_e2e_" + time.Now().Format("20060102150405")
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
	if n, err := store.Migrate(ctx, pool, "migrations"); err != nil {
		die("migrate: %v (n=%d)", err, n)
	} else if n != 9 {
		die("expected 9 migrations, got %d", n)
	}
	info("9 migrations applied to scratch db %s", dbName)

	// ---- in-process SMTP sink (the outbox's mail server) -------------------
	sink, err := smtpoutbox.NewSink()
	if err != nil {
		die("smtp sink: %v", err)
	}
	defer sink.Close()
	info("in-process SMTP sink on 127.0.0.1:%d", sink.Port())

	const (
		wizUser = "rootadmin"
		wizPass = "correct-horse-battery"
		org     = "Acme Corp"
	)
	smtpCfg := smtpoutbox.Config{Host: "127.0.0.1", Port: sink.Port(), From: "rmmway@acme.test", Username: "mailer", Password: "s3cret"}

	// ---- boot 1: the FRESH server ------------------------------------------
	step("boot 1 (fresh database) — the wizard triggers")
	apiBase, caMgr, issuer, _, cleanup1 := bootServer(pool, "admin", "admin")
	defer cleanup1()
	bootRootPEM := caMgr.RootCertPEM()
	check(len(parseCert(bootRootPEM).Subject.Organization) == 1 && parseCert(bootRootPEM).Subject.Organization[0] == "RMMWay",
		"boot root should carry the default org, got %v", parseCert(bootRootPEM).Subject.Organization)

	var st setupStatus
	code, body, err := getJSON(ctx, apiBase, "/api/setup/status", &st)
	if err != nil || code != 200 {
		die("GET /api/setup/status -> %d: %s (err=%v)", code, body, err)
	}
	check(st.Available, "status.available should be true on a Postgres-backed server")
	check(!st.Setup, "FRESH database must report setup=false (the wizard's trigger), got %+v", st)
	info("status: available=true setup=false -> the UI redirects to the wizard")

	// A leaf signed by the BOOT root (what a pre-setup mTLS client would hold).
	bootLeaf, bootLeafKey, err := caMgr.Root().IssueLeaf("dev-pre", "localhost", time.Hour)
	if err != nil {
		die("issue pre-setup leaf: %v", err)
	}
	check(mtlsHandshake(ctx, caMgr, bootLeaf, bootLeafKey, bootRootPEM) == nil,
		"pre-setup leaf must pass the mTLS handshake before the wizard runs")
	info("pre-setup leaf passes the mTLS handshake (trust pool = boot root)")

	// ---- the SMTP outbox sends in the OPEN window (pre-complete) ----------
	step("SMTP outbox: test mail delivered in the open (pre-setup) window")
	code, body, err = postJSON(ctx, apiBase, "/api/setup/smtp/test", map[string]any{
		"smtp": smtpCfg,
		"to":   "ops@acme.test",
	})
	if err != nil {
		die("smtp test request: %v", err)
	}
	check(code == 200, "POST /api/setup/smtp/test (open window) -> %d: %s", code, body)
	mails := sink.Mails()
	check(len(mails) == 1, "sink should have captured exactly 1 mail, got %d", len(mails))
	m := mails[0]
	check(strings.Contains(m, "To: ops@acme.test"), "mail To wrong: %s", firstLines(m, 6))
	check(strings.Contains(m, "Subject: RMMWay: SMTP outbox test"), "mail subject wrong: %s", firstLines(m, 6))
	check(strings.Contains(m, "From: rmmway@acme.test"), "mail From wrong: %s", firstLines(m, 6))
	info("outbox delivered the verification mail to ops@acme.test (AUTH + DATA captured)")

	// ---- the wizard: one POST completes everything --------------------------
	step("wizard completes: mint root admin + re-issue org CA + persist SMTP")
	code, body, err = postJSON(ctx, apiBase, "/api/setup/complete", map[string]any{
		"admin_user":     wizUser,
		"admin_password": wizPass,
		"org_name":       org,
		"smtp":           smtpCfg,
	})
	if err != nil {
		die("complete request: %v", err)
	}
	check(code == 200, "POST /api/setup/complete -> %d: %s", code, body)
	var doneSt setupStatus
	_ = json.Unmarshal(body, &doneSt)
	check(doneSt.Setup && doneSt.OrgName == org && doneSt.AdminUser == wizUser && doneSt.SMTPConfigured,
		"complete response wrong: %+v", doneSt)
	info("setup complete: admin=%s org=%q smtp_configured=true", doneSt.AdminUser, doneSt.OrgName)

	// The org root was re-issued under the org name (persisted in org_ca).
	var rootPEM []byte
	if err := pool.QueryRow(ctx, `SELECT root_cert_pem FROM org_ca WHERE id = 1`).Scan(&rootPEM); err != nil {
		die("read org_ca: %v", err)
	}
	newRoot := parseCert(rootPEM)
	check(len(newRoot.Subject.Organization) == 1 && newRoot.Subject.Organization[0] == org,
		"org root Subject.O should be [%q], got %v", org, newRoot.Subject.Organization)
	check(newRoot.Subject.CommonName == "RMMWay Org Root CA", "root CN should stay the stable anchor name, got %q", newRoot.Subject.CommonName)
	check(string(rootPEM) != string(bootRootPEM), "the root must actually be re-issued (new key pair)")
	check(string(caMgr.RootCertPEM()) == string(rootPEM), "the running manager must hold the re-issued root")
	info("org CA re-issued: Subject=%q / O=[%q], new key pair persisted", newRoot.Subject.CommonName, org)

	// The capability-token issuer now signs with the NEW root (the OnReissued
	// hook): a minted token must verify against the re-issued root cert.
	tok, err := issuer.Mint("dev-x", caps.CapRunScript, "cmd-1")
	if err != nil {
		die("mint capability token: %v", err)
	}
	if err := caps.Verify(tok, newRoot, "dev-x", caps.CapRunScript, "cmd-1", time.Now()); err != nil {
		die("post-setup capability token must verify against the RE-ISSUED root: %v", err)
	}
	info("capability tokens now sign with the re-issued root (issuer refreshed)")

	// The mTLS listener's trust pool updated IN PLACE (no listener restart):
	// a new-root leaf now passes, the boot-root leaf no longer does.
	newLeaf, newLeafKey, _, err := caMgr.IssueDevice(ctx, "dev-post", "localhost")
	if err != nil {
		die("issue post-setup leaf: %v", err)
	}
	check(mtlsHandshake(ctx, caMgr, newLeaf, newLeafKey, rootPEM) == nil,
		"post-setup leaf must pass the mTLS handshake against the new root")
	if mtlsHandshake(ctx, caMgr, bootLeaf, bootLeafKey, bootRootPEM) == nil {
		die("the BOOT-root leaf must now be REJECTED (the trust pool moved to the new root)")
	}
	info("mTLS trust pool live-swapped: new-root leaf accepted, boot-root leaf rejected")

	// The status endpoint now reports initialized.
	code, body, err = getJSON(ctx, apiBase, "/api/setup/status", &st)
	if err != nil || code != 200 || !st.Setup {
		die("status after complete -> %d %s (want setup=true)", code, body)
	}
	info("status: setup=true (wizard bypassed from here on)")

	// The open window has CLOSED: smtp/test now requires an operator token
	// (no unauthenticated open relay through the operator's SMTP account).
	code, body, err = postJSON(ctx, apiBase, "/api/setup/smtp/test", map[string]any{"smtp": smtpCfg, "to": "ops@acme.test"})
	if err != nil {
		die("smtp test (post-setup, no token): %v", err)
	}
	check(code == 401, "post-setup smtp/test without token -> %d (want 401, the gate)", code)
	loginSt, tok := login(ctx, apiBase, wizUser, wizPass)
	check(loginSt == 200 && tok != "", "login for gated smtp test -> %d (want 200)", loginSt)
	reqBody, _ := json.Marshal(map[string]any{"smtp": smtpCfg, "to": "ops@acme.test"})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/api/setup/smtp/test", strings.NewReader(string(reqBody)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		die("smtp test (post-setup, token): %v", err)
	}
	b2, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	check(resp.StatusCode == 200, "post-setup smtp/test WITH token -> %d: %s (want 200)", resp.StatusCode, b2)
	check(len(sink.Mails()) == 2, "sink should now hold 2 mails, got %d", len(sink.Mails()))
	info("smtp/test: open pre-setup, operator-gated after (no open relay)")

	// ---- login: minted credentials work, env fallback survives -------------
	step("login: wizard-minted root admin + env fallback")
	if st2, _ := login(ctx, apiBase, wizUser, wizPass); st2 != 200 {
		die("login %s -> %d (want 200 + token)", wizUser, st2)
	}
	if st3, _ := login(ctx, apiBase, wizUser, "wrong-password"); st3 != 401 {
		die("wrong wizard password -> %d (want 401)", st3)
	}
	if st4, _ := login(ctx, apiBase, "admin", "admin"); st4 != 200 {
		die("env admin fallback (admin/admin) -> %d (want 200, dev mode kept)", st4)
	}
	info("wizard admin authenticates (401 on wrong password); env fallback admin/admin still works")

	// The API never returns the password.
	code, body, err = getJSON(ctx, apiBase, "/api/setup", nil)
	if err != nil || code != 200 {
		die("GET /api/setup -> %d (err=%v)", code, err)
	}
	check(!strings.Contains(string(body), wizPass), "GET /api/setup leaked the password: %s", body)
	check(strings.Contains(string(body), org) && strings.Contains(string(body), "127.0.0.1"),
		"GET /api/setup should report the stored org + smtp host: %s", body)
	info("GET /api/setup returns org + smtp config, never the password")

	// Repeat completion is refused (409) even for a valid-looking payload —
	// authenticated (the gate is passed, the persisted state is what 409s).
	compBody, _ := json.Marshal(map[string]any{
		"admin_user": "someone", "admin_password": "another-strong-pw", "org_name": "Elsewhere",
	})
	compReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/api/setup/complete", strings.NewReader(string(compBody)))
	compReq.Header.Set("Content-Type", "application/json")
	compReq.Header.Set("Authorization", "Bearer "+tok)
	compResp, err := http.DefaultClient.Do(compReq)
	if err != nil {
		die("second complete request: %v", err)
	}
	compB, _ := io.ReadAll(compResp.Body)
	compResp.Body.Close()
	code = compResp.StatusCode
	body = compB
	check(code == 409, "second complete (authenticated) -> %d (want 409): %s", code, body)
	info("second POST /api/setup/complete -> 409 (one-time surface)")

	// ---- boot 2: subsequent boot bypasses the wizard ------------------------
	step("boot 2 (same database) — subsequent boot bypasses the wizard")
	apiBase2, caMgr2, _, _, cleanup2 := bootServer(pool, "nobody", "nopass") // DIFFERENT env admin
	defer cleanup2()

	check(string(caMgr2.RootCertPEM()) == string(rootPEM),
		"restart must RESTORE the re-issued root (no second re-issue)")
	check(caMgr2.Root().OrgName() == org, "restored root org = %q, want %q", caMgr2.Root().OrgName(), org)

	code, body, err = getJSON(ctx, apiBase2, "/api/setup/status", &st)
	if err != nil || code != 200 {
		die("boot2 status -> %d (err=%v)", code, err)
	}
	check(st.Setup, "boot2 must report setup=true (the wizard is bypassed), got %+v", st)
	check(st.OrgName == org && st.AdminUser == wizUser && st.SMTPConfigured,
		"boot2 status lost the stored choices: %+v", st)
	info("boot2: setup=true, org=%q admin=%q restored from the database", st.OrgName, st.AdminUser)

	// The wizard's admin still authenticates on the new process — even though
	// this boot's env admin is something else entirely (DB credentials win).
	var tokB string
	if stB, tb := login(ctx, apiBase2, wizUser, wizPass); stB != 200 || tb == "" {
		die("boot2 login with wizard admin -> %d (want 200)", stB)
	} else {
		tokB = tb
	}
	if stC, _ := login(ctx, apiBase2, "nobody", "nopass"); stC != 200 {
		die("boot2 env admin (nobody/nopass) -> %d (want 200)", stC)
	}
	info("boot2: wizard admin + this boot's env admin both authenticate")

	// The wizard's complete is 409 on the new process too (persisted state;
	// the request is authenticated so the gate is passed and the 409 is the
	// persisted done-state speaking).
	b2Body, _ := json.Marshal(map[string]any{
		"admin_user": "again", "admin_password": "yet-another-pw", "org_name": org,
	})
	b2Req, _ := http.NewRequestWithContext(ctx, http.MethodPost, apiBase2+"/api/setup/complete", strings.NewReader(string(b2Body)))
	b2Req.Header.Set("Content-Type", "application/json")
	b2Req.Header.Set("Authorization", "Bearer "+tokB)
	b2Resp, err := http.DefaultClient.Do(b2Req)
	if err != nil {
		die("boot2 complete request: %v", err)
	}
	b2B, _ := io.ReadAll(b2Resp.Body)
	b2Resp.Body.Close()
	code = b2Resp.StatusCode
	body = b2B
	check(code == 409, "boot2 complete -> %d (want 409)", code)
	info("boot2: complete still 409 (state is in the database, not the process)")

	// ---- guard: devices enrolled -> the wizard refuses -----------------------
	step("guard: enrolled devices block the wizard (no CA swap under leaves)")
	dbName2 := "rmmway_setup_guard_" + time.Now().Format("20060102150405")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+dbName2); err != nil {
		die("create guard db: %v", err)
	}
	defer func() {
		ctxC, cancelC := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelC()
		_, _ = admin.Exec(ctxC, `DROP DATABASE IF EXISTS `+dbName2)
	}()
	u2 := &url.URL{Scheme: u.Scheme, User: u.User, Host: u.Host, Path: "/" + dbName2}
	pool2, err := pgxpool.New(ctx, u2.String())
	if err != nil {
		die("guard pool: %v", err)
	}
	defer pool2.Close()
	if _, err := store.Migrate(ctx, pool2, "migrations"); err != nil {
		die("guard migrate: %v", err)
	}
	// A device is enrolled on the guard DB (the wizard must now refuse).
	dev2 := store.NewPostgresDevices(pool2)
	if err := dev2.Register(ctx, "dev-enrolled", "host-1", "linux", "amd64", "0.0.0", []string{}, 30, 30); err != nil {
		die("register guard device: %v", err)
	}
	apiBase3, _, _, _, cleanup3 := bootServer(pool2, "admin", "admin")
	defer cleanup3()
	// Grandfathering: a deployment that already has enrolled devices reports
	// itself as set up — the UI skips the wizard instead of offering a
	// completion that would have to refuse (the env admin stays the login).
	var gSt setupStatus
	code, body, err = getJSON(ctx, apiBase3, "/api/setup/status", &gSt)
	if err != nil || code != 200 {
		die("guard status -> %d (err=%v)", code, err)
	}
	check(gSt.Available && gSt.Setup && gSt.DevicesEnrolled,
		"enrolled deployment must be grandfathered as set up (no wizard), got %+v", gSt)
	info("status: setup=true devices_enrolled=true -> the UI shows the login, not the wizard")
	code, body, _ = postJSON(ctx, apiBase3, "/api/setup/complete", map[string]any{
		"admin_user": "late", "admin_password": "too-late-pw-1234", "org_name": "Too Late Inc",
	})
	check(code == 409 && strings.Contains(string(body), "enrolled"),
		"complete with enrolled devices -> %d %s (want 409 devices-enrolled)", code, body)
	// The root must be UNTOUCHED (the guard runs before the re-issue).
	var guardRoot []byte
	if err := pool2.QueryRow(ctx, `SELECT root_cert_pem FROM org_ca WHERE id = 1`).Scan(&guardRoot); err != nil {
		die("guard root read: %v", err)
	}
	gc := parseCert(guardRoot)
	check(len(gc.Subject.Organization) == 1 && gc.Subject.Organization[0] == "RMMWay",
		"guard db root must keep the boot default org, got %v", gc.Subject.Organization)
	info("wizard refused (409) with enrolled devices; the org CA was not swapped")

	step("PASS")
	fmt.Println("A-2 DoD met: a fresh database triggers the first-boot setup wizard")
	fmt.Println("(root admin minted, org CA defined under the org name, SMTP outbox")
	fmt.Println("configured + verified), and every subsequent boot bypasses it.")
}

// firstLines returns the first n lines of s (diagnostics).
func firstLines(s string, n int) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	var out []string
	for sc.Scan() && len(out) < n {
		out = append(out, sc.Text())
	}
	return strings.Join(out, " | ")
}
