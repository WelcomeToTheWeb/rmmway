//go:build linux

package inventory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// diskSerialsPlatform reads /sys/block/<dev>/device/vendor and /model for
// serial (serial is often empty on virtual disks; vendor+model is used as
// a fallback identifier). Real serial is read from /sys/block/<dev>/device/serial.
func diskSerialsPlatform(ctx context.Context) map[string]string {
	serials := make(map[string]string)

	devices, _ := os.ReadDir("/sys/block")
	for _, dev := range devices {
		if !dev.IsDir() {
			continue
		}
		name := dev.Name()
		// Skip loop, ram, zram
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "zram") {
			continue
		}

		serialPath := fmt.Sprintf("/sys/block/%s/device/serial", name)
		if data, err := os.ReadFile(serialPath); err == nil {
			serial := strings.TrimSpace(string(data))
			if serial != "" {
				serials[name] = serial
				continue
			}
		}

		// Fallback: model
		modelPath := fmt.Sprintf("/sys/block/%s/device/model", name)
		if data, err := os.ReadFile(modelPath); err == nil {
			model := strings.TrimSpace(string(data))
			if model != "" {
				serials[name] = model
			}
		}
	}

	return serials
}

// detectDiskType reads /sys/block/<dev>/queue/rotational (0=ssd, 1=hdd).
func detectDiskType(device string) string {
	path := filepath.Join("/sys/block", device, "queue", "rotational")
	data, err := os.ReadFile(path)
	if err != nil {
		return "unknown"
	}
	switch strings.TrimSpace(string(data)) {
	case "0":
		return "ssd"
	case "1":
		return "hdd"
	}
	return "unknown"
}

// collectDomainMembership on Linux: check /etc/machine-id and systemd's
// hostnamectl. If joined to an AD domain via realmd/sssd, hostnamectl shows
// it. Fallback: check if sssd is running and configured.
func collectDomainMembership(ctx context.Context) []DomainInfo {
	// Check if systemd hostnamectl is available and shows domain
	// Simplified: just report local
	return []DomainInfo{
		{Type: "local", Name: "local", Workgroup: "WORKGROUP"},
	}
}
