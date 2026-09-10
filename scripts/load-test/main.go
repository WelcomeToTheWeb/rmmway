// load-test — RMMWay synthetic agent load test harness.
//
// Simulates N fake agents that enroll, connect, and send heartbeats +
// metric batches to a live RMMWay server. Reports throughput, latency,
// and resource usage during the test period.
//
// Usage: ./load-test -n 5000 -duration 15m -server localhost
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// Config from flags.
var (
	numDevices     = flag.Int("n", 5000, "number of synthetic devices")
	duration       = flag.Duration("duration", 15*time.Minute, "how long to run the load phase")
	serverHost     = flag.String("server", "localhost", "server hostname")
	grpcPort       = flag.Int("grpc-port", 50051, "gRPC port")
	httpPort       = flag.Int("http-port", 8080, "HTTP API port")
	heartbeatInt   = flag.Duration("heartbeat", 60*time.Second, "heartbeat interval")
	metricsInt     = flag.Duration("metrics", 60*time.Second, "metric batch interval")
	alertRate      = flag.Float64("alert-rate", 0.01, "fraction of devices generating anomalous metrics (0-1)")
	parallel       = flag.Int("parallel", 100, "parallel connections per phase")
	reportInterval = flag.Duration("report-interval", 30*time.Second, "progress reporting interval")
	outputFile     = flag.String("output", "load-test-report.json", "report output file")
)

// Stats counters for the load test.
type Stats struct {
	enrolled        int64
	streamed        int64
	heartbeatsSent  int64
	heartbeatsAcked int64
	metricsSent     int64
	alertsFired     int64
	errors          int64
}

// Device tracks a synthetic agent's identity.
type Device struct {
	id    string
	jwt   string
}

// StreamClient is the generated gRPC client interface.
type StreamClient interface {
	agentv1.AgentService_StreamClient
}

// Harness orchestrates the load test.
type Harness struct {
	devices []*Device
	client  agentv1.AgentServiceClient
	conn    *grpc.ClientConn
	stats   *Stats
	rng     *rand.Rand
	rngMu   sync.Mutex
}

// NewHarness creates a new load test harness.
func NewHarness(conn *grpc.ClientConn) *Harness {
	return &Harness{
		conn:  conn,
		stats: &Stats{},
		rng:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// run executes the full load test.
func (h *Harness) run(ctx context.Context) error {
	// Phase 1: Enroll devices.
	log.Printf("Phase 1: Enrolling %d devices...", *numDevices)
	enrollStart := time.Now()
	h.enrollBatch(ctx, *numDevices)
	enrollDur := time.Since(enrollStart)
	log.Printf("Enrolled %d devices in %v", atomic.LoadInt64(&h.stats.enrolled), enrollDur)

	// Phase 2: Run load phase — agents send heartbeats and metrics.
	log.Printf("Phase 2: Load phase for %v...", *duration)
	loadStart := time.Now()
	h.runLoadPhase(ctx)
	loadDur := time.Since(loadStart)
	log.Printf("Load phase completed in %v", loadDur)

	// Collect final stats.
	return nil
}

// enrollBatch — mint bootstrap tokens and enroll each fake agent via HTTP.
func (h *Harness) enrollBatch(ctx context.Context, count int) {
	httpBase := fmt.Sprintf("http://%s:%d", *serverHost, *httpPort)

	// Create a channel to batch enrollments.
	devCh := make(chan int, count)
	defer close(devCh)

	// Spawn enrollment workers.
	var wg sync.WaitGroup
	for w := 0; w < *parallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range devCh {
				h.enrollOne(ctx, httpBase, i)
			}
		}()
	}

	// Send enrollment tasks.
	for i := 0; i < count; i++ {
		devCh <- i
	}

	wg.Wait()
}

// enrollOne enrolls a single fake agent.
func (h *Harness) enrollOne(ctx context.Context, httpBase string, idx int) {
	// Generate a unique hostname.
	hostname := fmt.Sprintf("synth-%04d.local", idx)

	// Mint a bootstrap token via HTTP.
	resp, err := http.Post(fmt.Sprintf("%s/api/bootstrap", httpBase),
		"application/json", nil)
	if err != nil {
		log.Printf("Failed to mint token for device %d: %v", idx, err)
		atomic.AddInt64(&h.stats.errors, 1)
		return
	}
	defer resp.Body.Close()

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log.Printf("Failed to decode bootstrap response for device %d: %v", idx, err)
		atomic.AddInt64(&h.stats.errors, 1)
		return
	}

	token, ok := body["token"].(string)
	if !ok {
		log.Printf("Bootstrap response missing token for device %d", idx)
		atomic.AddInt64(&h.stats.errors, 1)
		return
	}

	// Enroll the fake agent via gRPC.
	enrollReq := &agentv1.EnrollRequest{
		BootstrapToken: token,
		Hostname:       hostname,
		Os:             "linux",
		Arch:           "amd64",
		AgentVersion:   "0.1.0-synth",
		Interfaces:     []string{fmt.Sprintf("eth0:10.0.%d.%d/24", (idx/256)%256, idx%256)},
	}

	enrollResp, err := h.client.Enroll(ctx, enrollReq)
	if err != nil {
		log.Printf("Failed to enroll device %d: %v", idx, err)
		atomic.AddInt64(&h.stats.errors, 1)
		return
	}

	// Store the enrolled device.
	dev := &Device{
		id:  enrollResp.DeviceId,
		jwt: enrollResp.Jwt,
	}

	// Add to devices slice with sync.
	h.rngMu.Lock()
	h.devices = append(h.devices, dev)
	h.rngMu.Unlock()

	atomic.AddInt64(&h.stats.enrolled, 1)
}

// runLoadPhase — send heartbeats and metrics from all agents.
func (h *Harness) runLoadPhase(ctx context.Context) {
	// Spawn one heartbeat goroutine per device.
	var wg sync.WaitGroup

	// Limit concurrent goroutines using a semaphore.
	sem := make(chan struct{}, *parallel)

	h.rngMu.Lock()
	devices := h.devices
	h.rngMu.Unlock()

	for _, dev := range devices {
		wg.Add(1)
		sem <- struct{}{} // Acquire slot.
		go func(d *Device) {
			defer func() {
				<-sem // Release slot.
				wg.Done()
			}()
			h.runAgent(ctx, d)
		}(dev)
	}

	wg.Wait()
}

// runAgent simulates one agent sending heartbeats and metrics.
func (h *Harness) runAgent(ctx context.Context, dev *Device) {
	// Open a stream for this device.
	stream, err := h.client.Stream(ctx)
	if err != nil {
		log.Printf("Failed to open stream for device %s: %v", dev.id, err)
		atomic.AddInt64(&h.stats.errors, 1)
		return
	}
	defer stream.CloseSend()

	atomic.AddInt64(&h.stats.streamed, 1)

	// Send heartbeats + metrics at regular intervals.
	ticker := time.NewTicker(*heartbeatInt)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Generate metrics.
			metrics := h.collectMetrics()
			if h.rng.Float64() < *alertRate {
				metrics = h.anomalousMetrics()
			}

			// Send heartbeat with attached metrics.
			hb := &agentv1.StreamRequest{
				Payload: &agentv1.StreamRequest_Heartbeat{
					Heartbeat: &agentv1.Heartbeat{
						TimestampMs:   time.Now().UnixMilli(),
						CpuPercent:    metrics.Samples[0].Value,
						MemoryPercent: metrics.Samples[1].Value,
						Metrics:       metrics,
					},
				},
			}

			if err := stream.Send(hb); err != nil {
				return
			}

			// Wait for ack.
			ack, err := stream.Recv()
			if err != nil {
				return
			}

			if ack.GetHeartbeatAck() != nil {
				atomic.AddInt64(&h.stats.heartbeatsAcked, 1)
				atomic.AddInt64(&h.stats.metricsSent, int64(len(metrics.Samples)))
			}
			atomic.AddInt64(&h.stats.heartbeatsSent, 1)
		}
	}
}

// collectMetrics generates normal synthetic metrics.
func (h *Harness) collectMetrics() *agentv1.MetricBatch {
	now := time.Now().UnixMilli()
	h.rngMu.Lock()
	cpu := float64(h.rng.Intn(60) + 10) // 10-69%
	if h.rng.Float64() < 0.05 {
		cpu = float64(h.rng.Intn(30) + 70) // 70-99% spike
	}
	mem := float64(h.rng.Intn(40) + 40)        // 40-79%
	disk := float64(h.rng.Intn(40) + 50)       // 50-89%
	netBytes := float64(h.rng.Int63n(100000000))
	uptime := float64(h.rng.Intn(86400*30) + 3600) // 1h to 30 days
	h.rngMu.Unlock()

	return &agentv1.MetricBatch{
		CollectedAtMs: now,
		Samples: []*agentv1.Metric{
			{Name: "cpu.utilization_percent", Value: cpu, TimestampMs: now},
			{Name: "memory.used_percent", Value: mem, TimestampMs: now},
			{Name: "disk.used_percent", Source: "sda1", Value: disk, TimestampMs: now},
			{Name: "net.bytes_total", Source: "eth0", Value: netBytes, TimestampMs: now},
			{Name: "system.uptime_seconds", Value: uptime, TimestampMs: now},
		},
	}
}

// anomalousMetrics generates metrics designed to trigger baseline alerts.
func (h *Harness) anomalousMetrics() *agentv1.MetricBatch {
	now := time.Now().UnixMilli()
	atomic.AddInt64(&h.stats.alertsFired, 1)
	return &agentv1.MetricBatch{
		CollectedAtMs: now,
		Samples: []*agentv1.Metric{
			{Name: "cpu.utilization_percent", Value: 99.9, TimestampMs: now},
			{Name: "memory.used_percent", Value: 98.5, TimestampMs: now},
			{Name: "disk.used_percent", Source: "sda1", Value: 97.0, TimestampMs: now},
		},
	}
}

// LoadTestReport holds the final results.
type LoadTestReport struct {
	StartTime        time.Time  `json:"start_time"`
	EndTime          time.Time  `json:"end_time"`
	Duration         time.Duration `json:"duration"`
	DurationActual   time.Duration `json:"duration_actual"`
	NumDevices       int         `json:"num_devices"`
	HeartbeatInt     time.Duration `json:"heartbeat_interval"`
	MetricsInt       time.Duration `json:"metrics_interval"`
	AlertRate        float64     `json:"alert_rate"`
	Parallel         int         `json:"parallel"`
	ReportInterval   time.Duration `json:"report_interval"`
	ReportFile       string      `json:"report_file"`
	Enrolled         int64       `json:"enrolled"`
	Streamed         int64       `json:"streamed"`
	HeartbeatsSent   int64       `json:"heartbeats_sent"`
	HeartbeatsAcked  int64       `json:"heartbeats_acked"`
	MetricsSent      int64       `json:"metrics_sent"`
	AlertsFired      int64       `json:"alerts_fired"`
	Errors           int64       `json:"errors"`
	ServerMetrics    *ServerMetrics `json:"server_metrics,omitempty"`
}

// ServerMetrics holds server resource measurements.
type ServerMetrics struct {
	HostCPUUsagePercent  float64 `json:"host_cpu_usage_percent,omitempty"`
	HostMemoryUsedBytes  int64   `json:"host_memory_used_bytes,omitempty"`
	HostMemoryTotalBytes int64   `json:"host_memory_total_bytes,omitempty"`
	NatsQueuedMessages   int64   `json:"nats_queued_messages,omitempty"`
	NatsActiveSubs       int64   `json:"nats_active_subscriptions,omitempty"`
	NatsConnectedClients int64   `json:"nats_connected_clients,omitempty"`
	PgConnectionCount    int64   `json:"postgres_connection_count,omitempty"`
	DeviceCount          int64   `json:"device_count,omitempty"`
	AlertCount           int64   `json:"alert_count,omitempty"`
	OnlineDeviceCount    int64   `json:"online_device_count,omitempty"`
}

func main() {
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Signal handling.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("Interrupt received, shutting down...")
		cancel()
	}()

	// Connect to gRPC server.
	grpcAddr := fmt.Sprintf("%s:%d", *serverHost, *grpcPort)
	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("Failed to connect to gRPC server %s: %v", grpcAddr, err)
	}
	defer conn.Close()

	client := agentv1.NewAgentServiceClient(conn)

	// Create harness and run test.
	h := NewHarness(conn)
	h.client = client

	start := time.Now()
	log.Printf("Starting load test: %d devices for %v", *numDevices, *duration)
	log.Printf("Server: %s (gRPC :%d, HTTP :%d)", *serverHost, *grpcPort, *httpPort)

	// Run load test.
	if err := h.run(ctx); err != nil {
		log.Printf("Load test failed: %v", err)
	}

	// Collect server metrics.
	serverMetrics := collectServerMetrics(*serverHost, *httpPort)

	// Write report.
	report := &LoadTestReport{
		StartTime:      start,
		EndTime:        time.Now(),
		Duration:       *duration,
		DurationActual: time.Since(start),
		NumDevices:     *numDevices,
		HeartbeatInt:   *heartbeatInt,
		MetricsInt:     *metricsInt,
		AlertRate:      *alertRate,
		Parallel:       *parallel,
		ReportInterval: *reportInterval,
		ReportFile:     *outputFile,
		Enrolled:       atomic.LoadInt64(&h.stats.enrolled),
		Streamed:       atomic.LoadInt64(&h.stats.streamed),
		HeartbeatsSent: atomic.LoadInt64(&h.stats.heartbeatsSent),
		HeartbeatsAcked: atomic.LoadInt64(&h.stats.heartbeatsAcked),
		MetricsSent:    atomic.LoadInt64(&h.stats.metricsSent),
		AlertsFired:    atomic.LoadInt64(&h.stats.alertsFired),
		Errors:         atomic.LoadInt64(&h.stats.errors),
		ServerMetrics:  serverMetrics,
	}

	if err := writeReport(report); err != nil {
		log.Printf("Failed to write report: %v", err)
	}
}

func writeReport(r *LoadTestReport) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*outputFile, data, 0644); err != nil {
		return err
	}
	log.Printf("Report written to %s", *outputFile)
	return nil
}

func collectServerMetrics(host string, port int) *ServerMetrics {
	metrics := &ServerMetrics{}
	httpBase := fmt.Sprintf("http://%s:%d", host, port)

	// Query device count.
	if resp, err := http.Get(fmt.Sprintf("%s/api/devices", httpBase)); err == nil {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var devices []struct {
			Online bool `json:"online"`
		}
		if err := json.Unmarshal(body, &devices); err == nil {
			metrics.DeviceCount = int64(len(devices))
			for _, d := range devices {
				if d.Online {
					metrics.OnlineDeviceCount++
				}
			}
		}
	}

	// Query alert count.
	if resp, err := http.Get(fmt.Sprintf("%s/api/alerts?status=open", httpBase)); err == nil {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var alerts []struct{}
		if err := json.Unmarshal(body, &alerts); err == nil {
			metrics.AlertCount = int64(len(alerts))
		}
	}

	// NATS monitoring.
	natsMetrics := map[string]int64{}
	if resp, err := http.Get(fmt.Sprintf("http://%s:8222/varz", host)); err == nil {
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(&natsMetrics); err == nil {
			metrics.NatsActiveSubs = natsMetrics["subscriptions"]
			metrics.NatsConnectedClients = natsMetrics["connections"]
		}
	}

	return metrics
}
