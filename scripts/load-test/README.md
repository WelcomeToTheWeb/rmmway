# RMMWay Load Test Harness

Simulates N synthetic agents connecting to a running RMMWay server and
sending heartbeats, metric batches, and occasional alert-triggering anomalies.

## Usage

```sh
# Build
go build -o load-test .

# Run against a local dev stack
./load-test -server localhost -grpc-port 50051 -http-port 8080 -n 5000 -duration 15m

# Common flags
./load-test \
  -n 5000 \                # number of synthetic devices
  -duration 15m \          # how long to run
  -heartbeat 60s \         # heartbeat interval
  -metrics 60s \           # metric batch interval
  -alert-rate 0.01 \       # fraction of devices that generate anomalous metrics
  -parallel 100 \          # concurrent connections to establish per phase
  -report-interval 30s     # how often to print progress
```

## What it measures

- Connection success rate and latency
- Heartbeat round-trip latency
- Metric ingestion throughput
- Server CPU / memory (via /proc on Linux host)
- NATS queue depth (via NATS monitoring API)
- TimescaleDB query latency
- Alert generation rate
- Aggregate results written to `load-test-report.json`

## Methodology

Each synthetic agent:

1. Obtains a bootstrap enrollment token from the server's HTTP API
2. Enrolls via the gRPC `Enroll` RPC
3. Opens a long-lived gRPC `Stream` connection
4. Sends heartbeats with attached metric batches at the configured interval
5. Randomly generates anomalous metric values to trigger baseline alerts
6. Tracks and reports connection health and latency

The harness runs in phases:

1. **Enrollment phase**: All agents are enrolled
2. **Stream phase**: All agents open their gRPC streams
3. **Load phase**: Agents send heartbeats and metrics for the configured duration
4. **Teardown phase**: Connections are closed, final metrics are collected
