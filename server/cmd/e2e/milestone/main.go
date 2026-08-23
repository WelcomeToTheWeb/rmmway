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

	"github.com/welcometotheweb/rmmway/server/internal/baseline"
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
	if u, uerr := url.Parse(httpAddr); uerr == nil && u.Hostname() != "" {
		installCmd.Args = append(installCmd.Args, "--grpc-addr", net.JoinHostPort(u.Hostname(), "50051"))
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
// pattern (per-series level + day-of-week offset + hour-of-day sine) for 45
// days, so each (dow, hour) slot is a tight (6-observation) cluster. A clean
// current-hour sample sits at its slot's own level: seasonal robust z ~ 0
// and the within-day drift is a few points against the MAD-scaled band
// (max trend z observed on this shape is ~2, well under the z>=4 flag). The
// weekly dow step cannot false-fire either: the trend channel is same-day
// scoped in the engine, and the seasonal channel sees the dow step as the
// normal pattern. A faulted series carries a +35 spike in the CURRENT hour
// (the hour the engine scores) — far outside any slot's band (z is O(50),
// matching W2-3's measured z~87 on the same shape).
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
	weekly := func(seed, dow, hour float64) float64 {
		return seed + dow*8 + 15*math.Sin(2*math.Pi*hour/24)
	}
	ctx := context.Background()

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
