//go:build darwin

package inventory

import (
	"context"
)

// diskSerialsPlatform on macOS: not implemented (returns empty map).
func diskSerialsPlatform(ctx context.Context) map[string]string {
	return make(map[string]string)
}

// detectDiskType on macOS: not implemented (returns "unknown").
func detectDiskType(device string) string {
	return "unknown"
}

// collectDomainMembership on macOS: not implemented (returns local).
func collectDomainMembership(ctx context.Context) []DomainInfo {
	return []DomainInfo{
		{Type: "local", Name: "local", Workgroup: "WORKGROUP"},
	}
}
