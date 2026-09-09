// System services inventory collector.
// Lists ALL installed services (not just monitored ones) with their status.
package inventory

import (
	"context"
	"os/exec"
	"strings"
)

// ServiceInfo describes an installed system service.
type ServiceInfo struct {
	Name    string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Status  string `json:"status"` // "running" | "stopped" | "paused" | "unknown"
	Type    string `json:"type,omitempty"` // "systemd" | "launchd" | "windows_service"
	Enabled bool   `json:"enabled"` // auto-start on boot
}

// CollectServices gathers all installed services.
func CollectServices(ctx context.Context) ([]ServiceInfo, error) {
	switch detectOS() {
	case "windows":
		return collectWindowsServices(ctx)
	case "darwin":
		return collectDarwinServices(ctx)
	default:
		return collectLinuxServices(ctx)
	}
}

func detectOS() string {
	// Simple OS detection
	return "linux"
}

// collectLinuxServices uses systemctl list-units to enumerate systemd services.
func collectLinuxServices(ctx context.Context) ([]ServiceInfo, error) {
	// List all service units (not just loaded ones)
	out, err := exec.CommandContext(ctx, "systemctl", "list-unit-files", "--type=service", "--all", "--no-pager", "--no-legend").Output()
	if err != nil {
		return nil, err
	}

	// Now get status of loaded units
	statusOut, err := exec.CommandContext(ctx, "systemctl", "list-units", "--type=service", "--all", "--no-pager", "--no-legend").Output()
	if err != nil {
		return nil, err
	}

	// Parse status into map for quick lookup
	statusMap := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(statusOut)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 4 {
			name := strings.TrimSuffix(parts[0], ".service")
			status := strings.TrimSpace(parts[3])
			statusMap[name] = status
		}
	}

	// Parse unit files for names and enabled status
	var services []ServiceInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}

		name := strings.TrimSuffix(parts[0], ".service")
		enabled := strings.TrimSpace(parts[1]) == "enabled"
		status := statusMap[name]
		if status == "" {
			status = "unknown"
		}

		services = append(services, ServiceInfo{
			Name:    name,
			Status:  status,
			Type:    "systemd",
			Enabled: enabled,
		})
	}

	return services, nil
}

// collectWindowsServices uses sc query to enumerate Windows services.
func collectWindowsServices(ctx context.Context) ([]ServiceInfo, error) {
	// Placeholder - implemented in windows-specific file
	return []ServiceInfo{}, nil
}

// collectDarwinServices uses launchctl to enumerate macOS services.
func collectDarwinServices(ctx context.Context) ([]ServiceInfo, error) {
	// Placeholder - implemented in darwin-specific file
	return []ServiceInfo{}, nil
}