// Command e2e verifies W1-5+W1-6+W1-7+W2-2+W2-3 against a RUNNING
// rmmway-server: mint bootstrap (HTTP) -> enroll (gRPC) -> stream metrics
// -> operator login + dispatch a command through the real HTTP endpoint and
// assert it arrives on the live agent stream -> baseline engine flags a
// synthetic weekly-pattern spike and stays quiet on the clean series ->
// device + samples visible in TimescaleDB -> device immediately findable in
// Meilisearch (hostname + id).
//
// Usage: go run ./cmd/e2e [grpc-host:port] [http-host:port] [pg-dsn]
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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
	var gotCmds []string
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
				gotCmds = append(gotCmds, cmd.GetId())
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

	// 5. W2-2: operator login + dispatch a command through the REAL HTTP
	// endpoint; assert it arrives on this live stream. The agent only logs
	// receipt (execution + CommandResult reporting is W5-1); proving the
	// dispatch half reaches the owning agent's stream is the W2-2 bar.
	{
		lb, _ := json.Marshal(map[string]string{"username": "admin", "password": "admin"})
		lresp, err := http.Post(httpAddr+"/api/login", "application/json", bytes.NewReader(lb))
		if err != nil {
			die("login: %v", err)
		}
		var loginOut struct {
			Token string `json:"token"`
		}
		_ = json.NewDecoder(lresp.Body).Decode(&loginOut)
		lresp.Body.Close()
		if lresp.StatusCode != 200 || loginOut.Token == "" {
			die("login failed: status %d, no token", lresp.StatusCode)
		}
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
		for {
			cmdMu.Lock()
			n := len(gotCmds)
			cmdMu.Unlock()
			if n > 0 {
				fmt.Printf("command %s received on the live stream (agent uplink drain)\n", gotCmds[0])
				break
			}
			if time.Now().After(deadline) {
				die("command %s did not arrive on the stream within 5s", dOut.CommandID)
			}
			time.Sleep(100 * time.Millisecond)
		}
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
	// Seed 44 days of hourly samples (dow offset + hour-of-day sine) plus
	// a final-hour spike for a fresh device, run one deterministic pass
	// over the real hypertable via the live API, and assert the anomaly.
	baselineE2E(httpAddr, pgDSN)

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
			fmt.Println("PASS: W1-5+W1-6+W1-7+W2-2+W2-3 e2e — enroll, stream, command dispatched to live stream, metrics in Timescale, baseline anomaly flagged, device searchable in Meilisearch")
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
	weekly := func(at time.Time) float64 {
		return 40 + float64(at.Weekday())*8 + 15*math.Sin(2*math.Pi*float64(at.Hour())/24)
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
			DeviceID string   `json:"device_id"`
			Name     string   `json:"name"`
			At       string   `json:"at"`
			Value    float64  `json:"value"`
			Score    float64  `json:"score"`
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
	if _, err := pgConn.Exec(ctx, `DELETE FROM devices WHERE id LIKE $1`, devID+"%"); err != nil {
		die("baseline tidy devices: %v", err)
	}
}
