package collectors

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// defaultCertDirs is scanned when RMMWAY_CERT_DIRS is unset.
const defaultCertDirs = "/etc/ssl/certs"

// defaultCertScanCap bounds how many files one heartbeat inspects across
// all directories (a typo'd dir with a huge tree must not stall the push).
const defaultCertScanCap = 200

// maxCertScanCap caps RMMWAY_CERT_SCAN_CAP.
const maxCertScanCap = 1000

// parseCertDirs splits a comma-separated RMMWAY_CERT_DIRS value.
func parseCertDirs(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{defaultCertDirs}
	}
	parts := strings.Split(raw, ",")
	dirs := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			dirs = append(dirs, p)
		}
	}
	if len(dirs) == 0 {
		return []string{defaultCertDirs}
	}
	return dirs
}

// parseCertScanCap reads RMMWAY_CERT_SCAN_CAP (invalid values fall back to
// the default rather than erroring at agent startup).
func parseCertScanCap(raw string) int {
	if strings.TrimSpace(raw) == "" {
		return defaultCertScanCap
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 {
		return defaultCertScanCap
	}
	if n > maxCertScanCap {
		return maxCertScanCap
	}
	return n
}

// parseCertFile returns the first X.509 certificate in a PEM file, or
// nil when the file holds no certificate (non-PEM, private keys, CRLs).
func parseCertFile(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for len(data) > 0 {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return nil, nil
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		return cert, nil
	}
	return nil, nil
}

// emitCerts appends the cert.days_to_expiry family (source = file path).
// Directories that do not exist or are unreadable are skipped silently —
// the default dir is absent on Windows and macOS, where the family simply
// ships nothing unless the operator points RMMWAY_CERT_DIRS at a store.
// Non-certificate files are skipped silently. A parse failure on one file
// is a partial error but never aborts the scan.
func (c *defaultCollector) emitCerts(add func(name, source string, value float64)) error {
	now := c.now()
	dirs := c.certDirs
	cap := c.certCap
	if cap <= 0 {
		cap = defaultCertScanCap
	}
	inspected := 0
	var firstErr error
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // absent/unreadable dir: skip
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			names = append(names, e.Name())
		}
		sort.Strings(names) // deterministic scan order
		for _, name := range names {
			if inspected >= cap {
				break
			}
			inspected++
			path := filepath.Join(dir, name)
			cert, err := parseCertFile(path)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if cert == nil {
				continue
			}
			days := cert.NotAfter.Sub(now).Hours() / 24
			add("cert.days_to_expiry", path, days)
		}
		if inspected >= cap {
			break
		}
	}
	return firstErr
}
