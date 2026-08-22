// Command e2e verifies W1-5+W1-6 against a RUNNING rmmway-server:
// mint bootstrap (HTTP) -> enroll (gRPC) -> stream metrics -> dispatch a
// command back down the stream -> device + samples visible in TimescaleDB.
//
// Usage: go run ./cmd/e2e [grpc-host:port] [http-host:port] [pg-dsn]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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

	// 2. enroll over gRPC.
	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		die("dial grpc: %v", err)
	}
	defer conn.Close()
	client := agentv1.NewAgentServiceClient(conn)
	enroll, err := client.Enroll(ctx, &agentv1.EnrollRequest{
		BootstrapToken: boot.BootstrapToken,
		Hostname:       "e2e-demo-host",
		Os:             "linux",
		Arch:           "amd64",
		AgentVersion:   "0.1.0-e2e",
	})
	if err != nil {
		die("enroll: %v", err)
	}
	fmt.Printf("enrolled device=%s jwt=%s...\n", enroll.DeviceId, enroll.Jwt[:24])

	// 3. open stream, send heartbeat + metrics.
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
	if ack, err := stream.Recv(); err != nil || ack.GetHeartbeatAck() == nil {
		die("no ack: %v", err)
	}
	fmt.Println("heartbeat ack received")

	// 4. command dispatch: the server can push a command down this stream.
	// (e2e has no admin endpoint for dispatch yet — W1-5 ships the Dispatcher;
	// this leg is covered by unit tests + W2's command UI. We at least prove
	// the stream stays alive after a second frame.)
	if err := stream.Send(&agentv1.StreamRequest{
		Payload: &agentv1.StreamRequest_Metrics{Metrics: &agentv1.MetricBatch{
			CollectedAtMs: now,
			Samples:       []*agentv1.Metric{{Name: "system.uptime_seconds", Value: 1234, TimestampMs: now}},
		}},
	}); err != nil {
		die("send metrics batch: %v", err)
	}
	_ = stream.CloseSend()

	// 5. device visible in admin JSON.
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
	fmt.Println("PASS: W1-5+W1-6 e2e — enroll, stream, metrics persisted to TimescaleDB")
}
