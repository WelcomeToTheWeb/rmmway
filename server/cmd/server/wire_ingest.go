package main

import (
	"context"
	"log"
	"net"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/ca"
	"github.com/welcometotheweb/rmmway/server/internal/caps"
	"github.com/welcometotheweb/rmmway/server/internal/flow"
	"github.com/welcometotheweb/rmmway/server/internal/ingest"
	"github.com/welcometotheweb/rmmway/server/internal/sessionrelay"
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// wireIngest builds the gRPC ingest service and the two agent-facing gRPC
// listeners (plain bootstrap port + the W3-1 mTLS channel) and starts
// serving both. Pure move out of main() (wave-0 F2).
//
// ---- gRPC ingest (W1-5) + mTLS agent channel (W3-1) ---------------
func wireIngest(
	version string,
	jwtSecret []byte,
	grpcAddr, grpcMTLSAddr, httpAddr string,
	indexer *store.IndexerHook,
	caMgr *ca.Manager,
	capsIssuer *caps.Issuer,
	logSink store.LogSink,
	flowBus flow.Bus,
	publishEvent func(subject, deviceID, message string, data map[string]any),
	metricsSink store.MetricsSink,
	devicesStore store.DeviceStore,
	sessionRelay *sessionrelay.Registry, // gap #1a: frame relay + file transfer state
) (*ingest.Service, *grpc.Server, *grpc.Server) {
	svc := ingest.NewService(ingest.Config{JWTSecret: jwtSecret, Indexer: indexer, OrgCA: caMgr, Caps: capsIssuer, Logs: logSink,
		OnCommandResult: func(res *agentv1.CommandResult) {
			// W5-2: a FINAL agent command answer becomes a bus event so a
			// waiting flow script node advances (event-driven chain hop).
			if flowBus == nil {
				return
			}
			_ = flowBus.Publish(context.Background(), flow.SubjectCommand, &flow.Event{
				Type:      flow.SubjectCommand,
				CommandID: res.GetCommandId(),
				Status:    res.GetStatus().String(),
				Message:   res.GetError(),
				At:        time.Now().UTC(),
			})
		},
		Sessions: sessionRelay,
		OnDeviceEvent: func(action string, payload map[string]any) {
			// W6-2: inventory events (created / online) onto the bus.
			devID, _ := payload["device_id"].(string)
			publishEvent(flow.SubjectDevice, devID, action+" device", payload)
		},
	}, metricsSink, devicesStore)
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(svc.JWTInterceptor),
	)
	agentv1.RegisterAgentServiceServer(grpcServer, svc)

	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		log.Fatalf("grpc listen %s: %v", grpcAddr, err)
	}
	go func() {
		log.Printf("rmmway-server %s: gRPC agent ingest on %s", version, grpcAddr)
		if err := grpcServer.Serve(lis); err != nil {
			log.Printf("grpc server: %v", err)
		}
	}()

	// W3-1: second gRPC listener, mTLS. Same AgentService, but the TLS
	// layer requires a client leaf signed by the org root before any RPC
	// is processed (a random cert is rejected at the handshake), and the
	// server presents a root-signed cert so the agent verifies us too.
	// RMMWAY_GRPC_MTLS_ADDR=off disables it (plain-listener deployments).
	var mtlsServer *grpc.Server
	if grpcMTLSAddr != "off" && grpcMTLSAddr != "" {
		sans := mtlsSANs(grpcMTLSAddr, grpcAddr, httpAddr)
		log.Printf("grpc mTLS SANs: %s", strings.Join(sans, " "))
		mtlsCfg, err := caMgr.TLSConfig(sans)
		if err != nil {
			log.Fatalf("grpc mTLS: %v", err)
		}
		mtlsServer = grpc.NewServer(
			grpc.Creds(credentials.NewTLS(mtlsCfg)),
			grpc.UnaryInterceptor(svc.JWTInterceptor),
		)
		agentv1.RegisterAgentServiceServer(mtlsServer, svc)
		mtlsLis, err := net.Listen("tcp", grpcMTLSAddr)
		if err != nil {
			log.Fatalf("grpc mTLS listen %s: %v", grpcMTLSAddr, err)
		}
		go func() {
			log.Printf("rmmway-server %s: gRPC mTLS agent channel on %s (client cert required)", version, grpcMTLSAddr)
			if err := mtlsServer.Serve(mtlsLis); err != nil {
				log.Printf("grpc mTLS server: %v", err)
			}
		}()
	}

	return svc, grpcServer, mtlsServer
}
