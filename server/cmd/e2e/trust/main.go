// Command trust is the W4-4 milestone harness: "provable trust" (closes
// Block 2). It runs the exact two things a SKEPTIC — an external party who
// trusts the RMMWay builders with NOTHING — must be able to do:
//
// PART A — verify a signed release + read the SBOM
//
//	Builder side (the party we do NOT trust) cross-builds the agent, scans a
//	CycloneDX SBOM with the pinned syft, mints a minisign "release key", and
//	signs the binary + SBOM + SHA256SUMS with tools/signer (the real signer
//	CI uses).
//	Skeptic side (a fresh trust domain holding ONLY the public key) then,
//	with an independent implementation (go-minisign, interop-proven against
//	the reference minisign(1) in W3-4):
//	  A1. verifies the SHA256SUMS signature + every listed checksum;
//	  A2. verifies the binary + SBOM signatures against the public key;
//	  A3. TAMPER: flips one byte of the binary -> the signature is REJECTED
//	      (the check has teeth);
//	  A4. WRONG KEY: verifies against a different public key -> REJECTED;
//	  A5. reads the SBOM: CycloneDX 1.7, the component's sha256 == the real
//	      binary's sha256, and it lists the agent's actual Go dependencies.
//
// PART B — export a client and confirm the data is theirs
//
//	A client owner (also a skeptic about the server) enrolls one device,
//	feeds it known samples, then — through the REAL operator HTTP surface —
//	does a one-click export and confirms the bundle is THEIRS:
//	  B1. the bundle verifies against its OWN manifest (self-describing);
//	  B2. device.json identity matches the client they own (id/hostname/os);
//	  B3. the Parquet contains EXACTLY the samples they fed (count + spot
//	      values + labels + time range), re-read by a standard reader;
//	  B4. TAMPER: one flipped byte in metrics.parquet -> Verify REJECTS.
//
// It is self-contained and self-tearing-down: a temp dir for the release, a
// scratch Timescale DB for the export, both cleaned up on exit.
//
// Usage: cd server && go run ./cmd/e2e/trust
//
//	Part A needs: go (builds the agent + signer) and syft (auto-installed
//	into bin/ via scripts/install-syft.sh if absent).
//	Part B needs: RMMWAY_PG_DSN pointing at a Timescale-capable Postgres
//	where the user can CREATE DATABASE
//	(locally: RMMWAY_PG_DSN=postgres://postgres@localhost:5432/postgres).
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jedisct1/go-minisign"

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

// run runs a command, returning combined output; on error it dies with the
// output for diagnosis.
func run(dir, name string, env []string, args ...string) string {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		die("cmd %s %v (dir=%s): %v\n%s", name, args, dir, err, out.String())
	}
	return out.String()
}

func sha256File(p string) string {
	f, err := os.Open(p)
	if err != nil {
		die("open %s: %v", p, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		die("hash %s: %v", p, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func copyFile(src, dst string, mode os.FileMode) {
	b, err := os.ReadFile(src)
	if err != nil {
		die("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, b, mode); err != nil {
		die("write %s: %v", dst, err)
	}
}

// repoRoot walks up from cwd to the directory containing agent/ and
// tools/signer/.
func repoRoot() string {
	d, err := os.Getwd()
	if err != nil {
		die("cwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, e1 := os.Stat(filepath.Join(d, "agent", "go.mod")); e1 == nil {
			if _, e2 := os.Stat(filepath.Join(d, "tools", "signer", "go.mod")); e2 == nil {
				return d
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	cwd, _ := os.Getwd()
	die("could not locate the repo root from %s", cwd)
	return ""
}

// verifySigned is the SKEPTIC's check: does <file> carry a minisign
// signature (at <file>.minisig) that validates under the public key in
// pubFile? Returns the untrusted comment stamped into the signature.
func verifySigned(pubFile, file string) (comment string, ok bool, err error) {
	pk, err := minisign.NewPublicKeyFromFile(pubFile)
	if err != nil {
		return "", false, fmt.Errorf("public key: %w", err)
	}
	sig, err := minisign.NewSignatureFromFile(file + ".minisig")
	if err != nil {
		return "", false, fmt.Errorf("signature: %w", err)
	}
	ok, err = pk.VerifyFromFile(file, sig)
	if err != nil {
		return "", false, err
	}
	return sig.UntrustedComment, ok, nil
}

func main() {
	root := repoRoot()
	work, err := os.MkdirTemp("", "rmmway-trust-e2e-")
	if err != nil {
		die("temp: %v", err)
	}
	defer os.RemoveAll(work)

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()

	pass := "e2e-trust-pass"
	goosArch := runtime.GOOS + "-" + runtime.GOARCH
	asset := "rmmway-agent-" + goosArch
	agentDir := filepath.Join(root, "agent")
	signerDir := filepath.Join(root, "tools", "signer")

	fmt.Println("================================================================")
	fmt.Println(" W4-4 MILESTONE: \"provable trust\" — the skeptic story")
	fmt.Println("================================================================")

	// ================================================================== //
	//  PART A — verify a signed release + read the SBOM                   //
	// ================================================================== //
	fmt.Println("\n----------------------------------------------------------------")
	fmt.Println(" PART A: a skeptic verifies a signed release + reads the SBOM")
	fmt.Println("----------------------------------------------------------------")

	step("A0. builder: cross-build the real agent binary + the signer")
	relDir := filepath.Join(work, "release")
	_ = os.MkdirAll(relDir, 0o755)
	binPath := filepath.Join(relDir, asset)
	run(agentDir, "go", nil, "build", "-trimpath",
		"-ldflags", "-s -w -X main.version=0.9.0",
		"-o", binPath, "./cmd/agent")
	info("built %s (%d bytes)", asset, fileSize(binPath))

	// syft (pinned, auto-installed if absent) scans the SBOM.
	syft := filepath.Join(root, "bin", "syft")
	if _, err := os.Stat(syft); err != nil {
		info("syft not at bin/syft — installing (pinned) via scripts/install-syft.sh")
		run(root, "bash", nil, "scripts/install-syft.sh")
	}
	sbom := filepath.Join(relDir, asset+".cdx.json")
	step("A1. builder: scan the SBOM with the pinned syft (CycloneDX)")
	run(root, syft, nil, "file:"+binPath, "-o", "cyclonedx-json="+sbom, "-q")
	info("scanned %s -> %s", asset, filepath.Base(sbom))

	// Builder mints a throwaway minisign "release key" and signs everything
	// with the real tools/signer (the exact signer CI uses).
	step("A2. builder: mint the release keypair + sign (binary, SBOM, SHA256SUMS)")
	signer := filepath.Join(work, "rmmway-signer")
	run(signerDir, "go", nil, "build", "-o", signer, ".")
	keys := filepath.Join(work, "keys")
	run(work, signer, []string{"MINISIGN_PASS=" + pass},
		"keygen", "-dir", keys, "-force", "-pass", pass)
	pubKey := filepath.Join(keys, "minisign.pub")
	secKey := filepath.Join(keys, "minisign.key")

	// SHA256SUMS (relative to the release dir) — the checksum manifest.
	sums := filepath.Join(relDir, "SHA256SUMS")
	{
		var b bytes.Buffer
		for _, f := range []string{asset, asset + ".cdx.json"} {
			fmt.Fprintf(&b, "%s  %s\n", sha256File(filepath.Join(relDir, f)), f)
		}
		if err := os.WriteFile(sums, b.Bytes(), 0o644); err != nil {
			die("write SHA256SUMS: %v", err)
		}
	}
	// Sign the binary, SBOM and the SHA256SUMS manifest itself.
	run(work, signer, []string{"MINISIGN_PASS=" + pass}, "sign", "-k", secKey,
		"-pass", pass, "-c", "rmmway release v0.9.0",
		binPath, sbom, sums)
	info("signed %s, %s.cdx.json, SHA256SUMS with the release key", asset, asset)

	// A second, DIFFERENT keypair — for the wrong-key rejection test.
	keys2 := filepath.Join(work, "keys-other")
	run(work, signer, []string{"MINISIGN_PASS=" + pass},
		"keygen", "-dir", keys2, "-force", "-pass", pass)
	pubKeyOther := filepath.Join(keys2, "minisign.pub")

	// The SKEPTIC's trust domain: a fresh dir holding only what a downloader
	// gets — the artifacts, their signatures, the manifest — plus the public
	// key (delivered out-of-band). The secret key never enters this dir.
	step("A3. skeptic: assemble the download (artifacts + sigs + manifest + public key ONLY)")
	skep := filepath.Join(work, "skeptic")
	_ = os.MkdirAll(skep, 0o755)
	for _, f := range []string{asset, asset + ".cdx.json", asset + ".minisig",
		asset + ".cdx.json.minisig", "SHA256SUMS", "SHA256SUMS.minisig"} {
		copyFile(filepath.Join(relDir, f), filepath.Join(skep, f), 0o644)
	}
	copyFile(pubKey, filepath.Join(skep, "minisign.pub"), 0o644)
	skepPub := filepath.Join(skep, "minisign.pub")
	skepBin := filepath.Join(skep, asset)
	skepSBOM := filepath.Join(skep, asset+".cdx.json")
	skepSums := filepath.Join(skep, "SHA256SUMS")
	info("skeptic holds: %s, its .minisig, the SBOM + .minisig, SHA256SUMS + .minisig, minisign.pub", asset)

	// A1: the SHA256SUMS manifest is itself signed, and every checksum holds.
	step("A4. skeptic: verify SHA256SUMS (signature + every listed checksum)")
	if _, ok, err := verifySigned(skepPub, skepSums); err != nil || !ok {
		die("SHA256SUMS signature invalid under the release public key: ok=%v err=%v", ok, err)
	}
	raw, _ := os.ReadFile(skepSums)
	sumLines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for _, line := range sumLines {
		fields := strings.Fields(line)
		check(len(fields) == 2, "malformed SHA256SUMS line: %q", line)
		want, name := fields[0], fields[1]
		got := sha256File(filepath.Join(skep, name))
		check(got == want, "sha256 mismatch for %s: got %s want %s", name, got, want)
		info("sha256 OK  %s", name)
	}

	// A2: the binary + SBOM signatures validate under the public key.
	step("A5. skeptic: verify the binary + SBOM signatures against the public key")
	if c, ok, err := verifySigned(skepPub, skepBin); err != nil || !ok {
		die("binary signature invalid: ok=%v err=%v", ok, err)
	} else {
		info("minisign OK  %s  (sig comment: %q)", asset, c)
	}
	if c, ok, err := verifySigned(skepPub, skepSBOM); err != nil || !ok {
		die("SBOM signature invalid: ok=%v err=%v", ok, err)
	} else {
		info("minisign OK  %s.cdx.json  (sig comment: %q)", asset, c)
	}

	// A3: TAMPER — flip one byte of the binary; the signature must reject.
	step("A6. skeptic: TAMPER (flip one byte) -> the signature is REJECTED")
	tampered := filepath.Join(skep, asset+".tampered")
	b, _ := os.ReadFile(skepBin)
	b[len(b)/2] ^= 0xff
	_ = os.WriteFile(tampered, b, 0o755)
	_ = os.WriteFile(tampered+".minisig", mustRead(skepBin+".minisig"), 0o644)
	c, ok, err := verifySigned(skepPub, tampered)
	if err == nil && ok {
		die("tampered binary VERIFIED (comment %q) — the signature has no teeth", c)
	}
	info("tampered binary REJECTED (ok=%v err=%v) — a flipped byte is caught", ok, err)

	// A4: WRONG KEY — a different public key must also reject.
	step("A7. skeptic: WRONG KEY (different public key) -> REJECTED")
	c2, ok2, err2 := verifySigned(pubKeyOther, skepBin)
	if err2 == nil && ok2 {
		die("binary verified under a DIFFERENT key (comment %q) — key not bound", c2)
	}
	info("wrong-key verification REJECTED (ok=%v err=%v) — the check is bound to THE key", ok2, err2)

	// A5: read the SBOM and tie it to the real binary.
	step("A8. skeptic: read the SBOM (CycloneDX) + tie it to the binary + its deps")
	bom, err := readBOM(skepSBOM)
	check(err == nil, "parse SBOM: %v", err)
	check(bom.BOMFormat == "CycloneDX", "bomFormat = %q, want CycloneDX", bom.BOMFormat)
	check(bom.SpecVersion != "", "specVersion empty")
	info("SBOM: bomFormat=CycloneDX specVersion=%s", bom.SpecVersion)
	check(bom.Metadata.Component.Name == asset,
		"SBOM component name = %q, want %q", bom.Metadata.Component.Name, asset)
	wantHash := "sha256:" + sha256File(skepBin)
	check(bom.Metadata.Component.Version == wantHash,
		"SBOM component sha = %q, want %q (must equal the REAL binary)",
		bom.Metadata.Component.Version, wantHash)
	info("SBOM component %q sha256 == the real binary's sha256 (%s…)", asset, wantHash[:18])
	check(len(bom.Components) >= 10, "SBOM lists only %d components, want the agent's dep tree", len(bom.Components))
	// The SBOM must actually reflect the agent's real dependencies — spot the
	// ones the agent's go.mod declares.
	depNames := map[string]bool{}
	for _, c := range bom.Components {
		depNames[c.Name] = true
	}
	found := 0
	for _, want := range []string{"github.com/jedisct1/go-minisign",
		"github.com/shirou/gopsutil/v4", "google.golang.org/grpc"} {
		if depNames[want] {
			found++
		}
	}
	check(found == 3, "SBOM is missing known agent deps (found %d/3)", found)
	info("SBOM lists %d components incl. the agent's real deps (go-minisign, gopsutil, grpc)",
		len(bom.Components))

	// ================================================================== //
	//  PART B — export a client and confirm the data is theirs            //
	// ================================================================== //
	fmt.Println("\n----------------------------------------------------------------")
	fmt.Println(" PART B: a client owner exports a client + confirms it's theirs")
	fmt.Println("----------------------------------------------------------------")

	dsn := os.Getenv("RMMWAY_PG_DSN")
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres"
	}
	base = time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	total := totalSamples()
	ps := perSeries()

	step("B1. enroll one device + feed it known samples (the client's ground truth)")
	admin, pool, dbName, cleanup := scratchDB(ctx, dsn)
	defer cleanup()
	info("scratch db: %s", dbName)

	devices := store.NewPostgresDevices(pool)
	const devID = "dev_trust_e2e"
	const hostname = "fileserver-trust"
	if err := devices.Register(ctx, devID, hostname, runtime.GOOS, runtime.GOARCH,
		"0.9.0", []string{"10.1.2.3"}, 30, 30); err != nil {
		die("register: %v", err)
	}
	insertFixture(ctx, pool, devID, ps)
	if _, err := pool.Exec(ctx, `CALL refresh_continuous_aggregate('metrics_1m', NULL, NULL)`); err != nil {
		die("refresh CA: %v", err)
	}
	insertAlerts(ctx, pool, devID)
	info("device %s (%s): %d samples (%d series x %d) + rollups + 3 alerts",
		devID, hostname, total, nSeries, ps)

	step("B2. operator: one-click export through the REAL HTTP surface")
	apiSrv := httpapi.New(httpapi.Config{
		Devices:       devices,
		JWTSecret:     []byte("e2e-trust-secret"),
		AdminUser:     "admin",
		AdminPassword: "e2e-pass",
		Export: export.New(export.Config{
			Devices: devices,
			Metrics: export.NewPostgresMetrics(pool),
			Rollups: export.NewPostgresRollups(pool),
			Alerts:  export.NewPostgresAlerts(pool),
			Version: "rmmway-server/e2e-trust",
		}),
	})
	srvURL := serveAPI(apiSrv)
	defer srvURL.Close()
	token := login(ctx, srvURL.URL)
	// gates: no auth -> 401; authed unknown device -> 404 (C1: the /admin
	// mirror is auth-gated now, so bare no-token is a 401 too).
	resp, err := http.Get(srvURL.URL + "/api/devices/" + devID + "/export")
	check(err == nil && resp.StatusCode == http.StatusUnauthorized,
		"unauthed = %v/%d, want 401", err, resp.StatusCode)
	resp.Body.Close()
	resp, err = http.Get(srvURL.URL + "/admin/devices/nope/export")
	check(err == nil && resp.StatusCode == http.StatusUnauthorized,
		"admin export no token = %v/%d, want 401 (auth-gated)", err, resp.StatusCode)
	resp.Body.Close()
	resp, err = httpGetBearer(srvURL.URL+"/admin/devices/nope/export", token)
	check(err == nil && resp.StatusCode == http.StatusNotFound,
		"unknown device (authed /admin mirror) = %v/%d, want 404", err, resp.StatusCode)
	resp.Body.Close()

	resp, err = httpGetBearer(srvURL.URL+"/api/devices/"+devID+"/export", token)
	check(err == nil && resp.StatusCode == http.StatusOK, "export = %v/%d, want 200", err, resp.StatusCode)
	check(resp.Header.Get("Content-Type") == "application/zip",
		"content-type = %q, want application/zip", resp.Header.Get("Content-Type"))
	bundle, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	check(err == nil, "read bundle: %v", err)
	info("exported bundle: %d bytes (login + ONE GET, application/zip)", len(bundle))

	step("B3. confirm it's THEIRS: verify against its OWN manifest")
	mf, err := export.Verify(bytes.NewReader(bundle), int64(len(bundle)))
	check(err == nil, "Verify: %v", err)
	check(mf.Format == export.FormatName, "format = %s", mf.Format)
	rows := map[string]int64{}
	for _, f := range mf.Files {
		rows[f.Name] = f.Rows
	}
	check(rows[export.MetricsName] == int64(total),
		"metrics rows = %d, want EXACTLY %d (every sample they fed)", rows[export.MetricsName], total)
	check(rows[export.AlertsName] == 3, "alert rows = %d, want 3", rows[export.AlertsName])
	info("self-describing Verify OK: %d files, metrics=%d rollups=%d alerts=3 (all hashes/rows check out)",
		len(mf.Files), rows[export.MetricsName], rows[export.RollupsName])

	// Identity: device.json must describe the client they own.
	step("B4. confirm IDENTITY: device.json matches the client they own")
	dev, err := readDeviceFile(bytes.NewReader(bundle), int64(len(bundle)))
	check(err == nil, "read device.json: %v", err)
	check(dev.Device.ID == devID, "device id = %q, want %q", dev.Device.ID, devID)
	check(dev.Device.Hostname == hostname, "hostname = %q, want %q", dev.Device.Hostname, hostname)
	check(dev.Device.OS == runtime.GOOS && dev.Device.Arch == runtime.GOARCH,
		"os/arch = %s/%s, want %s/%s", dev.Device.OS, dev.Device.Arch, runtime.GOOS, runtime.GOARCH)
	check(len(dev.Device.Interfaces) == 1 && dev.Device.Interfaces[0] == "10.1.2.3",
		"interfaces = %v, want [10.1.2.3]", dev.Device.Interfaces)
	info("identity confirmed: %s / %s / %s/%s / 10.1.2.3 — this is their client",
		dev.Device.ID, dev.Device.Hostname, dev.Device.OS, dev.Device.Arch)

	// Content: the Parquet holds exactly the samples they fed, re-read by a
	// standard reader.
	step("B5. confirm CONTENT: the Parquet is exactly the samples they fed")
	mrows, err := export.ReadMetrics(bytes.NewReader(bundle), int64(len(bundle)))
	check(err == nil, "ReadMetrics: %v", err)
	check(len(mrows) == total, "re-read rows = %d, want %d", len(mrows), total)
	mismatch := 0
	var spot *export.MetricRow
	for i := range mrows {
		r := &mrows[i]
		if r.TS.UnixMilli() != r.TimestampMs {
			mismatch++
		}
		if r.Name == "cpu.utilization_percent" && r.Source == "" &&
			r.TimestampMs == base.Add(interval*7).UnixMilli() {
			spot = r
		}
	}
	check(mismatch == 0, "ts/timestamp_ms mismatches = %d, want 0", mismatch)
	check(spot != nil, "spot sample missing")
	check(spot.Value == cpuValue(7), "spot cpu value = %v, want %v (what they fed)",
		spot.Value, cpuValue(7))
	check(spot.Labels == `{"host":"`+hostname+`"}`, "spot labels = %q", spot.Labels)
	info("standard reader re-read %d rows; ts == timestamp_ms on every row; spot sample matches the value they fed", len(mrows))

	// Range: the time window is exactly what they fed.
	var minTS, maxTS time.Time
	for i := range mrows {
		if mrows[i].TS.Before(minTS) || minTS.IsZero() {
			minTS = mrows[i].TS
		}
		if mrows[i].TS.After(maxTS) {
			maxTS = mrows[i].TS
		}
	}
	check(minTS.Equal(base) && maxTS.Equal(base.Add(time.Duration(ps-1)*interval)),
		"range = [%s, %s], want [%s, %s]",
		minTS, maxTS, base, base.Add(time.Duration(ps-1)*interval))
	info("time range = [%s, %s] — exactly the window they fed",
		minTS.Format("01/02 15:04"), maxTS.Format("01/02 15:04"))

	// TAMPER: flip one byte in metrics.parquet -> Verify rejects.
	step("B6. TAMPER: flip one byte in metrics.parquet -> Verify REJECTS")
	tbundle := tamperZipEntry(ctx, bundle, export.MetricsName)
	if _, err := export.Verify(bytes.NewReader(tbundle), int64(len(tbundle))); err == nil {
		die("Verify accepted a tampered bundle — the data can be silently changed")
	} else {
		info("tampered bundle REJECTED: %v", err)
	}

	_ = admin
	step("PASS")
	fmt.Println("================================================================")
	fmt.Println(" W4-4 DoD MET — a skeptic can:")
	fmt.Println("   (a) verify a signed release + read the SBOM  (Parts A4-A8)")
	fmt.Println("   (b) export a client and confirm it's theirs   (Parts B3-B6)")
	fmt.Println(" Closes Block 2 (Trust & Supply Chain).")
	fmt.Println("================================================================")
}

// -------------------------------------------------------------------------- //
//  Part A: SBOM reading                                                       //
// -------------------------------------------------------------------------- //

type cdxComponent struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

type cdxBOM struct {
	BOMFormat   string `json:"bomFormat"`
	SpecVersion string `json:"specVersion"`
	Metadata    struct {
		Component cdxComponent `json:"component"`
	} `json:"metadata"`
	Components []cdxComponent `json:"components"`
}

func readBOM(path string) (*cdxBOM, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var bom cdxBOM
	if err := json.Unmarshal(b, &bom); err != nil {
		return nil, err
	}
	return &bom, nil
}

func mustRead(p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		die("read %s: %v", p, err)
	}
	return b
}

func fileSize(p string) int64 {
	s, err := os.Stat(p)
	if err != nil {
		die("stat %s: %v", p, err)
	}
	return s.Size()
}

// -------------------------------------------------------------------------- //
//  Part B: fixture + HTTP + device.json                                       //
// -------------------------------------------------------------------------- //

func cpuValue(i int) float64  { return 25 + 10*float64(i%100)/100*10 }
func diskValue(i int) float64 { return 62 + float64(i%200)/10 }
func netValue(i int) float64  { return 1000 + 500*float64(i%60)/60 }

// fixture geometry (all derived from base).
const (
	interval = 30 * time.Second
	days     = 1
	nSeries  = 3
)

var base time.Time // set in main

func perSeries() int { return int(time.Duration(days*24*time.Hour) / interval) }
func totalSamples() int {
	return perSeries() * nSeries
}

func insertFixture(ctx context.Context, pool *pgxpool.Pool, devID string, perSeries int) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		die("fixture begin: %v", err)
	}
	for i := 0; i < perSeries; i++ {
		ts := base.Add(time.Duration(i) * interval)
		ms := ts.UnixMilli()
		stmts := []struct {
			name, source string
			value        float64
			labels       string
		}{
			{"cpu.utilization_percent", "", cpuValue(i), `{"host":"fileserver-trust"}`},
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

func scratchDB(ctx context.Context, dsn string) (admin, pool *pgxpool.Pool, name string, cleanup func()) {
	u, err := url.Parse(dsn)
	if err != nil {
		die("parse dsn: %v", err)
	}
	admin, err = pgxpool.New(ctx, u.String())
	if err != nil {
		die("admin pool: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		die("postgres not reachable: %v (try RMMWAY_PG_DSN=postgres://postgres@localhost:5432/postgres)", err)
	}
	name = "rmmway_trust_e2e_" + time.Now().Format("20060102150405")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		die("create scratch db: %v (does the user have CREATEDB?)", err)
	}
	step("migrate scratch db " + name)
	u.Path = "/" + name
	pool, err = pgxpool.New(ctx, u.String())
	if err != nil {
		die("scratch pool: %v", err)
	}
	migN, migErr := store.Migrate(ctx, pool, "migrations")
	if migErr != nil {
		die("migrate: %v (n=%d)", migErr, migN)
	} else if migN < 5 {
		die("expected >=5 migrations, got %d", migN)
	}
	info("%d migrations applied to scratch db %s", migN, name)
	cleanup = func() {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()
		_, _ = admin.Exec(ctx2, `DROP DATABASE IF EXISTS `+name)
		admin.Close()
	}
	return
}

// deviceFile mirrors export.DeviceFile (the device.json payload).
type deviceFile struct {
	Schema string `json:"schema"`
	Device struct {
		ID         string   `json:"id"`
		Hostname   string   `json:"hostname"`
		OS         string   `json:"os"`
		Arch       string   `json:"arch"`
		Interfaces []string `json:"interfaces"`
	} `json:"device"`
}

func readDeviceFile(r io.ReaderAt, size int64) (*deviceFile, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if f.Name != export.DeviceName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
		var d deviceFile
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, err
		}
		return &d, nil
	}
	return nil, fmt.Errorf("device.json not in bundle")
}

// tamperZipEntry returns the bundle with one byte flipped inside `name`.
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

// -------------------------------------------------------------------------- //
//  Part B: HTTP helpers                                                       //
// -------------------------------------------------------------------------- //

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
	return &apiServer{URL: "http://" + lis.Addr().String(), stop: func() { _ = hs.Close() }}
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
