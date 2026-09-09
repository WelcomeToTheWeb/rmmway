// Installed software inventory collector.
// Detects installed packages via platform-specific methods:
//   - Linux: dpkg (Debian/Ubuntu), rpm (RHEL/CentOS/Fedora)
//   - Windows: MSI database, registry uninstall keys
//   - macOS: /usr/local/bin listpkg, Homebrew, etc.
package inventory

import (
	"context"
	"os/exec"
	"strings"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// SoftwareInfo describes an installed application.
type SoftwareInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Vendor  string `json:"vendor"`
	InstallDate string `json:"install_date,omitempty"`
	Arch    string `json:"arch,omitempty"`
	Path    string `json:"path,omitempty"`
	Source  string `json:"source"` // "dpkg" | "rpm" | "msi" | "registry" | "homebrew" | "pkg"
}

// CollectSoftware gathers installed software inventory.
func CollectSoftware(ctx context.Context) ([]SoftwareInfo, error) {
	var software []SoftwareInfo

	// Try dpkg first (Debian/Ubuntu)
	if dpkgInfo, err := collectDpkg(ctx); err == nil && len(dpkgInfo) > 0 {
		software = append(software, dpkgInfo...)
	} else if rpmInfo, err := collectRpm(ctx); err == nil && len(rpmInfo) > 0 {
		// Try rpm (RHEL/CentOS/Fedora)
		software = append(software, rpmInfo...)
	}

	// Add any additional sources
	// (Homebrew on macOS, etc.)

	return software, nil
}

// collectDpkg queries installed packages via dpkg -l.
func collectDpkg(ctx context.Context) ([]SoftwareInfo, error) {
	out, err := exec.CommandContext(ctx, "dpkg-query", "-Wf",
		"${Package}\t${Version}\t${Status}\n").Output()
	if err != nil {
		return nil, err
	}

	var packages []SoftwareInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}

		// Only include installed packages
		status := parts[2]
		if !strings.Contains(status, "installed") {
			continue
		}

		packages = append(packages, SoftwareInfo{
			Name:    parts[0],
			Version: parts[1],
			Source:  "dpkg",
		})
	}

	return packages, nil
}

// collectRpm queries installed packages via rpm -qa.
func collectRpm(ctx context.Context) ([]SoftwareInfo, error) {
	out, err := exec.CommandContext(ctx, "rpm", "-qa", "--qf",
		"%{NAME}\t%{VERSION}\n").Output()
	if err != nil {
		return nil, err
	}

	var packages []SoftwareInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}

		packages = append(packages, SoftwareInfo{
			Name:    parts[0],
			Version: parts[1],
			Source:  "rpm",
		})
	}

	return packages, nil
}

// ToProto converts software list to wire format.
func (s SoftwareInfo) ToProto() *agentv1.SoftwareInfo {
	return &agentv1.SoftwareInfo{
		Name:         s.Name,
		Version:      s.Version,
		Vendor:       s.Vendor,
		InstallDate:  s.InstallDate,
		Arch:         s.Arch,
		Path:         s.Path,
		Source:       s.Source,
	}
}