// Command export is the W4-3 definition-of-done harness: "export a client →
// a self-describing bundle (Parquet + JSON) that re-imports or opens in a
// standard tool."
//
// It boots the REAL operator HTTP surface (httpapi: login, auth gate, the
// /devices/{id}/export route) against a scratch Timescale database and
// tears it all down:
//
//  1. fixture: one enrolled device, 2 days of 30s samples over 3 series
//     (17280 rows), the 1-minute continuous aggregate materialized, and 3
//     alerts (open / acked / resolved);
//  2. one-click export: operator login + ONE GET /api/devices/{id}/export
//     streams the bundle (no auth -> 401, unknown device -> 404, /admin
//     mirror open);
//  3. self-describing: export.Verify checks the bundle end-to-end against
//     ITS OWN manifest (every sha256 + size + row count, no stray files);
//  4. tamper detection: a flipped byte inside metrics.parquet fails
//     Verify (the skeptic story);
//  5. windowing: ?since=&until= bounds the raw section exactly;
//  6. opens in a standard tool: metrics.parquet is re-read with an
//     independent standard Parquet reader (schema + values + ts
//     round-trip);
//  7. re-imports: the exported Parquet rows load into a FRESH scratch
//     database (migrated, device registered) and come back identical
//     (count + spot values + range).
//
// Usage: RMMWAY_PG_DSN=... go run ./cmd/e2e/export
// (needs a Timescale-capable Postgres where the user can CREATE DATABASE;
// locally: RMMWAY_PG_DSN=postgres://postgres@localhost:5432/postgres)
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/welcometotheweb/rmmway/server/internal/export"
	"github.com/welcometotheweb/rmmway/server/internal/httpapi"
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

// fixture geometry (all derived from base).
const (
	sampleInterval = 30 * time.Second
	days           = 2
)

var base time.Time // set in main

func samplesPerSeries() int { return int(time.Duration(days*24*time.Hour) / sampleInterval) }

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	dsn := os.Getenv("RMMWAY_PG_DSN")
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres"
	}
	base = time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	perSeries := samplesPerSeries()
	total := perSeries * 3

	admin, pool, dbName, cleanup := scratchDB(ctx, dsn)
	defer cleanup()
	info("scratch db: %s", dbName)

	// ---- fixture -----------------------------------------------------------
	step("fixture: device + 2 days of samples + alerts")
	devices := store.NewPostgresDevices(pool)
	const devID = "dev_export_e2e"
	if err := devices.Register(ctx, devID, "fileserver-x", "linux", "amd64", "0.5.0",
		[]string{"10.0.0.5"}, 30, 30); err != nil {
		die("register: %v", err)
	}
	insertFixture(ctx, pool, devID, perSeries)
	if _, err := pool.Exec(ctx, `CALL refresh_continuous_aggregate('metrics_1m', NULL, NULL)`); err != nil {
		die("refresh CA: %v", err)
	}
	insertAlerts(ctx, pool, devID)
	info("device %s (fileserver-x): %d samples (3 series × %d), 1m rollups materialized, 3 alerts", devID, total, perSeries)

	// ---- real operator HTTP surface ----------------------------------------
	step("boot in-process server (real httpapi: login + export route)")
	apiSrv := httpapi.New(httpapi.Config{
		Devices:       devices,
		JWTSecret:     []byte("e2e-export-secret"),
		AdminUser:     "admin",
		AdminPassword: "e2e-pass",
		Export: export.New(export.Config{
			Devices: devices,
			Metrics: export.NewPostgresMetrics(pool),
			Rollups: export.NewPostgresRollups(pool),
			Alerts:  export.NewPostgresAlerts(pool),
			Version: "rmmway-server/e2e",
		}),
	})
	srvURL := serveAPI(apiSrv)
	defer srvURL.Close()
	info("server on %s (GET /api/devices/{id}/export, /admin mirror, POST /api/login)", srvURL.URL)

	// ---- auth + routing gates ----------------------------------------------
	step("gates: no auth -> 401, unknown device -> 404")
	resp, err := http.Get(srvURL.URL + "/api/devices/" + devID + "/export")
	check(err == nil, "unauthed request: %v", err)
	check(resp.StatusCode == http.StatusUnauthorized, "unauthed = %d, want 401", resp.StatusCode)
	resp.Body.Close()

	resp, err = http.Get(srvURL.URL + "/admin/devices/nope/export")
	check(err == nil, "unknown device: %v", err)
	check(resp.StatusCode == http.StatusNotFound, "unknown device = %d, want 404", resp.StatusCode)
	resp.Body.Close()

	// ---- the one-click export ------------------------------------------------
	step("one-click export: login + ONE GET")
	token := login(ctx, srvURL.URL)
	resp, err = httpGetBearer(srvURL.URL+"/api/devices/"+devID+"/export", token)
	check(err == nil, "export request: %v", err)
	check(resp.StatusCode == http.StatusOK, "export = %d, want 200", resp.StatusCode)
	check(resp.Header.Get("Content-Type") == "application/zip",
		"content-type = %q, want application/zip", resp.Header.Get("Content-Type"))
	check(strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment; filename=\"rmmway-export-"+devID),
		"content-disposition = %q", resp.Header.Get("Content-Disposition"))
	bundle, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	check(err == nil, "read bundle: %v", err)
	info("bundle downloaded: %d bytes", len(bundle))

	// /admin mirror (open for e2e/ops) serves the same shape.
	resp, err = http.Get(srvURL.URL + "/admin/devices/" + devID + "/export?rollups=0")
	check(err == nil && resp.StatusCode == http.StatusOK, "admin mirror: %v status=%d", err, resp.StatusCode)
	if b, _ := io.ReadAll(resp.Body); b != nil {
		_, verr := export.Verify(bytes.NewReader(b), int64(len(b)))
		check(verr == nil, "admin mirror (rollups=0) bundle: %v", verr)
	}
	resp.Body.Close()

	// ---- self-describing verification -----------------------------------------
	step("self-describing: Verify against the bundle's own manifest")
	mf, err := export.Verify(bytes.NewReader(bundle), int64(len(bundle)))
	check(err == nil, "Verify: %v", err)
	check(mf.Device.ID == devID && mf.Device.Hostname == "fileserver-x",
		"manifest device = %+v", mf.Device)
	check(mf.Format == export.FormatName, "format = %s", mf.Format)

	names := map[string]bool{}
	rows := map[string]int64{}
	for _, f := range mf.Files {
		names[f.Name] = true
		rows[f.Name] = f.Rows
	}
	wantFiles := []string{export.ManifestName, export.DeviceName, export.MetricsName,
		export.RollupsName, export.AlertsName, export.ReadmeName}
	for _, n := range wantFiles {
		check(names[n], "bundle missing %s (has %v)", n, names)
	}
	check(len(mf.Files) == len(wantFiles), "bundle has %d files, want exactly %d", len(mf.Files), len(wantFiles))
	check(rows[export.MetricsName] == int64(total),
		"metrics rows = %d, want %d", rows[export.MetricsName], total)
	check(rows[export.RollupsName] > 0, "rollup rows = %d, want > 0", rows[export.RollupsName])
	check(rows[export.AlertsName] == 3, "alert rows = %d, want 3", rows[export.AlertsName])
	info("manifest verified: %d files, all sha256/size/rows check out (metrics=%d rollups=%d alerts=3)",
		len(mf.Files), rows[export.MetricsName], rows[export.RollupsName])

	// ---- tamper detection ------------------------------------------------------
	step("tamper: flip a byte in metrics.parquet -> Verify fails")
	tampered := tamperZipEntry(ctx, bundle, export.MetricsName)
	if _, err := export.Verify(bytes.NewReader(tampered), int64(len(tampered))); err == nil {
		die("Verify accepted a tampered bundle")
	} else {
		info("tampered bundle rejected: %v", err)
	}

	// ---- windowing --------------------------------------------------------------
	step("window: ?since=&until= bounds the raw section exactly")
	since := base.Add(24 * time.Hour)
	until := base.Add(48 * time.Hour)
	resp, err = httpGetBearer(srvURL.URL+"/api/devices/"+devID+"/export?since="+
		since.UTC().Format(time.RFC3339)+"&until="+until.UTC().Format(time.RFC3339), token)
	check(err == nil && resp.StatusCode == http.StatusOK, "windowed export: %v status=%d", err, resp.StatusCode)
	wb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	wmf, err := export.Verify(bytes.NewReader(wb), int64(len(wb)))
	check(err == nil, "windowed Verify: %v", err)
	var wrows int64
	for _, f := range wmf.Files {
		if f.Name == export.MetricsName {
			wrows = f.Rows
		}
	}
	check(wrows == int64(perSeries/2*3), "windowed rows = %d, want %d (day 2, all 3 series)",
		wrows, perSeries/2*3)
	check(wmf.Range != nil && wmf.Range.Since.Equal(since) && wmf.Range.Until.Equal(until),
		"manifest range = %+v, want [%s, %s)", wmf.Range, since, until)
	info("window [%s, %s) -> %d rows (exact)", since.Format("01/02 15:04"), until.Format("01/02 15:04"), wrows)

	// ---- opens in a standard tool -------------------------------------------------
	step("standard tool: re-read metrics.parquet with an independent Parquet reader")
	mrows, err := export.ReadMetrics(bytes.NewReader(bundle), int64(len(bundle)))
	check(err == nil, "ReadMetrics: %v", err)
	check(len(mrows) == total, "re-read rows = %d, want %d", len(mrows), total)
	// ts (Timestamp[ns]) and the raw agent wall clock must agree row-for-row.
	mismatches := 0
	seenSeries := map[string]bool{}
	var spotCPU *export.MetricRow
	for i := range mrows {
		r := &mrows[i]
		if r.TS.UnixMilli() != r.TimestampMs {
			mismatches++
		}
		seenSeries[r.Name+"@"+r.Source] = true
		if r.Name == "cpu.utilization_percent" && r.Source == "" && r.TimestampMs == base.Add(sampleInterval*100).UnixMilli() {
			spotCPU = r
		}
	}
	check(mismatches == 0, "ts/timestamp_ms mismatches = %d, want 0", mismatches)
	check(len(seenSeries) == 3, "series re-read = %v, want 3", seenSeries)
	check(spotCPU != nil, "spot sample missing")
	check(spotCPU.Value == cpuValue(100), "spot cpu value = %v, want %v (value round-trip)",
		spotCPU.Value, cpuValue(100))
	check(spotCPU.Labels == `{"host":"fileserver-x"}`, "spot labels = %q (labels JSON round-trip)", spotCPU.Labels)
	info("re-read %d rows across %d series; ts == timestamp_ms on every row; value + labels round-trip",
		len(mrows), len(seenSeries))

	// ---- re-import ------------------------------------------------------------------
	step("re-import: load the exported Parquet into a FRESH database")
	reimpDB, reimpName, reimpCleanup := scratchDB2(ctx, dsn)
	defer reimpCleanup()
	red := store.NewPostgresDevices(reimpDB)
	if err := red.Register(ctx, devID, "fileserver-x", "linux", "amd64", "0.5.0",
		[]string{"10.0.0.5"}, 30, 30); err != nil {
		die("re-import register: %v", err)
	}
	tx, err := reimpDB.Begin(ctx)
	check(err == nil, "re-import begin: %v", err)
	for _, r := range mrows {
		if _, err := tx.Exec(ctx, `
			INSERT INTO metrics (device_id, name, source, value, labels, timestamp_ms, ts)
			VALUES ($1,$2,$3,$4,$5::jsonb,$6,$7)
			ON CONFLICT DO NOTHING`,
			devID, r.Name, r.Source, r.Value, r.Labels, r.TimestampMs, r.TS); err != nil {
			die("re-import insert: %v", err)
		}
	}
	check(tx.Commit(ctx) == nil, "re-import commit")
	var reimpCount int
	check(reimpDB.QueryRow(ctx, `SELECT count(*) FROM metrics WHERE device_id=$1`, devID).
		Scan(&reimpCount) == nil, "re-import count query")
	check(reimpCount == total, "re-imported rows = %d, want %d", reimpCount, total)
	var minTS, maxTS time.Time
	check(reimpDB.QueryRow(ctx, `SELECT min(ts), max(ts) FROM metrics`).Scan(&minTS, &maxTS) == nil, "re-import range")
	check(minTS.Equal(base) && maxTS.Equal(base.Add(time.Duration(perSeries-1)*sampleInterval)),
		"re-import range = [%s, %s]", minTS, maxTS)
	info("re-imported %d rows into fresh db %s: count + time range identical — the data is portable",
		reimpCount, reimpName)

	_ = admin
	step("PASS")
	fmt.Println("W4-3 DoD met: one-click export -> self-describing bundle (Parquet + JSON);")
	fmt.Println("Verify (own manifest) + tamper detection + windowing + standard-tool re-read +")
	fmt.Println("re-import into a fresh database all pass.")
}

// ---- fixture -----------------------------------------------------------------

func cpuValue(i int) float64 {
	// Deterministic: ~20-30% with a periodic 10% wave.
	return 25 + 10*float64(i%100)/100*10
}

func diskValue(i int) float64 { return 62 + float64(i%200)/10 }

func netValue(i int) float64 { return 1000 + 500*float64(i%60)/60 }

func insertFixture(ctx context.Context, pool *pgxpool.Pool, devID string, perSeries int) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		die("fixture begin: %v", err)
	}
	for i := 0; i < perSeries; i++ {
		ts := base.Add(time.Duration(i) * sampleInterval)
		ms := ts.UnixMilli()
		stmts := []struct {
			name, source string
			value        float64
			labels       string
		}{
			{"cpu.utilization_percent", "", cpuValue(i), `{"host":"fileserver-x"}`},
			{"disk.used_percent", "/dev/sda1", diskValue(i), `{}`},
			{"net.rate_bytes_per_sec", "eth0", netValue(i), `{"iface":"eth0"}`},
		}
		for _, s := range stmts {
			if _, err := tx.Exec(ctx, `
				INSERT INTO metrics (device_id, name, source, value, labels, timestamp_ms, ts)
				VALUES ($1,$2,$3,$4,$5::jsonb,$6,$7)
				ON CONFLICT DO NOTHING`,
				devID, s.name, s.source, s.value, s.labels, ms, ts); err != nil {
				die("fixture insert %s: %v", s.name, err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		die("fixture commit: %v", err)
	}
}

func insertAlerts(ctx context.Context, pool *pgxpool.Pool, devID string) {
	if _, err := pool.Exec(ctx, `
		INSERT INTO alerts (device_id, name, source, status, score, channel, value, expected, events, first_at, last_at, resolved_at, acked_at)
		VALUES
		 ($1,'cpu.utilization_percent','', 'open',     12.5, 'seasonal', 91, 25, 4, $2, $2,        NULL, NULL),
		 ($1,'disk.used_percent','/dev/sda1','acked',    7.0, 'trend',    88, 62, 2, $2, $2,        NULL, $2),
		 ($1,'net.rate_bytes_per_sec','eth0','resolved', 9.5, 'seasonal', 4200, 1000, 3, $2, $2, $2, $2)`,
		devID, base); err != nil {
		die("insert alerts: %v", err)
	}
}

// ---- scratch DB helpers ----------------------------------------------------------

// scratchDB creates a fresh migrated Timescale DB (needs CREATE DATABASE).
func scratchDB(ctx context.Context, dsn string) (admin *pgxpool.Pool, pool *pgxpool.Pool, name string, cleanup func()) {
	admin, pool, name = makeScratch(ctx, dsn, "rmmway_export_e2e_")
	cleanup = func() { dropScratch(admin, name) }
	return
}

func scratchDB2(ctx context.Context, dsn string) (*pgxpool.Pool, string, func()) {
	admin, pool, name := makeScratch(ctx, dsn, "rmmway_export_reimport_")
	cleanup := func() { dropScratch(admin, name) }
	return pool, name, cleanup
}

func makeScratch(ctx context.Context, dsn, prefix string) (*pgxpool.Pool, *pgxpool.Pool, string) {
	u, err := url.Parse(dsn)
	if err != nil {
		die("parse dsn: %v", err)
	}
	admin, err := pgxpool.New(ctx, u.String())
	if err != nil {
		die("admin pool: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		die("postgres not reachable: %v (try RMMWAY_PG_DSN=postgres://postgres@localhost:5432/postgres)", err)
	}
	name := prefix + time.Now().Format("20060102150405")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		die("create scratch db: %v (does the user have CREATEDB?)", err)
	}
	step("migrate scratch db " + name)
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		die("scratch pool: %v", err)
	}
	migN, migErr := store.Migrate(ctx, pool, "migrations")
	if migErr != nil {
		die("migrate: %v (n=%d)", migErr, migN)
	} else if migN < 5 {
		die("expected >=5 migrations (W1-6..W5-1), got %d", migN)
	}
	info("%d migrations applied to scratch db %s", migN, name)
	return admin, pool, name
}

func dropScratch(admin *pgxpool.Pool, name string) {
	if admin == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+name)
	admin.Close()
}

// ---- http + zip helpers -----------------------------------------------------------

type apiServer struct {
	URL  string
	stop func()
}

func (s *apiServer) Close() { s.stop() }

func serveAPI(srv *httpapi.Server) *apiServer {
	mux := http.NewServeMux()
	srv.Register(mux)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		die("listen: %v", err)
	}
	hs := &http.Server{Handler: mux}
	go func() { _ = hs.Serve(lis) }()
	return &apiServer{
		URL:  "http://" + lis.Addr().String(),
		stop: func() { _ = hs.Close() },
	}
}

func login(ctx context.Context, base string) string {
	payload := `{"username":"admin","password":"e2e-pass"}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/login", strings.NewReader(payload))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		die("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		die("login = %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		die("login decode: %v", err)
	}
	if out.Token == "" {
		die("login returned no token")
	}
	return out.Token
}

func httpGetBearer(rawURL, token string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return http.DefaultClient.Do(req)
}

// tamperZipEntry returns the bundle with one byte flipped inside `name`
// (rebuilding the zip so only that entry's payload changes).
func tamperZipEntry(ctx context.Context, bundle []byte, name string) []byte {
	zr, err := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		die("tamper: open zip: %v", err)
	}
	out := &bytes.Buffer{}
	zw := zip.NewWriter(out)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			die("tamper: open %s: %v", f.Name, err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		if f.Name == name && len(b) > 8 {
			b[len(b)/2] ^= 0xff
		}
		w, err := zw.Create(f.Name)
		if err != nil {
			die("tamper: create %s: %v", f.Name, err)
		}
		if _, err := w.Write(b); err != nil {
			die("tamper: write %s: %v", f.Name, err)
		}
	}
	if err := zw.Close(); err != nil {
		die("tamper: close zip: %v", err)
	}
	return out.Bytes()
}
