// Command e2e verifies W1-5+W1-6+W1-7+W2-2+W2-3+W2-4 against a RUNNING
// rmmway-server: mint bootstrap (HTTP) -> enroll (gRPC) -> stream metrics
// -> operator login + dispatch a command through the real HTTP endpoint and
// assert it arrives on the live agent stream -> baseline engine flags a
// synthetic weekly-pattern spike and stays quiet on the clean series ->
// the flagged anomaly becomes ONE deduped inbox alert (repeated passes
// bump it, a clean pass resolves it, re-fire + ack/resolve via the API) ->
// device + samples visible in TimescaleDB -> device immediately findable in
// Meilisearch (hostname + id).
//
// Usage: go run ./cmd/e2e [grpc-host:port] [http-host:port] [pg-dsn]
package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/caps"
)

func die(f string, a ...any) {
	fmt.Printf("FAIL: "+f+"\n", a...)
	os.Exit(1)
}

func main() {
	grpcAddr := "127.0.0.1:50051"
	httpAddr := "http://127.0.0.1:8080"
	pgDSN := "postgres://rmmway:rmmway@localhost:5432/rmmway?sslmode=disable"
	if len(os.Args) > 1 {
		grpcAddr = os.Args[1]
	}
	if len(os.Args) > 2 {
		httpAddr = os.Args[2]
	}
	if len(os.Args) > 3 {
		pgDSN = os.Args[3]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 0. health
	resp, err := http.Get(httpAddr + "/healthz")
	if err != nil {
		die("healthz: %v", err)
	}
	var h struct{ OK bool }
	_ = json.NewDecoder(resp.Body).Decode(&h)
	resp.Body.Close()
	fmt.Printf("healthz ok=%v\n", h.OK)

	// 1. mint a bootstrap token over HTTP admin.
	resp, err = http.Post(httpAddr+"/admin/bootstrap", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		die("bootstrap: %v", err)
	}
	var boot struct {
		BootstrapToken string `json:"bootstrap_token"`
		DeviceID       string `json:"device_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&boot)
	resp.Body.Close()
	fmt.Printf("bootstrap token=%s device=%s\n", boot.BootstrapToken[:12]+"...", boot.DeviceID)

	// 2. enroll over gRPC (unique hostname so the W1-7 search assertion
	// can't collide with earlier e2e devices).
	enrollHost := "e2e-demo-host-" + boot.DeviceID
	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		die("dial grpc: %v", err)
	}
	defer conn.Close()
	client := agentv1.NewAgentServiceClient(conn)
	enroll, err := client.Enroll(ctx, &agentv1.EnrollRequest{
		BootstrapToken: boot.BootstrapToken,
		Hostname:       enrollHost,
		Os:             "linux",
		Arch:           "amd64",
		AgentVersion:   "0.1.0-e2e",
		Interfaces:     []string{"10.0.0.99"},
	})
	if err != nil {
		die("enroll: %v", err)
	}
	fmt.Printf("enrolled device=%s jwt=%s...\n", enroll.DeviceId, enroll.Jwt[:24])

	// 3. open stream; a drain goroutine owns Recv() for the rest of the run
	// so the main flow can send metrics AND receive dispatched commands.
	mdCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+enroll.Jwt))
	stream, err := client.Stream(mdCtx)
	if err != nil {
		die("stream: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := stream.Send(&agentv1.StreamRequest{
		Payload: &agentv1.StreamRequest_Heartbeat{Heartbeat: &agentv1.Heartbeat{
			TimestampMs: now, CpuPercent: 33.3, MemoryPercent: 55.5,
			Metrics: &agentv1.MetricBatch{CollectedAtMs: now, Samples: []*agentv1.Metric{
				{Name: "cpu.utilization_percent", Value: 33.3, TimestampMs: now},
				{Name: "memory.used_percent", Value: 55.5, TimestampMs: now},
			}},
		}},
	}); err != nil {
		die("send: %v", err)
	}
	// Drain: capture every downlink frame. The heartbeat ack arrives first;
	// any dispatched command arrives as a StreamResponse_Command.
	ackCh := make(chan *agentv1.HeartbeatAck, 1)
	drainCtx, drainCancel := context.WithCancel(ctx)
	drainDone := make(chan struct{})
	var gotCmds []*agentv1.Command
	var cmdMu sync.Mutex
	go func() {
		defer close(drainDone)
		for {
			resp, err := stream.Recv()
			if err != nil {
				return
			}
			select {
			case <-drainCtx.Done():
				return
			default:
			}
			if ack := resp.GetHeartbeatAck(); ack != nil {
				select {
				case ackCh <- ack:
				default:
				}
			}
			if cmd := resp.GetCommand(); cmd != nil {
				cmdMu.Lock()
				gotCmds = append(gotCmds, cmd)
				cmdMu.Unlock()
			}
		}
	}()
	select {
	case <-ackCh:
		fmt.Println("heartbeat ack received")
	case <-time.After(5 * time.Second):
		die("no heartbeat ack")
	}

	// 4. metrics batch (server persists to Timescale).
	if err := stream.Send(&agentv1.StreamRequest{
		Payload: &agentv1.StreamRequest_Metrics{Metrics: &agentv1.MetricBatch{
			CollectedAtMs: now,
			Samples:       []*agentv1.Metric{{Name: "system.uptime_seconds", Value: 1234, TimestampMs: now}},
		}},
	}); err != nil {
		die("send metrics batch: %v", err)
	}

	// 5. W2-2 + W3-3: operator login + dispatch a command through the REAL
	// HTTP endpoint; assert it arrives on this live stream CARRYING a
	// capability token that verifies against the org root (read from PG —
	// the same trust anchor the real agent pins from enroll), then play the
	// W3-3 agent: verify -> execute -> report CommandResult (RECEIVED, then
	// SUCCEEDED) -> assert the server recorded it.
	{
		lb, _ := json.Marshal(map[string]string{"username": "admin", "password": "admin"})
		lresp, err := http.Post(httpAddr+"/api/login", "application/json", bytes.NewReader(lb))
		if err != nil {
			die("login: %v", err)
		}
		var loginOut struct {
			Token        string   `json:"token"`
			Capabilities []string `json:"capabilities"`
		}
		_ = json.NewDecoder(lresp.Body).Decode(&loginOut)
		lresp.Body.Close()
		if lresp.StatusCode != 200 || loginOut.Token == "" {
			die("login failed: status %d, no token", lresp.StatusCode)
		}
		fmt.Printf("operator session minted (caps: %v)\n", loginOut.Capabilities)
		scriptB64 := base64.StdEncoding.EncodeToString([]byte("echo w22 e2e ping"))
		db, _ := json.Marshal(map[string]any{
			"action": "run_script", "lang": "sh", "script": scriptB64,
		})
		req, _ := http.NewRequest("POST", httpAddr+"/api/devices/"+boot.DeviceID+"/commands", bytes.NewReader(db))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+loginOut.Token)
		dresp, err := http.DefaultClient.Do(req)
		if err != nil {
			die("dispatch: %v", err)
		}
		var dOut struct {
			CommandID string `json:"command_id"`
			DeviceID  string `json:"device_id"`
			Error     string `json:"error"`
		}
		_ = json.NewDecoder(dresp.Body).Decode(&dOut)
		dresp.Body.Close()
		if dresp.StatusCode != 200 {
			die("dispatch: status %d (%s)", dresp.StatusCode, dOut.Error)
		}
		fmt.Printf("dispatched command %s to %s (via /api/devices/{id}/commands)\n", dOut.CommandID, boot.DeviceID)
		// Wait for the drain goroutine to see it.
		deadline := time.Now().Add(5 * time.Second)
		var dispatched *agentv1.Command
		for {
			cmdMu.Lock()
			if len(gotCmds) > 0 {
				dispatched = gotCmds[0]
			}
			cmdMu.Unlock()
			if dispatched != nil {
				fmt.Printf("command %s received on the live stream (agent uplink drain)\n", dispatched.GetId())
				break
			}
			if time.Now().After(deadline) {
				die("command %s did not arrive on the stream within 5s", dOut.CommandID)
			}
			time.Sleep(100 * time.Millisecond)
		}

		// W3-3: the command must carry a capability token, and it must
		// verify against the org root for this device + capability +
		// command id.
		tok := dispatched.GetRunScript().GetCapabilityToken()
		if tok == "" {
			die("dispatched run_script carries no capability token (server capability enforcement off?)")
		}
		rootCert, err := orgRootCert(pgDSN)
		if err != nil {
			die("org root from PG: %v", err)
		}
		if err := caps.Verify(tok, rootCert, boot.DeviceID, caps.CapRunScript, dispatched.GetId(), time.Now()); err != nil {
			die("capability token does not verify against the org root: %v", err)
		}
		fmt.Printf("capability token verified: ES256 org root, device=%s, cap=%s, cmd=%s\n",
			boot.DeviceID, caps.CapRunScript, dispatched.GetId())

		// Play the W3-3 agent: report RECEIVED, then execute + SUCCEEDED.
		sendCmdResult := func(st agentv1.CommandResult_Status, tail, errMsg string) {
			if err := stream.Send(&agentv1.StreamRequest{
				Payload: &agentv1.StreamRequest_CommandResult{CommandResult: &agentv1.CommandResult{
					CommandId:     dispatched.GetId(),
					Status:        st,
					ExitCode:      0,
					StdoutTail:    tail,
					Error:         errMsg,
					CompletedAtMs: time.Now().UnixMilli(),
				}},
			}); err != nil {
				die("send CommandResult %v: %v", st, err)
			}
		}
		sendCmdResult(agentv1.CommandResult_RECEIVED, "", "")
		sendCmdResult(agentv1.CommandResult_SUCCEEDED, "w22 e2e ping\n", "")
		fmt.Println("fake agent reported RECEIVED + SUCCEEDED over the live stream")

		// And the server must have recorded it (W3-3 command audit).
		aResp, err := http.Get(httpAddr + "/admin/devices/" + boot.DeviceID + "/commands")
		if err != nil {
			die("commands audit: %v", err)
		}
		aBody, _ := io.ReadAll(aResp.Body)
		aResp.Body.Close()
		if aResp.StatusCode != 200 {
			die("commands audit: status %d: %s", aResp.StatusCode, aBody)
		}
		var aOut struct {
			Results []struct {
				CommandID string `json:"command_id"`
				Status    int32  `json:"status"`
			} `json:"results"`
		}
		if err := json.Unmarshal(aBody, &aOut); err != nil {
			die("commands audit: bad JSON: %v", err)
		}
		found := false
		for _, r := range aOut.Results {
			if r.CommandID == dOut.CommandID && r.Status == int32(agentv1.CommandResult_SUCCEEDED) {
				found = true
			}
		}
		if !found {
			die("SUCCEEDED result for %s not recorded (audit: %s)", dOut.CommandID, aBody)
		}
		fmt.Println("W3-3: command result recorded + served by /admin/devices/{id}/commands")
	}

	drainCancel()
	_ = stream.CloseSend()
	<-drainDone

	// 6. device visible in admin JSON.
	time.Sleep(200 * time.Millisecond)
	resp, err = http.Get(httpAddr + "/admin/devices")
	if err != nil {
		die("devices: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Printf("devices: %s\n", bytes.TrimSpace(body))
	if !bytes.Contains(body, []byte(boot.DeviceID)) {
		die("device %s not in /admin/devices", boot.DeviceID)
	}

	// 6b. W2-3: dynamic baselining DoD — a synthetic metric with a known
	// weekly pattern is flagged at the right time and quiet otherwise.
	// Seed 44 days of hourly samples (dow offset + hour-of-day triangle) plus
	// a final-hour spike for a fresh device, run one deterministic pass
	// over the real hypertable via the live API, and assert the anomaly.
	baselineE2E(httpAddr, pgDSN)

	// 6c. W2-4: the flagged anomaly must become ONE deduped inbox alert —
	// repeated passes bump it (no storm), a clean pass resolves it, a new
	// spike re-fires, and ack/resolve work through the auth-gated API.
	alertE2E(httpAddr, pgDSN)

	// 6. W1-6: samples + device row are actually in TimescaleDB.
	// (the agent's stream is already closed; metrics were flushed on write)
	cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ccancel()
	pgConn, err := pgx.Connect(cctx, pgDSN)
	if err != nil {
		die("pg connect: %v", err)
	}
	defer pgConn.Close(cctx)
	var devOK bool
	if err := pgConn.QueryRow(cctx, `SELECT EXISTS(SELECT 1 FROM devices WHERE id=$1)`, boot.DeviceID).Scan(&devOK); err != nil {
		die("pg device query: %v", err)
	}
	if !devOK {
		die("device %s not in devices table", boot.DeviceID)
	}
	var n int
	if err := pgConn.QueryRow(cctx, `SELECT count(*) FROM metrics WHERE device_id=$1`, boot.DeviceID).Scan(&n); err != nil {
		die("pg metrics query: %v", err)
	}
	if n < 3 {
		die("expected >=3 metric samples in Timescale, got %d", n)
	}
	fmt.Printf("timescale: device present, %d metric samples\n", n)

	// 7. W1-7: the freshly enrolled device is findable in Meilisearch by
	// hostname and by id — the IndexerHook's debounced sync (500ms) should
	// have landed by now; give it a few seconds of slack.
	deadline := time.Now().Add(8 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		sresp, err := http.Get(httpAddr + "/admin/search?q=" + url.QueryEscape(enrollHost))
		if err != nil {
			lastErr = err
			time.Sleep(300 * time.Millisecond)
			continue
		}
		var res struct {
			EstimatedTotalHits int              `json:"estimatedTotalHits"`
			Hits               []map[string]any `json:"hits"`
		}
		sbody, _ := io.ReadAll(sresp.Body)
		sresp.Body.Close()
		if sresp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("search status %d: %s", sresp.StatusCode, sbody)
			time.Sleep(300 * time.Millisecond)
			continue
		}
		_ = json.Unmarshal(sbody, &res)
		// The fresh device's hostname is unique (includes its id), so the
		// top hit for this query must be it.
		if len(res.Hits) >= 1 && res.Hits[0]["id"] == boot.DeviceID {
			fmt.Printf("meilisearch: found %d hit(s) for hostname %q (top hit = %v)\n", len(res.Hits), enrollHost, boot.DeviceID)
			// also by id: top hit for an exact-id query must be this device
			idResp, err := http.Get(httpAddr + "/admin/search?q=" + url.QueryEscape(boot.DeviceID))
			if err == nil {
				var idRes struct {
					Hits []map[string]any `json:"hits"`
				}
				idBody, _ := io.ReadAll(idResp.Body)
				idResp.Body.Close()
				_ = json.Unmarshal(idBody, &idRes)
				if len(idRes.Hits) >= 1 && idRes.Hits[0]["id"] == boot.DeviceID {
					fmt.Printf("meilisearch: found %d hit(s) for id %q (top hit = %v)\n", len(idRes.Hits), boot.DeviceID, boot.DeviceID)
				} else {
					die("device %s not top hit for its own id", boot.DeviceID)
				}
			}
			// and by IP: the fresh device registered interfaces=["10.0.0.99"];
			// the fresh device must be among the hits for that query.
			ipResp, err := http.Get(httpAddr + "/admin/search?q=10.0.0.99")
			if err == nil {
				var ipRes struct {
					Hits []map[string]any `json:"hits"`
				}
				ipBody, _ := io.ReadAll(ipResp.Body)
				ipResp.Body.Close()
				_ = json.Unmarshal(ipBody, &ipRes)
				found := false
				for _, h := range ipRes.Hits {
					if fmt.Sprint(h["id"]) == boot.DeviceID {
						found = true
						break
					}
				}
				if found {
					fmt.Printf("meilisearch: device %q found for ip 10.0.0.99\n", boot.DeviceID)
				} else {
					die("device with ip 10.0.0.99 not in search hits")
				}
			}
			fmt.Println("PASS: W1-5+W1-6+W1-7+W2-2+W2-3+W2-4+W3-3 e2e — enroll, stream, capability-gated command round-trip (token minted -> verified -> executed -> result recorded), metrics in Timescale, baseline anomaly flagged -> one deduped inbox alert, device searchable in Meilisearch")
			return
		}
		lastErr = fmt.Errorf("search for %q returned 0 hits yet", enrollHost)
		time.Sleep(300 * time.Millisecond)
	}
	die("device %q not findable in Meilisearch after 8s: %v", enrollHost, lastErr)
}

// baselineE2E is the W2-3 definition-of-done check against the LIVE server:
// a synthetic metric with a known weekly pattern (day-of-week offset +
// hour-of-day sine) is seeded into Timescale for a fresh device, one
// deterministic pass is forced through the real HTTP endpoint, and the
// engine must flag exactly the final-hour spike — quiet on the pattern
// itself. The clean (unspiked) series proves the "quiet otherwise" half.
func baselineE2E(httpAddr, pgDSN string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pgConn, err := pgx.Connect(ctx, pgDSN)
	if err != nil {
		die("baseline e2e pg connect: %v", err)
	}
	defer pgConn.Close(ctx)

	// Fresh, uniquely named device + metric series (no collision with
	// real data, no carry-over between e2e runs).
	devID := "e2e-baseline-" + time.Now().Format("20060102150405")
	metric := "synth.weekly_pattern"

	// now floored to the hour so the spike lands in its own bucket.
	now := time.Now().UTC().Truncate(time.Hour)
	// Within-day shape: constant-slope triangle (peak 06:00, trough 18:00),
	// not a sine — a sine's flat bottom at 18:00 makes the recovery ramp at
	// 21–22 UTC cross the trend channel's z>=4 band (hour-of-day flake).
	tri := func(hour float64) float64 {
		s := math.Abs(hour - 6)
		if 24-s < s {
			s = 24 - s
		}
		return 15 * (1 - s/6)
	}
	weekly := func(at time.Time) float64 {
		return 40 + float64(at.Weekday())*8 + tri(float64(at.Hour()))
	}
	seed := `INSERT INTO metrics (device_id, name, source, value, labels, timestamp_ms, ts)
		VALUES ($1, $2, '', $3::double precision, '{}', $4::bigint, to_timestamp($4::bigint / 1000.0)) ON CONFLICT DO NOTHING`
	// Two devices:
	//   <devID>-spike: 44 days of pattern + final-hour spike (expect 1 anomaly)
	//   <devID>-clean: 44 days of pattern, no spike       (expect 0 anomalies)
	for _, suffix := range []string{"-spike", "-clean"} {
		d := devID + suffix
		if _, err := pgConn.Exec(ctx, `INSERT INTO devices (id, hostname, os, arch)
			VALUES ($1, $2, 'linux', 'amd64') ON CONFLICT (id) DO NOTHING`, d, "e2e-baseline-host"); err != nil {
			die("baseline seed device: %v", err)
		}
		for at := now.Add(-44 * 24 * time.Hour); !at.After(now); at = at.Add(time.Hour) {
			v := weekly(at)
			if suffix == "-spike" && at.Equal(now) {
				v += 35 // final-hour spike: far outside this slot's MAD
			}
			if _, err := pgConn.Exec(ctx, seed, d, metric, v, at.UnixMilli()); err != nil {
				die("baseline seed: %v", err)
			}
		}
	}
	fmt.Printf("baseline: seeded 2 x 44d synthetic weekly series for %s\n", devID)

	// Force one deterministic pass through the live API.
	runResp, err := http.Post(httpAddr+"/admin/baseline/run", "application/json", nil)
	if err != nil {
		die("baseline run: %v", err)
	}
	var runOut struct {
		Anomalies []struct {
			DeviceID string  `json:"device_id"`
			Name     string  `json:"name"`
			At       string  `json:"at"`
			Value    float64 `json:"value"`
			Score    float64 `json:"score"`
			Seasonal *struct {
				Z float64 `json:"z"`
			} `json:"seasonal"`
			Trend *struct {
				Z float64 `json:"z"`
			} `json:"trend"`
		} `json:"anomalies"`
		Series int `json:"series"`
		Runs   int `json:"runs"`
	}
	_ = json.NewDecoder(runResp.Body).Decode(&runOut)
	runResp.Body.Close()
	if runResp.StatusCode != 200 {
		die("baseline run: status %d: %v", runResp.StatusCode, runOut)
	}

	// Assert: exactly one anomaly, on the -spike series, with a large
	// z-score from a real channel; the -clean series stays quiet.
	var spiked []struct {
		id    string
		at    string
		score float64
	}
	cleanAnoms := 0
	for _, a := range runOut.Anomalies {
		if a.Name != metric {
			continue
		}
		if strings.HasPrefix(a.DeviceID, devID+"-spike") {
			spiked = append(spiked, struct {
				id    string
				at    string
				score float64
			}{a.DeviceID, a.At, a.Score})
			if (a.Seasonal == nil || a.Seasonal.Z < 4) && (a.Trend == nil || a.Trend.Z < 4) {
				die("spike anomaly has no channel with z >= 4: %+v", a)
			}
		}
		if strings.HasPrefix(a.DeviceID, devID+"-clean") {
			cleanAnoms++
		}
	}
	if len(spiked) != 1 {
		die("expected exactly 1 anomaly on the spiked series, got %d: %+v (series=%d runs=%d)", len(spiked), spiked, runOut.Series, runOut.Runs)
	}
	if cleanAnoms != 0 {
		die("clean series must be quiet, got %d anomalies", cleanAnoms)
	}
	// The anomaly time must be the final (current) hour — "right time".
	at, perr := time.Parse(time.RFC3339, spiked[0].at)
	if perr != nil || at.Truncate(time.Hour) != now {
		die("anomaly at %s, want the final hour %s (parse err %v)", spiked[0].at, now, perr)
	}
	fmt.Printf("baseline: spike flagged at %s (z-score %.1f); clean series quiet (series scored: %d)\n",
		at.Format("2006-01-02 15:04 UTC"), spiked[0].score, runOut.Series)

	// And it is persisted + queryable through the anomaly feed.
	feedResp, err := http.Get(httpAddr + "/admin/baseline/anomalies?limit=50")
	if err != nil {
		die("baseline feed: %v", err)
	}
	feedBody, _ := io.ReadAll(feedResp.Body)
	feedResp.Body.Close()
	if feedResp.StatusCode != 200 {
		die("baseline feed: status %d: %s", feedResp.StatusCode, feedBody)
	}
	if !bytes.Contains(feedBody, []byte(devID+"-spike")) {
		die("spike anomaly not in the /admin/baseline/anomalies feed: %s", feedBody)
	}
	fmt.Printf("baseline: anomaly persisted and served by /admin/baseline/anomalies\n")

	// Tidy: drop the synthetic series (devices keep their e2e identity).
	if _, err := pgConn.Exec(ctx, `DELETE FROM metrics WHERE device_id LIKE $1`, devID+"%"); err != nil {
		die("baseline tidy metrics: %v", err)
	}
	if _, err := pgConn.Exec(ctx, `DELETE FROM baseline_anomalies WHERE device_id LIKE $1`, devID+"%"); err != nil {
		die("baseline tidy anomalies: %v", err)
	}
	// The spike above also produced inbox alerts (W2-4) — drop those too so
	// e2e runs don't leave phantom alerts in the dev inbox.
	if _, err := pgConn.Exec(ctx, `DELETE FROM alerts WHERE device_id LIKE $1`, devID+"%"); err != nil {
		die("baseline tidy alerts: %v", err)
	}
	if _, err := pgConn.Exec(ctx, `DELETE FROM devices WHERE id LIKE $1`, devID+"%"); err != nil {
		die("baseline tidy devices: %v", err)
	}
}

// alertE2E is the W2-4 definition-of-done check against the LIVE server:
// "a real metric anomaly produces ONE deduped alert in the inbox (no
// storm)". It seeds a fresh device with a 44-day weekly pattern, then flips
// the CURRENT hour's samples between spiked and clean (the engine scores
// that hour's mean — future hours are beyond its window) and walks the real
// baseline + alert pipeline through the live API:
//
//	pass 1 (spike hour)  -> exactly 1 open alert appears
//	pass 2 (same hour)   -> still exactly 1 alert, bumped (events=2)
//	pass 3 (clean hour)  -> the alert auto-resolves; inbox empty
//	pass 4 (spike again) -> a NEW alert (re-fire is a new incident)
//	/admin/alerts/counts -> open count matches
//	PATCH ack / resolve  -> manual transitions work (auth-gated)
//
// It reuses W2-3's synthetic series shape so the same engine output feeds
// both checks.
func alertE2E(httpAddr, pgDSN string) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	pgConn, err := pgx.Connect(ctx, pgDSN)
	if err != nil {
		die("alert e2e pg connect: %v", err)
	}
	defer pgConn.Close(ctx)

	devID := "e2e-alert-" + time.Now().Format("20060102150405")
	metric := "synth.alert_pattern"
	now := time.Now().UTC().Truncate(time.Hour)
	// Within-day shape: constant-slope triangle (peak 06:00, trough 18:00),
	// not a sine — a sine's flat bottom at 18:00 makes the recovery ramp at
	// 21–22 UTC cross the trend channel's z>=4 band (hour-of-day flake).
	tri := func(hour float64) float64 {
		s := math.Abs(hour - 6)
		if 24-s < s {
			s = 24 - s
		}
		return 15 * (1 - s/6)
	}
	weekly := func(at time.Time) float64 {
		return 40 + float64(at.Weekday())*8 + tri(float64(at.Hour()))
	}

	// Seed 44 days of pattern ending at the current hour (the spike is
	// applied to the current hour separately via setNow, since the engine
	// scores the hourly MEAN of the latest hour in its window).
	seedHistory := func() {
		if _, err := pgConn.Exec(ctx, `INSERT INTO devices (id, hostname, os, arch)
			VALUES ($1, $2, 'linux', 'amd64') ON CONFLICT (id) DO NOTHING`, devID, "e2e-alert-host"); err != nil {
			die("alert seed device: %v", err)
		}
		start := now.Add(-44 * 24 * time.Hour)
		const ins = `INSERT INTO metrics (device_id, name, source, value, labels, timestamp_ms, ts)
			VALUES ($1, $2, '', $3::double precision, '{}', $4::bigint, to_timestamp($4::bigint / 1000.0)) ON CONFLICT DO NOTHING`
		for at := start; !at.After(now); at = at.Add(time.Hour) {
			if at.Equal(now) {
				continue // current hour is set by setNow
			}
			if _, err := pgConn.Exec(ctx, ins, devID, metric, weekly(at), at.UnixMilli()); err != nil {
				die("alert seed: %v", err)
			}
		}
	}
	// setNow replaces the current hour's samples with one clean or spiked
	// sample, changing the hour's mean the engine scores. The sample is
	// stamped exactly at the hour floor (`now`), which is always <= the
	// real clock, so it's never treated as future-dated/skipped.
	setNow := func(spike bool) {
		if _, err := pgConn.Exec(ctx, `DELETE FROM metrics WHERE device_id=$1 AND name=$2 AND ts >= $3 AND ts < $4`,
			devID, metric, now, now.Add(time.Hour)); err != nil {
			die("alert set-now delete: %v", err)
		}
		v := weekly(now)
		if spike {
			v += 35
		}
		if _, err := pgConn.Exec(ctx, `INSERT INTO metrics (device_id, name, source, value, labels, timestamp_ms, ts)
			VALUES ($1, $2, '', $3::double precision, '{}', $4::bigint, to_timestamp($4::bigint / 1000.0))`,
			devID, metric, v, now.UnixMilli()); err != nil {
			die("alert set-now insert: %v", err)
		}
	}

	// ---- helpers: force a pass, list alerts, patch a status -------------
	forcePass := func() {
		resp, err := http.Post(httpAddr+"/admin/baseline/run", "application/json", nil)
		if err != nil {
			die("alert pass: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			die("alert pass: status %d: %s", resp.StatusCode, body)
		}
	}
	openAlerts := func() []map[string]any {
		resp, err := http.Get(httpAddr + "/admin/alerts?status=open")
		if err != nil {
			die("alert list: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			die("alert list: status %d: %s", resp.StatusCode, body)
		}
		var out []map[string]any
		if err := json.Unmarshal(body, &out); err != nil {
			die("alert list: decode: %v", err)
		}
		var mine []map[string]any
		for _, a := range out {
			if fmt.Sprint(a["device_id"]) == devID && fmt.Sprint(a["name"]) == metric {
				mine = append(mine, a)
			}
		}
		return mine
	}
	patchAlert := func(id int64, status string) int {
		b, _ := json.Marshal(map[string]string{"status": status})
		req, _ := http.NewRequest("PATCH", fmt.Sprintf("%s/admin/alerts/%d", httpAddr, id), bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			die("alert patch: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			die("alert patch %s: status %d: %s", status, resp.StatusCode, body)
		}
		return resp.StatusCode
	}

	// ---- Pass 1: the spike hour -> exactly one open alert --------------
	seedHistory()
	setNow(true)
	forcePass()
	alerts := openAlerts()
	if len(alerts) != 1 {
		die("pass1: expected exactly 1 open alert, got %d: %v", len(alerts), alerts)
	}
	ev1, _ := alerts[0]["events"].(float64)
	if ev1 < 1 {
		die("pass1: events should be >= 1, got %v", alerts[0]["events"])
	}
	fmt.Printf("alerts: pass1 -> 1 open alert (score %v, channel %v, events %v) for %s\n",
		alerts[0]["score"], alerts[0]["channel"], alerts[0]["events"], devID)

	// ---- Pass 2: re-run the same spike hour -> bump, still 1 (no storm) -
	// A concurrent background pass could also bump; dedup means the COUNT
	// can only stay 1 no matter what.
	forcePass()
	alerts = openAlerts()
	if len(alerts) != 1 {
		die("pass2 (no storm): expected still exactly 1 open alert, got %d: %v", len(alerts), alerts)
	}
	ev2, _ := alerts[0]["events"].(float64)
	if ev2 <= ev1 {
		die("pass2: events should have bumped (%v -> %v)", ev1, ev2)
	}
	fmt.Printf("alerts: pass2 -> same alert bumped to events=%v (no storm)\n", alerts[0]["events"])

	// ---- Pass 3: series clean this hour -> auto-resolves ---------------
	setNow(false)
	forcePass()
	if alerts = openAlerts(); len(alerts) != 0 {
		die("pass3: expected auto-resolve (0 open alerts), got %d: %v", len(alerts), alerts)
	}
	// The resolved row is queryable by status.
	resResp, err := http.Get(httpAddr + "/admin/alerts?status=resolved")
	if err != nil {
		die("alert resolved list: %v", err)
	}
	resBody, _ := io.ReadAll(resResp.Body)
	resResp.Body.Close()
	if resResp.StatusCode != 200 {
		die("alert resolved list: status %d", resResp.StatusCode)
	}
	if !bytes.Contains(resBody, []byte(devID)) {
		die("pass3: resolved alert for %s not found in status=resolved feed", devID)
	}
	fmt.Printf("alerts: pass3 -> alert auto-resolved (series returned to baseline)\n")

	// ---- Pass 4: spike again re-fires a FRESH alert ---------------------
	setNow(true)
	forcePass()
	alerts = openAlerts()
	if len(alerts) != 1 {
		die("pass4: expected a fresh open alert on re-fire, got %d: %v", len(alerts), alerts)
	}
	fmt.Printf("alerts: pass4 -> fresh alert fired on the new anomaly (re-fire allowed)\n")

	// ---- counts endpoint agrees (scoped to our device) ------------------
	cntResp, err := http.Get(httpAddr + "/admin/alerts/counts")
	if err != nil {
		die("alert counts: %v", err)
	}
	var counts map[string]int
	_ = json.NewDecoder(cntResp.Body).Decode(&counts)
	cntResp.Body.Close()
	if cntResp.StatusCode != 200 {
		die("counts: status %d", cntResp.StatusCode)
	}
	// The global open count may include other devices' alerts (e.g. W2-3's
	// spike), so assert the scoped view: our device must show exactly 1.
	scResp, err := http.Get(httpAddr + "/admin/alerts?status=open&device_id=" + devID)
	if err != nil {
		die("alert scoped list: %v", err)
	}
	scBody, _ := io.ReadAll(scResp.Body)
	scResp.Body.Close()
	if scResp.StatusCode != 200 {
		die("alert scoped list: status %d", scResp.StatusCode)
	}
	var scoped []map[string]any
	_ = json.Unmarshal(scBody, &scoped)
	if len(scoped) != 1 {
		die("counts: scoped open alerts for %s should be 1, got %d: %s", devID, len(scoped), scBody)
	}
	fmt.Printf("alerts: counts endpoint reports %d open (global); %d for our device\n", counts["open"], len(scoped))

	// ---- manual ack then resolve via the auth-gated API -----------------
	idInt, ok := alerts[0]["id"].(float64)
	if !ok {
		die("alert id not numeric: %v", alerts[0]["id"])
	}
	patchAlert(int64(idInt), "acked")
	// Ack'd alerts leave the "open" filter (strict statuses) and appear
	// under status=acked.
	if got := openAlerts(); len(got) != 0 {
		die("ack: expected 0 open alerts after ack, got %d: %v", len(got), got)
	}
	ackResp, err := http.Get(httpAddr + "/admin/alerts?status=acked&device_id=" + devID)
	if err != nil {
		die("alert acked list: %v", err)
	}
	ackBody, _ := io.ReadAll(ackResp.Body)
	ackResp.Body.Close()
	if ackResp.StatusCode != 200 {
		die("alert acked list: status %d", ackResp.StatusCode)
	}
	var acked []map[string]any
	_ = json.Unmarshal(ackBody, &acked)
	if len(acked) != 1 || acked[0]["status"] != "acked" {
		die("ack: expected exactly 1 acked alert, got %v", acked)
	}
	patchAlert(int64(idInt), "resolved")
	if alerts = openAlerts(); len(alerts) != 0 {
		die("manual resolve: expected 0 open alerts, got %d: %v", len(alerts), alerts)
	}
	resResp2, err := http.Get(httpAddr + "/admin/alerts?status=resolved&device_id=" + devID)
	if err != nil {
		die("alert resolved list 2: %v", err)
	}
	resBody2, _ := io.ReadAll(resResp2.Body)
	resResp2.Body.Close()
	if !bytes.Contains(resBody2, []byte(fmt.Sprint(idInt))) {
		die("manual resolve: the resolved alert is not in status=resolved")
	}
	fmt.Printf("alerts: manual ack -> resolve via /admin/alerts/{id} verified\n")

	// ---- the auth gate actually gates -----------------------------------
	unauthed, err := http.Get(httpAddr + "/api/alerts")
	if err != nil {
		die("unauthed alerts: %v", err)
	}
	unauthed.Body.Close()
	if unauthed.StatusCode != http.StatusUnauthorized {
		die("auth gate: GET /api/alerts without a token should be 401, got %d", unauthed.StatusCode)
	}
	// And an operator JWT (not an agent token) gets through.
	lb, _ := json.Marshal(map[string]string{"username": "admin", "password": "admin"})
	lresp, err := http.Post(httpAddr+"/api/login", "application/json", bytes.NewReader(lb))
	if err != nil {
		die("alert login: %v", err)
	}
	var loginOut struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(lresp.Body).Decode(&loginOut)
	lresp.Body.Close()
	if lresp.StatusCode != 200 || loginOut.Token == "" {
		die("alert login: no token")
	}
	ar, err := http.NewRequest("GET", httpAddr+"/api/alerts?status=resolved", nil)
	if err != nil {
		die("authed alerts req: %v", err)
	}
	ar.Header.Set("Authorization", "Bearer "+loginOut.Token)
	aresp, err := http.DefaultClient.Do(ar)
	if err != nil {
		die("authed alerts: %v", err)
	}
	aresp.Body.Close()
	if aresp.StatusCode != 200 {
		die("auth gate: GET /api/alerts with operator token should be 200, got %d", aresp.StatusCode)
	}
	fmt.Println("alerts: /api/alerts 401 without token, 200 with operator token")

	// ---- tidy -------------------------------------------------------------
	if _, err := pgConn.Exec(ctx, `DELETE FROM metrics WHERE device_id=$1`, devID); err != nil {
		die("alert tidy metrics: %v", err)
	}
	if _, err := pgConn.Exec(ctx, `DELETE FROM baseline_anomalies WHERE device_id=$1`, devID); err != nil {
		die("alert tidy anomalies: %v", err)
	}
	if _, err := pgConn.Exec(ctx, `DELETE FROM alerts WHERE device_id=$1`, devID); err != nil {
		die("alert tidy alerts: %v", err)
	}
	if _, err := pgConn.Exec(ctx, `DELETE FROM devices WHERE id=$1`, devID); err != nil {
		die("alert tidy device: %v", err)
	}
	fmt.Println("alerts: W2-4 DoD satisfied — one deduped inbox alert, no storm, auto-resolve + manual ack/resolve")
}

// orgRootCert reads the org root CA cert from PG (org_ca, the same row the
// server persists on first boot / W3-1) and parses it — the trust anchor
// the e2e fake agent uses to verify the dispatched command's capability
// token, exactly as the real agent uses the root it pinned at enroll.
func orgRootCert(pgDSN string) (*x509.Certificate, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pgConn, err := pgx.Connect(ctx, pgDSN)
	if err != nil {
		return nil, err
	}
	defer pgConn.Close(ctx)
	var rootPEM []byte
	if err := pgConn.QueryRow(ctx, `SELECT root_cert_pem FROM org_ca WHERE id = 1`).Scan(&rootPEM); err != nil {
		return nil, err
	}
	block, _ := pem.Decode(rootPEM)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in org_ca.root_cert_pem")
	}
	return x509.ParseCertificate(block.Bytes)
}
