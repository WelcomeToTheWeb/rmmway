// Command mtlscheck is a W3-1 dev-time smoke check (not a permanent binary):
// it enrolls a scratch device over the PLAIN gRPC port, then proves the mTLS
// port (:50052) accepts the issued leaf, rejects a random (non-org) cert, and
// rejects a client with no cert at all.
//
// Usage: go run ./cmd/mtlscheck [plain-host:port] [mtls-host:port]
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/ca"
)

func die(f string, a ...any) {
	fmt.Printf("FAIL: "+f+"\n", a...)
	os.Exit(1)
}

func postJSON(url string, payload []byte, out any, token ...string) error {
	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if len(token) > 0 && token[0] != "" {
		req.Header.Set("Authorization", "Bearer "+token[0])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: status %d: %s", url, resp.StatusCode, b)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func main() {
	plain, mtls := "127.0.0.1:50051", "127.0.0.1:50052"
	if len(os.Args) > 1 {
		plain = os.Args[1]
	}
	if len(os.Args) > 2 {
		mtls = os.Args[2]
	}
	host, _, err := net.SplitHostPort(plain)
	if err != nil {
		host = plain
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// 1. operator login (admin/admin) -> the C1-gated /admin/* routes need
	// the operator JWT before a bootstrap token can be minted.
	var opLogin struct {
		Token string `json:"token"`
	}
	if err := postJSON("http://127.0.0.1:8080/api/login",
		[]byte(`{"username":"admin","password":"admin"}`), &opLogin); err != nil {
		die("login: %v", err)
	}
	if opLogin.Token == "" {
		die("login: no token returned")
	}

	// 2. mint a bootstrap token over HTTP (operator-authed).
	var boot struct {
		BootstrapToken string `json:"bootstrap_token"`
	}
	if err := postJSON("http://127.0.0.1:8080/admin/bootstrap", []byte("{}"), &boot, opLogin.Token); err != nil {
		die("bootstrap: %v", err)
	}

	// 3. enroll over the PLAIN port — the response must carry the mTLS identity.
	conn, err := grpc.NewClient(plain, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		die("dial plain: %v", err)
	}
	defer conn.Close()
	enroll, err := agentv1.NewAgentServiceClient(conn).Enroll(ctx, &agentv1.EnrollRequest{
		BootstrapToken: boot.BootstrapToken,
		Hostname:       "mtlscheck-" + time.Now().Format("150405"),
		Os:             "linux", Arch: "amd64", AgentVersion: "0.0.0-mtlscheck",
	})
	if err != nil {
		die("enroll: %v", err)
	}
	if enroll.LeafCertPem == "" || enroll.LeafKeyPem == "" || enroll.OrgRootCaPem == "" {
		die("enroll response missing mTLS identity (leaf/key/root all required)")
	}
	fmt.Printf("enrolled %s; mTLS identity present (leaf %dB, key %dB, root %dB)\n",
		enroll.DeviceId, len(enroll.LeafCertPem), len(enroll.LeafKeyPem), len(enroll.OrgRootCaPem))

	// 4. valid leaf -> Stream over the mTLS port (server verified vs pinned root).
	kp, err := tls.X509KeyPair([]byte(enroll.LeafCertPem), []byte(enroll.LeafKeyPem))
	if err != nil {
		die("leaf keypair: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(enroll.OrgRootCaPem)) {
		die("bad org root PEM")
	}
	mtlsConn, err := grpc.NewClient(mtls, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{kp},
		RootCAs:      roots,
		ServerName:   host,
	})))
	if err != nil {
		die("dial mTLS: %v", err)
	}
	defer mtlsConn.Close()
	md := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+enroll.Jwt))
	stream, err := agentv1.NewAgentServiceClient(mtlsConn).Stream(md)
	if err != nil {
		die("stream over mTLS (valid leaf): %v", err)
	}
	if err := stream.Send(&agentv1.StreamRequest{
		Payload: &agentv1.StreamRequest_Heartbeat{
			Heartbeat: &agentv1.Heartbeat{TimestampMs: time.Now().UnixMilli(), CpuPercent: 2, MemoryPercent: 2},
		},
	}); err != nil {
		die("heartbeat over mTLS: %v", err)
	}
	ack, err := stream.Recv()
	if err != nil || ack.GetHeartbeatAck() == nil {
		die("ack over mTLS: %v (ack=%v)", err, ack)
	}
	fmt.Println("OK: valid org-issued leaf streamed a live heartbeat+ack over the mTLS port")

	// 5. random cert (different CA) -> rejected at the handshake.
	rogue, err := ca.GenerateRoot()
	if err != nil {
		die("rogue root: %v", err)
	}
	rc, rk, err := rogue.IssueLeaf("dev-rogue", host, time.Hour)
	if err != nil {
		die("rogue leaf: %v", err)
	}
	rkp, err := tls.X509KeyPair(rc, rk)
	if err != nil {
		die("rogue keypair: %v", err)
	}
	rogueConn, err := grpc.NewClient(mtls, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{rkp},
		RootCAs:      roots, // still verify the server with OUR root
		ServerName:   host,
	})))
	if err != nil {
		die("dial rogue: %v", err)
	}
	defer rogueConn.Close()
	if _, err := agentv1.NewAgentServiceClient(rogueConn).Stream(md); err == nil {
		die("a NON-org-issued leaf was ACCEPTED on the mTLS port (DoD violation)")
	}
	if st, ok := status.FromError(err); ok {
		msg := st.Message()
		if len(msg) > 80 {
			msg = msg[:80]
		}
		fmt.Printf("OK: random (non-org) cert rejected — grpc %s: %s\n", st.Code(), msg)
	} else {
		fmt.Printf("OK: random (non-org) cert rejected — %v\n", err)
	}

	// 6. no client cert at all -> rejected too (RequireAndVerifyClientCert).
	noCert, err := grpc.NewClient(mtls, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		RootCAs:    roots,
		ServerName: host,
	})))
	if err != nil {
		die("dial no-cert: %v", err)
	}
	defer noCert.Close()
	if _, err := agentv1.NewAgentServiceClient(noCert).Stream(md); err == nil {
		die("a client with NO cert was ACCEPTED on the mTLS port")
	}
	fmt.Println("OK: no client cert -> rejected (client cert is required, not optional)")
}
