// Command e2e verifies W1-5+W1-6+W1-7+W2-2 against a RUNNING rmmway-server:
// mint bootstrap (HTTP) -> enroll (gRPC) -> stream metrics -> operator login
// + dispatch a command through the real HTTP endpoint and assert it arrives
// on the live agent stream -> device + samples visible in TimescaleDB ->
// device immediately findable in Meilisearch (hostname + id).
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
	"net/http"
	"net/url"
	"os"
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
			fmt.Println("PASS: W1-5+W1-6+W1-7+W2-2 e2e — enroll, stream, command dispatched to live stream, metrics in Timescale, device searchable in Meilisearch")
			return
		}
		lastErr = fmt.Errorf("search for %q returned 0 hits yet", enrollHost)
		time.Sleep(300 * time.Millisecond)
	}
	die("device %q not findable in Meilisearch after 8s: %v", enrollHost, lastErr)
}
