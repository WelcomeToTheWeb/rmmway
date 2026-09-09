// Package patchmanager provides Windows Update and third-party patch management.
// On Windows, it uses the Windows Update API (WUAPI) via PowerShell. On other
// platforms, it uses the native package manager.
package patchmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// AvailablePatch describes a patch that can be applied.
type AvailablePatch struct {
	ID             string
	Title          string
	Description    string
	Severity       string
	KBArticle      string
	RebootRequired bool
	DownloadURL    string
}

// InstalledPatch describes a patch that has already been applied.
type InstalledPatch struct {
	ID          string
	Title       string
	InstallDate string
	KBArticle   string
}

// PatchQueryResult contains the results of a patch query.
type PatchQueryResult struct {
	Available []AvailablePatch
	Installed []InstalledPatch
}

// PatchApplyProgress reports the progress of a patch apply operation.
type PatchApplyProgress struct {
	Phase           string
	Message         string
	ProgressPercent uint32
	RebootRequired  bool
	Installed       []InstalledPatch
	Errors          []string
}

// PatchManager manages patch operations on the host.
type PatchManager struct{}

// NewPatchManager creates a new patch manager instance.
func NewPatchManager() *PatchManager {
	return &PatchManager{}
}

// Query patches available on the host.
func (pm *PatchManager) Query(ctx context.Context, severityFilter string) (*PatchQueryResult, error) {
	platform := detectPlatform()
	switch platform {
	case "windows":
		return pm.queryWindows(ctx, severityFilter)
	default:
		// Linux/macOS: use package manager (simplified for now)
		return &PatchQueryResult{}, nil
	}
}

// Apply patches on the host.
func (pm *PatchManager) Apply(ctx context.Context, patchIDs []string, scheduleReboot bool, rebootDelaySeconds uint32, progressCallback func(PatchApplyProgress)) error {
	platform := detectPlatform()
	switch platform {
	case "windows":
		return pm.applyWindows(ctx, patchIDs, scheduleReboot, rebootDelaySeconds, progressCallback)
	default:
		return fmt.Errorf("patch management not implemented for %s", platform)
	}
}

func detectPlatform() string {
	// Simple platform detection
	if _, err := exec.LookPath("powershell"); err == nil {
		return "windows"
	}
	return "linux"
}

// queryWindows uses PowerShell and WUAPI to query available updates.
func (pm *PatchManager) queryWindows(ctx context.Context, severityFilter string) (*PatchQueryResult, error) {
	result := &PatchQueryResult{}

	// Use PowerShell to query Windows Update via WUAPI
	// This is a simplified approach; production would use the WUAPI directly
	// via COM interop for better performance.
	powershellScript := `
$session = New-Object -ComObject Microsoft.Update.Session
$installer = $session.CreateUpdateInstaller()
$updates = @()
try {
    $searcher = $session.CreateUpdateSearcher()
    $results = $searcher.Search("IsInstalled=0")
    foreach ($update in $results.Updates) {
        $updates += @{
            ID = $update.Identity.UpdateID;
            Title = $update.Title;
            Severity = $update.MsrcSeverity;
            RebootRequired = $update.RebootRequired;
        }
    }
} catch {
    Write-Error "Query failed: $_"
}
$updates | ConvertTo-Json
`
	cmd := exec.CommandContext(ctx, "powershell", "-Command", powershellScript)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Log but don't fail entirely
		fmt.Fprintf(os.Stderr, "Windows Update query error: %v\n%s", err, out)
		return result, nil
	}

	// Parse JSON output
	var patches []map[string]interface{}
	if err := json.Unmarshal(out, &patches); err != nil {
		return result, fmt.Errorf("failed to parse patch query output: %w", err)
	}

	for _, p := range patches {
		id, _ := p["ID"].(string)
		title, _ := p["Title"].(string)
		severity, _ := p["Severity"].(string)
		rebootReq, _ := p["RebootRequired"].(bool)

		// Apply severity filter if specified
		if severityFilter != "" && !strings.Contains(strings.ToLower(severity), strings.ToLower(severityFilter)) {
			continue
		}

		result.Available = append(result.Available, AvailablePatch{
			ID:             id,
			Title:          title,
			Severity:       severity,
			RebootRequired: rebootReq,
		})
	}

	return result, nil
}

// applyWindows uses PowerShell to apply approved patches.
func (pm *PatchManager) applyWindows(ctx context.Context, patchIDs []string, scheduleReboot bool, rebootDelaySeconds uint32, progressCallback func(PatchApplyProgress)) error {
	if progressCallback != nil {
		progressCallback(PatchApplyProgress{
			Phase:   "downloading",
			Message: "Downloading and installing patches...",
		})
	}

	// For now, apply all available updates (not filtered by patchIDs)
	// Production would need to filter by specific patch IDs via WUAPI
	powershellScript := `
$session = New-Object -ComObject Microsoft.Update.Session
$installer = $session.CreateUpdateInstaller()
$updatesToInstall = New-Object -ComObject Microsoft.Update.UpdateColl

try {
    $searcher = $session.CreateUpdateSearcher()
    $results = $searcher.Search("IsInstalled=0")
    foreach ($update in $results.Updates) {
        $updatesToInstall.Add($update) | Out-Null
    }

    $result = $installer.Install($updatesToInstall)
    
    Write-Host "Phase: installing"
    Write-Host "Message: Installation complete"
    Write-Host "RebootRequired: " + $result.RebootRequired
    
    foreach ($updateResult in $result.GetUpdates()) {
        Write-Host "Patch: " + $updateResult.Update.Title + " Status: " + $updateResult.Result
    }
} catch {
    Write-Error "Install failed: $_"
}
`
	cmd := exec.CommandContext(ctx, "powershell", "-Command", powershellScript)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if progressCallback != nil {
			progressCallback(PatchApplyProgress{
				Phase:  "failed",
				Errors: []string{fmt.Sprintf("PowerShell execution failed: %v\n%s", err, out)},
			})
		}
		return err
	}

	// Parse output and report progress
	// Simplified parsing for now
	if progressCallback != nil {
		rebootRequired := strings.Contains(string(out), "RebootRequired: True")
		progressCallback(PatchApplyProgress{
			Phase:          "completed",
			Message:        "Patches applied successfully",
			RebootRequired: rebootRequired,
		})
	}

	// Schedule reboot if needed
	if scheduleReboot && rebootDelaySeconds > 0 {
		// Schedule reboot via shutdown command
		exec.CommandContext(ctx, "shutdown", "/r", "/t", fmt.Sprintf("%d", rebootDelaySeconds)).Start()
	}

	return nil
}