// Command milestone is the W2-5 milestone: "monitored in 5 min" E2E demo.
//
// It proves the WHOLE Block 1 story on the live dev stack, end to end, with
// the REAL artifacts — not a simulated agent:
//
//  1. The current agent binary is staged as a "release" and served from a
//     local HTTP mirror (the production path is the same installer hitting a
//     GitHub release; scripts/install.sh already honours the
//     RMMWAY_GITHUB_API / RMMWAY_DOWNLOAD_BASE overrides for exactly this).
//  2. A bootstrap token is minted through the real /admin/bootstrap.
//  3. The REAL one-line installer (scripts/install.sh) runs on this machine:
//     it downloads the agent over HTTP, verifies it runs, installs it,
//     writes /etc/rmmway/agent.env and starts it as a systemd service.
//  4. The REAL agent binary self-enrolls (W1-4), persists its identity, and
//     streams live collector metrics (W1-2) — the first monitored device,
//     timed from t=0. Gated checks: online in the API, findable in
//     Meilisearch, samples visible in TimescaleDB.
//  5. ALERT PRECISION ON A TEST ESTATE: a synthetic estate (12 devices x 5
//     series x 45 days of known weekly patterns) is seeded with EXACT ground
//     truth — K series carry an injected current-hour spike, everything else
//     is clean pattern — and scored by the REAL engine. Two paths over the
//     same data: an in-process run of the engine (the same baseline.Job the
//     server runs) and the live server pass (/admin/baseline/run); the two
//     flagged sets must agree exactly. Precision/recall are then reported
//     against ground truth.
//  6. LIVE FAULT on the real device's own series (its largest disk volume):
//     a flat baseline at the measured real level is seeded, the agent is
//     stopped for a deterministic injection window, the current hour is
//     spiked -> the real engine flags it -> exactly ONE deduped inbox alert
//     appears (a second pass bumps events — no storm) -> it auto-resolves
//     when the series returns to baseline -> re-fire + manual ack/resolve
//     work through the auth-gated API (401 without a token).
//  7. Teardown: the demo agent service + install + seeded estate are removed.
//
// Usage: go run ./cmd/e2e/milestone [http-host:port] [pg-dsn] [repo-root]
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/baseline"
	"github.com/welcometotheweb/rmmway/server/internal/ca"
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

func die(f string, a ...any) {
	fmt.Printf("FAIL: "+f+"\n", a...)
	os.Exit(1)
}

func step(name string) { fmt.Printf("\n== %s ==\n", name) }
func info(f string, a ...any) {
	fmt.Printf("   "+f+"\n", a...)
}

// ---- HTTP helpers ------------------------------------------------------------

func getJSON(url string, out any) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: status %d: %s", url, resp.StatusCode, truncate(string(body), 300))
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

func postJSON(url string, payload []byte, out any) (int, error) {
	resp, err := http.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if out != nil {
		_ = json.Unmarshal(body, out)
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("%s: status %d: %s", url, resp.StatusCode, truncate(string(body), 300))
	}
	return resp.StatusCode, nil
}

func requestJSON(method, url string, payload []byte, hdrs map[string]string, out any) (int, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		_ = json.Unmarshal(b, out)
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("%s %s: status %d: %s", method, url, resp.StatusCode, truncate(string(b), 300))
	}
	return resp.StatusCode, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func pollUntil(what string, timeout time.Duration, fn func() error) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if last = fn(); last == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timeout after %v waiting for %s: %v", timeout, what, last)
}

// ---- main --------------------------------------------------------------------

func main() {
	httpAddr := "http://127.0.0.1:8080"
	pgDSN := "postgres://rmmway:***@localhost:5432/rmmway?sslmode=disable"
	repoRoot := "/opt/projects/RMMWay"
	if len(os.Args) > 1 {
		httpAddr = os.Args[1]
	}
	if len(os.Args) > 2 {
		pgDSN = os.Args[2]
	}
	if len(os.Args) > 3 {
		repoRoot = os.Args[3]
	}
	if runtime.GOOS != "linux" {
		die("milestone demo runs on linux (systemd service path)")
	}
	if os.Geteuid() != 0 {
		die("the installer path needs root (systemd unit); run as root")
	}

	t0 := time.Now()
	info("milestone E2E starting: %s (repo %s)", httpAddr, repoRoot)

	// 0. health — the stack must be up.
	var h struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	if err := getJSON(httpAddr+"/healthz", &h); err != nil {
		die("healthz: %v", err)
	}
	info("stack healthy (server %s)", h.Version)

	pgConn, err := pgx.Connect(context.Background(), pgDSN)
	if err != nil {
		die("pg connect: %v", err)
	}
	defer pgConn.Close(context.Background())
	ctx := context.Background()

	// ---------------------------------------------------------------- 1.
	step("1. stage agent release on a local mirror")
	install := filepath.Join(repoRoot, "scripts/install.sh")
	if _, err := os.Stat(install); err != nil {
		die("installer not found at %s: %v", install, err)
	}
	agentBin := filepath.Join(repoRoot, "agent/dist/rmmway-agent-linux-amd64")
	if _, err := os.Stat(agentBin); err != nil {
		die("agent binary missing (run `make agent` first): %v", err)
	}
	verOut, err := exec.Command(agentBin, "--version").CombinedOutput()
	if err != nil {
		die("agent --version: %v: %s", err, verOut)
	}
	info("staged agent: %s", strings.TrimSpace(string(verOut)))

	mirror := filepath.Join(os.TempDir(), "rmmway-mirror")
	assetPath := filepath.Join(mirror, "v1", "rmmway-agent-linux-amd64")
	if err := os.MkdirAll(filepath.Dir(assetPath), 0755); err != nil {
		die("mirror mkdir: %v", err)
	}
	binBytes, err := os.ReadFile(agentBin)
	if err != nil {
		die("read agent bin: %v", err)
	}
	if err := os.WriteFile(assetPath, binBytes, 0755); err != nil {
		die("write mirror asset: %v", err)
	}
	apiJSON, _ := json.Marshal(map[string]string{"tag_name": "v1"})
	// Local release mirror: the installer's "latest" lookup + asset download.
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/repos/welcometotheweb/rmmway/releases/latest",
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(apiJSON)
			}))
		mux.Handle("/", http.FileServer(http.Dir(mirror)))
		_ = http.ListenAndServe("127.0.0.1:18099", mux)
	}()
	time.Sleep(300 * time.Millisecond) // let the listener come up

	// ---------------------------------------------------------------- 2.
	step("2. mint bootstrap token (real /admin/bootstrap)")
	var boot struct {
		BootstrapToken string `json:"bootstrap_token"`
		DeviceID       string `json:"device_id"`
	}
	if _, err := postJSON(httpAddr+"/admin/bootstrap", []byte("{}"), &boot); err != nil {
		die("bootstrap: %v", err)
	}
	if boot.BootstrapToken == "" || boot.DeviceID == "" {
		die("bootstrap: empty token/device")
	}
	info("bootstrap token minted for device %s", boot.DeviceID)

	// ---------------------------------------------------------------- 3.
	step("3. one-line bootstrap via the REAL installer (systemd)")
	if old, _ := exec.Command("systemctl", "is-active", "rmmway-agent.service").CombinedOutput();
		strings.TrimSpace(string(old)) == "active" {
		die("rmmway-agent already active — not a clean machine (teardown a prior run first)")
	}
	env := os.Environ()
	// The installer appends /repos/${REPO}/releases/latest to RMMWAY_GITHUB_API
	// and /${VERSION}/rmmway-agent-<os>-<arch> to RMMWAY_DOWNLOAD_BASE.
	env = append(env,
		"RMMWAY_GITHUB_API=http://127.0.0.1:18099",
		"RMMWAY_DOWNLOAD_BASE=http://127.0.0.1:18099",
	)
	installCmd := exec.Command("bash", install, "--server", httpAddr, "--bootstrap", boot.BootstrapToken)
	// The dev server splits HTTP (:8080) and gRPC (:50051); the agent can't
	// know the gRPC port from the --server URL, so hand it explicitly (the
	// installer's --grpc-addr writes RMMWAY_GRPC_ADDR into the agent config).
	// The W3-1 mTLS channel (:50052) is passed the same way.
	if u, uerr := url.Parse(httpAddr); uerr == nil && u.Hostname() != "" {
		installCmd.Args = append(installCmd.Args,
			"--grpc-addr", net.JoinHostPort(u.Hostname(), "50051"),
			"--grpc-mtls-addr", net.JoinHostPort(u.Hostname(), "50052"),
		)
	}
	installCmd.Env = env
	// The "monitored in 5 min" clock starts here: at the moment the operator
	// pastes the bootstrap one-liner (installer + agent bring-up).
	tBootstrap := time.Now()
	installOut := &bytes.Buffer{}
	installCmd.Stdout = installOut
	installCmd.Stderr = installOut
	if err := installCmd.Run(); err != nil {
		die("installer failed: %v\n%s", err, installOut.String())
	}
	for _, line := range strings.Split(installOut.String(), "\n") {
		if line != "" {
			info("installer: %s", line)
		}
	}
	if err := pollUntil("rmmway-agent.service active", 30*time.Second, func() error {
		out, err := exec.Command("systemctl", "is-active", "rmmway-agent.service").CombinedOutput()
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(out)) != "active" {
			return fmt.Errorf("state %q", strings.TrimSpace(string(out)))
		}
		return nil
	}); err != nil {
		die("agent service not active: %v\njournal:\n%s", err, journal(30))
	}
	info("agent service active via systemd")

	// ---------------------------------------------------------------- 4.
	step("4. agent self-enrolled (W1-4) and is streaming live metrics")
	type devOut struct {
		ID           string    `json:"id"`
		Hostname     string    `json:"hostname"`
		AgentVersion string    `json:"agent_version"`
		Online       bool      `json:"online"`
		LastSeen     time.Time `json:"last_seen"`
	}
	var devices []devOut
	if err := pollUntil("device online in /admin/devices", 60*time.Second, func() error {
		if err := getJSON(httpAddr+"/admin/devices", &devices); err != nil {
			return err
		}
		for _, d := range devices {
			if d.ID == boot.DeviceID {
				if !d.Online {
					return fmt.Errorf("device known but not online yet")
				}
				return nil
			}
		}
		return fmt.Errorf("device %s not in list yet", boot.DeviceID)
	}); err != nil {
		die("device never went online: %v\njournal:\n%s", err, journal(40))
	}
	host, agentVerSeen := "", ""
	for _, d := range devices {
		if d.ID == boot.DeviceID {
			host, agentVerSeen = d.Hostname, d.AgentVersion
		}
	}
	tMonitored := time.Since(tBootstrap)
	info("device %s (host %q, agent %s) ONLINE after %s", boot.DeviceID, host, agentVerSeen, tMonitored.Round(100*time.Millisecond))
	info(">>> TIME-TO-FIRST-MONITORED: %s (gate: <= 5m)  [clock: bootstrap one-liner -> device online]", tMonitored.Round(time.Second))
	if tMonitored > 5*time.Minute {
		die("time-to-monitored %s exceeds the 5-minute gate", tMonitored)
	}

	if err := pollUntil("live metrics in Timescale", 90*time.Second, func() error {
		var n int
		if err := pgConn.QueryRow(ctx,
			`SELECT count(*) FROM metrics WHERE device_id=$1`, boot.DeviceID).Scan(&n); err != nil {
			return err
		}
		if n < 5 {
			return fmt.Errorf("only %d samples yet", n)
		}
		return nil
	}); err != nil {
		die("no live metrics landed: %v", err)
	}
	var nSamples, families int
	_ = pgConn.QueryRow(ctx, `SELECT count(*) FROM metrics WHERE device_id=$1`, boot.DeviceID).Scan(&nSamples)
	_ = pgConn.QueryRow(ctx, `SELECT count(DISTINCT name) FROM metrics WHERE device_id=$1`, boot.DeviceID).Scan(&families)
	info("live metrics streaming: %d samples across %d families in Timescale", nSamples, families)

	if err := pollUntil("device findable in Meilisearch", 20*time.Second, func() error {
		var res struct {
			Hits []map[string]any `json:"hits"`
		}
		if err := getJSON(httpAddr+"/admin/search?q="+host, &res); err != nil {
			return err
		}
		for _, hit := range res.Hits {
			if fmt.Sprint(hit["id"]) == boot.DeviceID {
				return nil
			}
		}
		return fmt.Errorf("device not among %d hits yet", len(res.Hits))
	}); err != nil {
		die("device not findable in Meilisearch: %v", err)
	}
	info("findable in Meilisearch by hostname %q", host)

	// ---------------------------------------------------------------- 4b.
	// W3-1 DoD, proven on the wire: the agent's own leaf (read back from the
	// identity file the REAL installer/agent wrote) gets a live stream on the
	// mTLS port; a cert NOT issued by the org root is rejected at the
	// handshake, before any RPC is processed.
	step("4b. mTLS channel DoD (real agent leaf accepted, random cert rejected)")
	if err := verifyMTLS(httpAddr, pgConn); err != nil {
		die("mTLS DoD: %v", err)
	}
	info("mTLS DoD: agent leaf verified on :50052; random (non-org) cert rejected at the handshake")

	// ---------------------------------------------------------------- 5.
	// The precision + live-fault windows are pinned to the current UTC hour;
	// don't let the run straddle an hour boundary mid-measurement.
	if rem := time.Until(time.Now().UTC().Truncate(time.Hour).Add(time.Hour)); rem < 3*time.Minute {
		info("close to a UTC hour boundary — waiting %s for a clean scoring window", rem.Round(time.Second))
		time.Sleep(rem + 10*time.Second)
	}
	step("5. alert precision on the test estate (seeded + scored, two engine paths)")
	est := estatePrecision(httpAddr, pgDSN, pgConn)
	info("estate: %d devices x %d series x 45d, %d injected faults", est.Devices, est.SeriesPerDevice, est.Faults)
	info("precision: TP=%d FP=%d FN=%d  =>  precision=%.1f%%  recall=%.1f%%",
		est.TP, est.FP, est.FN, 100*est.Precision, 100*est.Recall)
	if est.Recall < 1.0 {
		die("fault recall %.1f%% < 100%%: missed %d injected faults", 100*est.Recall, est.FN)
	}
	if est.Precision < 0.99 {
		die("precision %.1f%% below the near-zero-false-positive bar (99%%)", 100*est.Precision)
	}
	info("engine cross-check: in-process run vs live-server pass over the same data — set difference %d (must be 0)", est.CrossDiff)
	if est.CrossDiff != 0 {
		die("engine paths disagree on %d estate series", est.CrossDiff)
	}
	// The estate has done its job; purge it before the live-fault phase so the
	// server's alert inbox stays focused on the demo device.
	purgeEstate(ctx, pgConn)
	info("estate purged (precision evidence captured above)")

	// ---------------------------------------------------------------- 6.
	step("6. live fault on the real device's own series -> ONE deduped alert")
	live := liveFaultE2E(httpAddr, pgConn, boot.DeviceID)
	info("fault on real series %s[%s]: flagged at z=%.1f", live.Metric, live.Source, live.Z)
	info("-> exactly %d open alert (2nd pass bumps events to %d, no storm)", live.Open, live.Events)
	if !live.AutoResolved {
		die("live alert did not auto-resolve after the series returned to baseline")
	}
	info("-> auto-resolved when the series returned to baseline")
	info("-> re-fire + manual ack/resolve verified through the auth-gated API; 401 without a token")

	// ---------------------------------------------------------------- 7.
	step("7. teardown (demo agent + demo device removed)")
	teardown(ctx, pgConn, boot.DeviceID)
	info("teardown complete (service disabled, install removed, demo device purged)")

	// ---------------------------------------------------------------- 8.
	total := time.Since(t0)
	fmt.Println("\n================ MILESTONE E2E: PASS ================")
	fmt.Printf("  time to first monitored device      : %s  (gate: <= 5m)\n", tMonitored.Round(time.Second))
	fmt.Printf("  total demo (incl. estate + precision) : %s\n", total.Round(time.Second))
	fmt.Printf("  alert precision (test estate)       : %.1f%%  (TP=%d FP=%d FN=%d)\n", 100*est.Precision, est.TP, est.FP, est.FN)
	fmt.Printf("  alert recall (injected faults)      : %.1f%%\n", 100*est.Recall)
	fmt.Printf("  dedup: 1 open alert per faulted series; auto-resolve + manual ack/resolve OK\n")
	fmt.Printf("  mTLS (W3-1): agent's own leaf streamed live on :50052; non-org leaf rejected at handshake\n")
	fmt.Printf("  real artifacts: installer + systemd service + agent binary + live collectors\n")
	fmt.Println("=======================================================")
}

const estatePrefix = "estate-"

// ---- 5. alert precision on a seeded test estate ------------------------------

// estatePrecision seeds a synthetic estate with KNOWN ground truth and scores
// it with the REAL engine, twice over the same (static) data:
//
//   - in-process: baseline.Job over the Postgres baseline source — the exact
//     engine code the server runs;
//   - live:       POST /admin/baseline/run on the running server — the
//     production path that also drives the alert inbox.
//
// Both runs are deterministic functions of the table contents, so the two
// flagged sets must agree exactly (CrossDiff must be 0). Precision/recall
// then compare the flagged set against ground truth.
//
// Ground truth is exact by construction. Every clean series follows a weekly
// pattern (per-series level + day-of-week offset + a constant-slope
// within-day triangle) for 45 days, so each (dow, hour) slot is a tight
// (6-observation) cluster. A clean current-hour sample sits at its slot's own
// level: seasonal robust z = 0, and the triangle's constant slope keeps the
// within-day trend z under 2.7 at every hour (the engine's z>=4 flag is never
// crossed — a sine instead peaked at z≈10.2 at 21:00 UTC), so the estate is
// hour-of-day-independent. A faulted series carries a +35 spike in the
// CURRENT hour (the hour the engine scores) — far outside any slot's band
// (z is O(50–140), matching W2-3's measured z~87 on the same shape).
func estatePrecision(httpAddr, pgDSN string, pg *pgx.Conn) estateStats {
	rng := rand.New(rand.NewSource(20260823)) // fixed seed: reproducible estate
	const (
		devices    = 12
		seriesN    = 5
		days       = 45
		nFaults    = 25
		spikeDelta = 35.0
	)
	metricNames := []string{"synth.est_cpu", "synth.est_mem", "synth.est_disk", "synth.est_net", "synth.est_io"}

	now := time.Now().UTC().Truncate(time.Hour)
	// Within-day shape: a TRIANGLE (peak 06:00, trough 18:00, constant
	// slope 15/6 per hour) — deliberately NOT a sine. The sine's flat
	// bottom around 18:00 makes a small-MAD 4h window at 21–22 UTC, where
	// the recovery ramp crosses the trend channel's z>=4 band (measured
	// z≈4.19 on 2026-08-23 22:00 UTC — every clean series flagged). The
	// constant-slope triangle keeps the worst trend z at 2.70 at EVERY
	// hour, every level (30–70) and every dow offset, so the estate is
	// hour-of-day-independent: the milestone passes regardless of when it
	// runs. Seasonal behaviour is unchanged (tight 6-obs per-slot
	// clusters; clean z=0, +35 spike z O(50–140)).
	tri := func(hour float64) float64 {
		s := math.Abs(hour - 6)
		if 24-s < s {
			s = 24 - s
		}
		return 15 * (1 - s/6)
	}
	weekly := func(seed, dow, hour float64) float64 {
		return seed + dow*8 + tri(hour)
	}
	ctx := context.Background()

	// Idempotent re-seed: a prior run that failed before purgeEstate leaves
	// estate rows behind, and the seed below is a plain INSERT — leftover
	// rows would double the history and break the trend baseline (uniform
	// false positives). Wipe the estate's data first so every run scores a
	// clean 45-day window.
	for _, q := range []string{
		`DELETE FROM alerts WHERE device_id LIKE $1`,
		`DELETE FROM baseline_anomalies WHERE device_id LIKE $1`,
		`DELETE FROM metrics WHERE device_id LIKE $1`,
		`DELETE FROM devices WHERE id LIKE $1`,
	} {
		if _, err := pg.Exec(ctx, q, estatePrefix+"%"); err != nil {
			die("estate pre-seed purge: %v", err)
		}
	}

	type series struct {
		devID, metric string
		seed          float64
	}
	var seriesList []series
	for d := 0; d < devices; d++ {
		devID := fmt.Sprintf("%sd%02d", estatePrefix, d)
		if _, err := pg.Exec(ctx,
			`INSERT INTO devices (id, hostname, os, arch) VALUES ($1,$2,'linux','amd64')
			 ON CONFLICT (id) DO NOTHING`, devID, "estate-host"); err != nil {
			die("estate device insert: %v", err)
		}
		for s := 0; s < seriesN; s++ {
			seriesList = append(seriesList, series{devID, metricNames[s], 30 + rng.Float64()*40})
		}
	}

	// Seed 45 days of hourly samples per series via multi-row VALUES batches.
	const perBatch = 150
	for _, sr := range seriesList {
		var tuples []string
		var args []any
		// tuple for the row at argument index n (1-based): 4 placeholders.
		tuple := func(n int) string {
			return fmt.Sprintf("($%d,$%d,'',$%d::double precision,'{}',$%d::bigint,to_timestamp($%d::bigint/1000.0))",
				n, n+1, n+2, n+3, n+3)
		}
		flush := func() {
			if len(tuples) == 0 {
				return
			}
			stmt := "INSERT INTO metrics (device_id, name, source, value, labels, timestamp_ms, ts) VALUES " +
				strings.Join(tuples, ",")
			if _, err := pg.Exec(ctx, stmt, args...); err != nil {
				die("estate seed: %v", err)
			}
			tuples, args = nil, nil
		}
		row := 0
		for at := now.Add(-time.Duration((days-1)*24) * time.Hour); !at.After(now); at = at.Add(time.Hour) {
			v := weekly(sr.seed, float64(at.Weekday()), float64(at.Hour()))
			tuples = append(tuples, tuple(4*row+1))
			args = append(args, sr.devID, sr.metric, v, at.UnixMilli())
			row++
			if len(tuples) == perBatch {
				flush()
				row = 0
			}
		}
		flush()
	}
	info("seeded estate: %d devices, %d series, %d samples of 45d weekly pattern",
		devices, len(seriesList), days*24*len(seriesList))

	// Inject faults: pick N distinct series; replace their current-hour
	// sample with value + 35 (the engine scores the current hour's mean).
	pool := make([]int, 0, len(seriesList))
	for i := range seriesList {
		pool = append(pool, i)
	}
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	faultKeys := make([]string, 0, nFaults)
	for _, idx := range pool[:nFaults] {
		sr := seriesList[idx]
		var base float64
		if err := pg.QueryRow(ctx,
			`SELECT value FROM metrics WHERE device_id=$1 AND name=$2 AND ts >= $3 AND ts < $4
			 ORDER BY ts DESC LIMIT 1`, sr.devID, sr.metric, now, now.Add(time.Hour)).Scan(&base); err != nil {
			die("estate fault base read: %v", err)
		}
		if _, err := pg.Exec(ctx,
			`DELETE FROM metrics WHERE device_id=$1 AND name=$2 AND ts >= $3 AND ts < $4`,
			sr.devID, sr.metric, now, now.Add(time.Hour)); err != nil {
			die("estate fault delete: %v", err)
		}
		if _, err := pg.Exec(ctx,
			`INSERT INTO metrics (device_id, name, source, value, labels, timestamp_ms, ts)
			 VALUES ($1,$2,'',$3::double precision,'{}',$4::bigint,to_timestamp($4::bigint/1000.0))`,
			sr.devID, sr.metric, base+spikeDelta, now.UnixMilli()); err != nil {
			die("estate fault insert: %v", err)
		}
		faultKeys = append(faultKeys, sr.devID+"\x00"+sr.metric)
	}

	// --- path A: in-process run of the real engine -------------------------
	p2, err := pgxpool.New(ctx, pgDSN)
	if err != nil {
		die("in-process pg pool: %v", err)
	}
	job := &baseline.Job{Source: store.NewPostgresBaselineSource(p2)}
	anoms, err := job.RunOnce(ctx, time.Now())
	p2.Close()
	if err != nil {
		die("in-process baseline run: %v", err)
	}
	localFlagged := map[string]bool{}
	maxZ := 0.0
	for _, a := range anoms {
		if strings.HasPrefix(a.DeviceID, estatePrefix) {
			localFlagged[a.DeviceID+"\x00"+a.Name] = true
			if a.Score > maxZ {
				maxZ = a.Score
			}
		}
	}

	// --- path B: the live server pass (production path) ---------------------
	var runOut struct {
		Anomalies []struct {
			DeviceID string  `json:"device_id"`
			Name     string  `json:"name"`
			Score    float64 `json:"score"`
		} `json:"anomalies"`
		Series int `json:"series"`
		Runs   int `json:"runs"`
	}
	resp, err := http.Post(httpAddr+"/admin/baseline/run", "application/json", nil)
	if err != nil {
		die("estate live baseline run: %v", err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&runOut)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		die("estate live baseline run: status %d", resp.StatusCode)
	}
	liveFlagged := map[string]bool{}
	for _, a := range runOut.Anomalies {
		if strings.HasPrefix(a.DeviceID, estatePrefix) {
			liveFlagged[a.DeviceID+"\x00"+a.Name] = true
		}
	}

	// --- the two engine paths must agree exactly ---------------------------
	crossDiff := 0
	for k := range localFlagged {
		if !liveFlagged[k] {
			crossDiff++
		}
	}
	for k := range liveFlagged {
		if !localFlagged[k] {
			crossDiff++
		}
	}

	// --- precision vs ground truth (from the live pass) ---------------------
	faultSet := map[string]bool{}
	for _, k := range faultKeys {
		faultSet[k] = true
	}
	tp, fp, fn := 0, 0, 0
	for _, k := range faultKeys {
		if liveFlagged[k] {
			tp++
		} else {
			fn++
		}
	}
	for k := range liveFlagged {
		if !faultSet[k] {
			fp++
			info("  FP: %s", strings.ReplaceAll(k, "\x00", "/"))
		}
	}
	precision := 0.0
	if tp+fp > 0 {
		precision = float64(tp) / float64(tp + fp)
	}
	recall := 0.0
	if tp+fn > 0 {
		recall = float64(tp) / float64(tp + fn)
	}
	info("engine scored %d series (live pass); flagged %d estate series (TP=%d FP=%d FN=%d), max z=%.1f",
		runOut.Series, len(liveFlagged), tp, fp, fn, maxZ)
	return estateStats{
		Devices: devices, SeriesPerDevice: seriesN, Faults: nFaults,
		TP: tp, FP: fp, FN: fn, Precision: precision, Recall: recall, CrossDiff: crossDiff,
	}
}

type estateStats struct {
	Devices, SeriesPerDevice, Faults int
	TP, FP, FN                       int
	Precision, Recall                float64
	CrossDiff                        int
}

func purgeEstate(ctx context.Context, pg *pgx.Conn) {
	for _, q := range []string{
		`DELETE FROM alerts WHERE device_id LIKE $1`,
		`DELETE FROM baseline_anomalies WHERE device_id LIKE $1`,
		`DELETE FROM metrics WHERE device_id LIKE $1`,
		`DELETE FROM devices WHERE id LIKE $1`,
	} {
		if _, err := pg.Exec(ctx, q, estatePrefix+"%"); err != nil {
			die("estate purge: %v", err)
		}
	}
}

// ---- 6. live fault -> one deduped alert, auto-resolve -------------------------

// liveFaultE2E exercises the full live path on the REAL enrolled device:
//
//  1. Pick one of the agent's own series (its largest disk volume — bounded,
//     near-stable) and measure its actual current level L from the agent's
//     own samples.
//  2. Stop the agent (the step-4 monitoring proof is already complete) so the
//     current hour's mean is exactly the sample we control — no live dilution
//     of the injected spike.
//  3. Seed 44 days of flat history at L (the engine's same-slot + trend
//     baselines for this series are then exactly L).
//  4. Spike the current hour (L+35) -> live engine pass -> exactly ONE open
//     deduped alert; a second pass bumps events (no storm).
//  5. Return the hour to L -> pass -> assert auto-resolve.
//  6. Re-spike -> pass -> fresh alert; manual ack then resolve through the
//     AUTH-GATED /api/alerts (401 without a token).
//
// The agent's background baseline pass (5m cadence) may run concurrently;
// every assertion is safe under that: dedup caps the open count at 1, a
// concurrent pass only bumps events or reconciles the same state, and the
// final checks run after the series is returned to baseline (no re-fire
// source remains).
func liveFaultE2E(httpAddr string, pg *pgx.Conn, devID string) liveStats {
	ctx := context.Background()

	// 1. Pick the real device's largest disk series + its real level.
	var metric, source string
	var level float64
	err := pg.QueryRow(ctx, `
		SELECT name, source, avg(value)
		FROM metrics WHERE device_id=$1 AND name='disk.used_percent'
		GROUP BY name, source ORDER BY avg(value) DESC LIMIT 1`, devID).Scan(&metric, &source, &level)
	if err != nil || level <= 0 {
		die("no disk.used_percent series from the live agent (got %v)", err)
	}
	info("live series: %s[%s] at %.1f%% (measured from the agent's own samples)", metric, source, level)

	// 2. Stop the agent for a deterministic injection window.
	if out, err := exec.Command("systemctl", "stop", "rmmway-agent.service").CombinedOutput(); err != nil {
		die("stop agent: %v: %s", err, out)
	}
	info("agent service stopped (deterministic fault-injection window)")

	now := time.Now().UTC().Truncate(time.Hour)
	const spike = 35.0
	const ins = `INSERT INTO metrics (device_id, name, source, value, labels, timestamp_ms, ts)
		VALUES ($1,$2,$3,$4::double precision,'{}',$5::bigint,to_timestamp($5::bigint/1000.0))`
	// 3. 44 days of flat history at the real level L.
	for at := now.Add(-44 * 24 * time.Hour); !at.After(now); at = at.Add(time.Hour) {
		if _, err := pg.Exec(ctx, ins, devID, metric, source, level, at.UnixMilli()); err != nil {
			die("live history seed: %v", err)
		}
	}

	forcePass := func() {
		resp, err := http.Post(httpAddr+"/admin/baseline/run", "application/json", nil)
		if err != nil {
			die("live force pass: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			die("live force pass: status %d: %s", resp.StatusCode, truncate(string(body), 300))
		}
	}
	setHour := func(v float64) {
		if _, err := pg.Exec(ctx,
			`DELETE FROM metrics WHERE device_id=$1 AND name=$2 AND source=$3 AND ts >= $4 AND ts < $5`,
			devID, metric, source, now, now.Add(time.Hour)); err != nil {
			die("live set-hour delete: %v", err)
		}
		if _, err := pg.Exec(ctx, ins, devID, metric, source, v, now.UnixMilli()); err != nil {
			die("live set-hour insert: %v", err)
		}
	}
	openAlerts := func() []map[string]any {
		resp, err := http.Get(httpAddr + "/admin/alerts?status=open")
		if err != nil {
			die("live alert list: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var out []map[string]any
		_ = json.Unmarshal(body, &out)
		var mine []map[string]any
		for _, a := range out {
			if fmt.Sprint(a["device_id"]) == devID && fmt.Sprint(a["name"]) == metric {
				mine = append(mine, a)
			}
		}
		return mine
	}

	// 4. Spike -> exactly 1 open alert; re-pass bumps events (no storm).
	setHour(level + spike)
	forcePass()
	alerts := openAlerts()
	if len(alerts) != 1 {
		die("live fault: expected exactly 1 open alert, got %d: %v", len(alerts), alerts)
	}
	z, _ := alerts[0]["score"].(float64)
	ev1, _ := alerts[0]["events"].(float64)
	info("spike hour: 1 open alert, score(z)=%.1f, events=%v", z, ev1)
	forcePass()
	alerts = openAlerts()
	if len(alerts) != 1 {
		die("live no-storm: expected still 1 open alert, got %d", len(alerts))
	}
	ev2, _ := alerts[0]["events"].(float64)
	if ev2 <= ev1 {
		die("live no-storm: events did not bump (%v -> %v)", ev1, ev2)
	}
	info("re-pass: still 1 open alert, events %v -> %v (dedup, no storm)", ev1, ev2)

	// 5. Return to baseline -> auto-resolve.
	setHour(level)
	forcePass()
	if got := openAlerts(); len(got) != 0 {
		die("live auto-resolve: alert still open after a clean pass: %v", got)
	}
	info("clean hour: alert auto-resolved (series returned to baseline)")

	// 6. Re-spike -> fresh alert; ack + resolve via the AUTH-GATED API.
	setHour(level + spike)
	forcePass()
	alerts = openAlerts()
	if len(alerts) != 1 {
		die("live re-fire: expected 1 fresh open alert, got %d", len(alerts))
	}
	id, _ := alerts[0]["id"].(float64)
	lb, _ := json.Marshal(map[string]string{"username": "admin", "password": "admin"})
	var loginOut struct {
		Token string `json:"token"`
	}
	if _, err := postJSON(httpAddr+"/api/login", lb, &loginOut); err != nil {
		die("live login: %v", err)
	}
	auth := map[string]string{"Authorization": "Bearer " + loginOut.Token}
	if _, err := requestJSON("PATCH", fmt.Sprintf("%s/api/alerts/%d", httpAddr, int64(id)),
		[]byte(`{"status":"acked"}`), auth, nil); err != nil {
		die("live ack: %v", err)
	}
	if got := openAlerts(); len(got) != 0 {
		die("live ack: expected 0 open after ack, got %d", len(got))
	}
	if _, err := requestJSON("PATCH", fmt.Sprintf("%s/api/alerts/%d", httpAddr, int64(id)),
		[]byte(`{"status":"resolved"}`), auth, nil); err != nil {
		die("live resolve: %v", err)
	}
	// Leave the series clean, so nothing can re-fire while we verify.
	setHour(level)
	forcePass()
	if got := openAlerts(); len(got) != 0 {
		die("live resolve: expected 0 open after resolve + clean pass, got %d", len(got))
	}
	unauthed, err := http.Get(httpAddr + "/api/alerts")
	if err != nil {
		die("live auth gate: %v", err)
	}
	unauthed.Body.Close()
	if unauthed.StatusCode != http.StatusUnauthorized {
		die("live auth gate: expected 401 without token, got %d", unauthed.StatusCode)
	}
	info("manual ack -> resolve verified; /api/alerts 401 without token")

	return liveStats{Metric: metric, Source: source, Z: z, Open: 1, Events: int(ev2), AutoResolved: true}
}

type liveStats struct {
	Metric, Source string
	Z              float64
	Open           int
	Events         int
	AutoResolved   bool
}

// ---- 4b. W3-1 mTLS DoD on the wire --------------------------------------------

// identityFile is the persisted identity the real agent wrote (same path the
// installer's config + agent default resolve to).
const identityFile = "/etc/rmmway/agent-identity.json"

type agentIdentity struct {
	DeviceID string `json:"device_id"`
	JWT      string `json:"jwt"`
	TLS      *struct {
		LeafCertPEM string `json:"leaf_cert_pem"`
		LeafKeyPEM  string `json:"leaf_key_pem"`
		OrgRootPEM  string `json:"org_root_ca_pem"`
	} `json:"tls"`
}

// verifyMTLS proves the W3-1 DoD against the LIVE mTLS gRPC port:
//
//  1. read the demo agent's real persisted identity (leaf + key + org root)
//     from /etc/rmmway/agent-identity.json — the file the installer/agent
//     wrote during step 3/4, not something this harness minted;
//  2. open a Stream over TLS presenting that leaf, trusting ONLY the org
//     root (so the server's cert is verified too) — a valid leaf must get
//     a live stream with the demo JWT;
//  3. open a second connection presenting a leaf from a DIFFERENT CA (a
//     "random" cert) — the handshake must fail before any RPC runs.
func verifyMTLS(httpAddr string, pg *pgx.Conn) error {
	u, err := url.Parse(httpAddr)
	if err != nil {
		return err
	}
	host := u.Hostname()
	mtlsAddr := net.JoinHostPort(host, "50052")

	// 1. the agent's own persisted mTLS material.
	b, err := os.ReadFile(identityFile)
	if err != nil {
		return fmt.Errorf("read agent identity %s: %w", identityFile, err)
	}
	var id agentIdentity
	if err := json.Unmarshal(b, &id); err != nil {
		return fmt.Errorf("parse identity: %w", err)
	}
	if id.TLS == nil || id.TLS.LeafCertPEM == "" || id.TLS.LeafKeyPEM == "" || id.TLS.OrgRootPEM == "" {
		return fmt.Errorf("identity has no mTLS material (leaf/key/root) — server predates W3-1?")
	}
	leafKP, err := tls.X509KeyPair([]byte(id.TLS.LeafCertPEM), []byte(id.TLS.LeafKeyPEM))
	if err != nil {
		return fmt.Errorf("agent leaf keypair: %w", err)
	}
	rootPool := x509.NewCertPool()
	if !rootPool.AppendCertsFromPEM([]byte(id.TLS.OrgRootPEM)) {
		return fmt.Errorf("no cert in the persisted org root PEM")
	}
	// Cross-check against the DB: the recorded leaf in device_certs must be
	// the same one the agent is using (enroll issued it, not the harness).
	var dbLeaf []byte
	if err := pg.QueryRow(context.Background(),
		`SELECT leaf_cert_pem FROM device_certs WHERE device_id=$1`, id.DeviceID).Scan(&dbLeaf); err != nil {
		return fmt.Errorf("device_certs has no leaf for %s: %w", id.DeviceID, err)
	}
	if !bytes.Equal(dbLeaf, []byte(id.TLS.LeafCertPEM)) {
		return fmt.Errorf("agent's leaf != the leaf the server recorded for it (enroll/identity mismatch)")
	}

	// 2. valid leaf -> live stream on the mTLS port (server cert verified
	// against the pinned org root; client leaf presented).
	creds := credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{leafKP},
		RootCAs:      rootPool,
		ServerName:   host,
	})
	conn, err := grpc.NewClient(mtlsAddr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return fmt.Errorf("dial mTLS %s: %w", mtlsAddr, err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	md := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+id.JWT))
	stream, err := agentv1.NewAgentServiceClient(conn).Stream(md)
	if err != nil {
		return fmt.Errorf("stream with the agent's real leaf: %w", err)
	}
	now := time.Now().UnixMilli()
	if err := stream.Send(&agentv1.StreamRequest{
		Payload: &agentv1.StreamRequest_Heartbeat{Heartbeat: &agentv1.Heartbeat{
			TimestampMs: now, CpuPercent: 1.0, MemoryPercent: 1.0,
		}},
	}); err != nil {
		return fmt.Errorf("heartbeat over mTLS: %w", err)
	}
	ack, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("ack over mTLS: %w", err)
	}
	if ack.GetHeartbeatAck() == nil {
		return fmt.Errorf("expected a heartbeat ack over mTLS, got %T", ack.GetPayload())
	}
	info("agent's persisted leaf (device %s) streamed a live heartbeat+ack over %s", id.DeviceID, mtlsAddr)

	// 3. a cert from a DIFFERENT org root -> rejected at the TLS handshake.
	rogue, err := ca.GenerateRoot()
	if err != nil {
		return fmt.Errorf("rogue root: %w", err)
	}
	rogueLeafCert, rogueLeafKey, err := rogue.IssueLeaf("dev-rogue", "rogue-host", time.Hour)
	if err != nil {
		return fmt.Errorf("rogue leaf: %w", err)
	}
	rogueKP, err := tls.X509KeyPair(rogueLeafCert, rogueLeafKey)
	if err != nil {
		return fmt.Errorf("rogue keypair: %w", err)
	}
	rogueConn, err := grpc.NewClient(mtlsAddr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{rogueKP},
		RootCAs:      rootPool, // still trust OUR root for the server side
		ServerName:   host,
	})))
	if err != nil {
		return fmt.Errorf("dial rogue mTLS: %w", err)
	}
	defer rogueConn.Close()
	rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer rcancel()
	rmd := metadata.NewOutgoingContext(rctx, metadata.Pairs("authorization", "Bearer "+id.JWT))
	if _, err := agentv1.NewAgentServiceClient(rogueConn).Stream(rmd); err == nil {
		return fmt.Errorf("a NON-org-issued leaf was ACCEPTED on the mTLS port (the random-cert-rejected DoD failed)")
	}
	if st, ok := status.FromError(err); ok {
		info("random cert rejected as expected (grpc %s: %s)", st.Code(), st.Message()[:min(len(st.Message()), 80)])
	} else {
		info("random cert rejected as expected (%v)", err)
	}
	return nil
}

// ---- 7. teardown --------------------------------------------------------------

func teardown(ctx context.Context, pg *pgx.Conn, demoDevID string) {
	run := func(name string, args ...string) {
		_, _ = exec.Command(name, args...).CombinedOutput()
	}
	run("systemctl", "disable", "--now", "rmmway-agent.service")
	run("rm", "-f", "/usr/local/bin/rmmway-agent")
	run("rm", "-rf", "/etc/rmmway")
	run("rm", "-f", "/etc/systemd/system/rmmway-agent.service")
	run("systemctl", "daemon-reload")
	for _, q := range []string{
		`DELETE FROM alerts WHERE device_id=$1`,
		`DELETE FROM baseline_anomalies WHERE device_id=$1`,
		`DELETE FROM metrics WHERE device_id=$1`,
		`DELETE FROM device_certs WHERE device_id=$1`,
		`DELETE FROM devices WHERE id=$1`,
	} {
		if _, err := pg.Exec(ctx, q, demoDevID); err != nil {
			die("teardown demo device: %v", err)
		}
	}
	_ = os.RemoveAll(filepath.Join(os.TempDir(), "rmmway-mirror"))
}

// ---- misc --------------------------------------------------------------------

func journal(lines int) string {
	out, _ := exec.Command("journalctl", "-u", "rmmway-agent.service", "-n", strconv.Itoa(lines), "--no-pager").CombinedOutput()
	return string(out)
}
