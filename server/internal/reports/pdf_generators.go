// pdf_generators.go — PDF variants of all 5 report types.
package reports

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/jung-kurt/gofpdf"
)

// GenerateFleetStatusPDF generates a PDF fleet status report.
func (s *Store) GenerateFleetStatusPDF(ctx context.Context) (io.Reader, error) {
	rows, err := s.db.Query(ctx, `
		SELECT d.id, d.hostname, d.os, d.arch, d.online,
		       d.first_seen, d.last_seen,
		       (SELECT count(*) FROM alerts a
		        WHERE a.device_id = d.id AND a.status = 'open') AS open_alerts
		FROM devices d ORDER BY d.hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Collect all data into memory
	var deviceRows [][]string
	var totalDevices, onlineCount, offlineCount, totalAlerts int

	for rows.Next() {
		var id, hostname, os, arch string
		var online bool
		var firstSeen, lastSeen time.Time
		var openAlerts int
		if err := rows.Scan(&id, &hostname, &os, &arch, &online,
			&firstSeen, &lastSeen, &openAlerts); err != nil {
			return nil, err
		}

		totalDevices++
		if online {
			onlineCount++
		} else {
			offlineCount++
		}
		totalAlerts += openAlerts

		onlineStr := "Online"
		if !online {
			onlineStr = "Offline"
		}
		deviceRows = append(deviceRows, []string{
			hostname,
			fmt.Sprintf("%s/%s", os, arch),
			onlineStr,
			firstSeen.Format("2006-01-02"),
			lastSeen.Format("2006-01-02 15:04"),
			fmt.Sprint(openAlerts),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Summary items
	summaryItems := []string{
		fmt.Sprintf("Total Devices: %d", totalDevices),
		fmt.Sprintf("Online: %d (%.1f%%)", onlineCount, float64(onlineCount)/float64(totalDevices)*100),
		fmt.Sprintf("Offline: %d (%.1f%%)", offlineCount, float64(offlineCount)/float64(totalDevices)*100),
		fmt.Sprintf("Total Open Alerts: %d", totalAlerts),
	}

	headers := []string{"Hostname", "OS/Arch", "Status", "First Seen", "Last Seen", "Open Alerts"}
	return GeneratePDFWithSummary("Fleet Status Report", "", "Fleet Overview", summaryItems, headers, deviceRows)
}

// GenerateDeviceReportPDF generates a PDF device report.
func (s *Store) GenerateDeviceReportPDF(ctx context.Context, deviceID string) (io.Reader, error) {
	var hostname, os, arch string
	var online bool
	var firstSeen, lastSeen time.Time
	var metricCount int

	err := s.db.QueryRow(ctx, `
		SELECT d.hostname, d.os, d.arch, d.online, d.first_seen, d.last_seen,
			   (SELECT count(*) FROM metrics m WHERE m.device_id = d.id) AS metric_count
		FROM devices d WHERE d.id = $1`, deviceID).Scan(
		&hostname, &os, &arch, &online, &firstSeen, &lastSeen, &metricCount)
	if err != nil {
		return nil, fmt.Errorf("query device %s: %w", deviceID, err)
	}

	onlineStr := "Online"
	if !online {
		onlineStr = "Offline"
	}

	summaryItems := []string{
		fmt.Sprintf("Hostname: %s", hostname),
		fmt.Sprintf("Operating System: %s", os),
		fmt.Sprintf("Architecture: %s", arch),
		fmt.Sprintf("Status: %s", onlineStr),
		fmt.Sprintf("First Seen: %s", firstSeen.Format("2006-01-02 15:04:05 MST")),
		fmt.Sprintf("Last Seen: %s", lastSeen.Format("2006-01-02 15:04:05 MST")),
		fmt.Sprintf("Metric Samples: %d", metricCount),
	}

	// Recent alerts
	var alertRows [][]string
	alertRowsQ, err := s.db.Query(ctx, `
		SELECT a.name, a.status, a.score, a.channel, a.first_at, a.last_at
		FROM alerts a WHERE a.device_id = $1 ORDER BY a.last_at DESC LIMIT 20`, deviceID)
	if err != nil {
		return nil, err
	}
	defer alertRowsQ.Close()
	for alertRowsQ.Next() {
		var name, status, channel string
		var score float64
		var firstAt, lastAt time.Time
		if err := alertRowsQ.Scan(&name, &status, &score, &channel, &firstAt, &lastAt); err != nil {
			return nil, err
		}
		alertRows = append(alertRows, []string{
			name, status, fmt.Sprintf("%.1f", score), channel,
			firstAt.Format("2006-01-02 15:04"), lastAt.Format("2006-01-02 15:04"),
		})
	}
	if err := alertRowsQ.Err(); err != nil {
		return nil, err
	}

	pdf := gofpdf.New("P", "mm", "A4", "")
	pdf.SetTitle("Device Report: "+hostname, false)
	pdf.AddPage()

	b := newBasePDFReport("Device Report", hostname, "")
	b.generateHeader(pdf)
	b.writeSummaryBox(pdf, "Device Information", summaryItems)

	if len(alertRows) > 0 {
		pdf.Ln(5)
		b.writeTable(pdf,
			[]string{"Alert Name", "Status", "Score", "Channel", "First Occurred", "Last Occurred"},
			alertRows)
	} else {
		pdf.Ln(5)
		pdf.SetFont("Helvetica", "", 10)
		pdf.SetTextColor(100, 100, 100)
		pdf.CellFormat(0, 8, "No recent alerts for this device.", "", 1, "L", false, 0, "")
	}

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("output PDF: %w", err)
	}
	return &buf, nil
}

// GeneratePatchCompliancePDF generates a PDF patch compliance report.
func (s *Store) GeneratePatchCompliancePDF(ctx context.Context) (io.Reader, error) {
	// Query: count devices by their last successful agent update check
	rows, err := s.db.Query(ctx, `
		SELECT
			d.hostname, d.os, d.arch,
			d.agent_version,
			d.last_seen,
			(SELECT MAX(UPDATE_CHECK_AT) FROM agent_updates au WHERE au.device_id = d.id) AS last_update_check
		FROM devices d ORDER BY d.hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var patchRows [][]string
	var compliant, nonCompliant int
	threshold := time.Now().Add(-30 * 24 * time.Hour) // 30 days

	for rows.Next() {
		var hostname, os, arch, agentVersion string
		var lastSeen time.Time
		var lastUpdateCheck *time.Time
		if err := rows.Scan(&hostname, &os, &arch, &agentVersion, &lastSeen, &lastUpdateCheck); err != nil {
			return nil, err
		}

		status := "Compliant"
		if lastUpdateCheck == nil || lastUpdateCheck.Before(threshold) {
			status = "Non-Compliant"
			nonCompliant++
		} else {
			compliant++
		}

		lastCheckStr := "Never"
		if lastUpdateCheck != nil {
			lastCheckStr = lastUpdateCheck.Format("2006-01-02 15:04")
		}

		patchRows = append(patchRows, []string{
			hostname,
			fmt.Sprintf("%s/%s", os, arch),
			agentVersion,
			status,
			lastCheckStr,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	total := compliant + nonCompliant
	summaryItems := []string{
		fmt.Sprintf("Total Devices: %d", total),
		fmt.Sprintf("Compliant: %d (%.1f%%)", compliant, float64(compliant)/float64(total)*100),
		fmt.Sprintf("Non-Compliant: %d (%.1f%%)", nonCompliant, float64(nonCompliant)/float64(total)*100),
		"Compliance Window: Last 30 Days",
	}

	headers := []string{"Hostname", "OS/Arch", "Agent Version", "Status", "Last Update Check"}
	return GeneratePDFWithSummary("Patch Compliance Report", "", "Compliance Overview", summaryItems, headers, patchRows)
}

// GenerateLicenseCompliancePDF generates a PDF license compliance report.
func (s *Store) GenerateLicenseCompliancePDF(ctx context.Context) (io.Reader, error) {
	// Count devices by client license tier
	var licenseRows [][]string
	var totalDevices int

	rows, err := s.db.Query(ctx, `
		SELECT d.hostname, d.client_id, c.license_tier
		FROM devices d
		LEFT JOIN clients c ON d.client_id = c.id
		ORDER BY d.client_id, d.hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var hostname, clientID, licenseTier string
		if err := rows.Scan(&hostname, &clientID, &licenseTier); err != nil {
			return nil, err
		}
		if clientID == "" {
			clientID = "Unassigned"
		}
		if licenseTier == "" {
			licenseTier = "Unknown"
		}
		totalDevices++
		licenseRows = append(licenseRows, []string{hostname, clientID, licenseTier})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	summaryItems := []string{
		fmt.Sprintf("Total Devices: %d", totalDevices),
	}

	headers := []string{"Hostname", "Client", "License Tier"}
	return GeneratePDFWithSummary("License Compliance Report", "", "License Overview", summaryItems, headers, licenseRows)
}

// GenerateUptimeSLAPDF generates a PDF uptime/SLA report.
func (s *Store) GenerateUptimeSLAPDF(ctx context.Context) (io.Reader, error) {
	// Calculate uptime based on heartbeat frequency
	var slaRows [][]string
	var totalDevices int

	rows, err := s.db.Query(ctx, `
		SELECT d.hostname, d.os, d.arch, d.online,
		       (SELECT count(*) FROM heartbeats h WHERE h.device_id = d.id AND h.timestamp_ms > (now() - interval '30 days')) AS heartbeat_count
		FROM devices d ORDER BY d.hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var hostname, os, arch string
		var online bool
		var heartbeatCount int
		if err := rows.Scan(&hostname, &os, &arch, &online, &heartbeatCount); err != nil {
			return nil, err
		}
		totalDevices++

		status := "Up"
		if !online {
			status = "Down"
		}

		// Expected heartbeats: 1 per 5 minutes = 288 per day = 8640 per 30 days
		expectedHeartbeats := 8640
		uptimePct := 100.0
		if expectedHeartbeats > 0 {
			uptimePct = float64(heartbeatCount) / float64(expectedHeartbeats) * 100
			if uptimePct > 100 {
				uptimePct = 100
			}
		}

		slaRows = append(slaRows, []string{
			hostname,
			fmt.Sprintf("%s/%s", os, arch),
			status,
			fmt.Sprintf("%.2f%%", uptimePct),
			fmt.Sprint(heartbeatCount),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	summaryItems := []string{
		fmt.Sprintf("Total Devices: %d", totalDevices),
		"Period: Last 30 Days",
	}

	headers := []string{"Hostname", "OS/Arch", "Status", "Uptime %", "Heartbeats"}
	return GeneratePDFWithSummary("Uptime/SLA Report", "", "SLA Overview", summaryItems, headers, slaRows)
}
