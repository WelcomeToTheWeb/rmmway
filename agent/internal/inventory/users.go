// User accounts inventory collector.
package inventory

import (
	"context"
	"os/exec"
	"strings"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// UserAccount is an alias for the proto type for convenience.
type UserAccount = agentv1.UserAccount

// CollectUsers gathers local user account information.
func CollectUsers(ctx context.Context) ([]UserAccount, error) {
	switch detectOS() {
	case "windows":
		return collectWindowsUsers(ctx)
	case "darwin":
		return collectDarwinUsers(ctx)
	default:
		return collectLinuxUsers(ctx)
	}
}

// collectLinuxUsers parses /etc/passwd for user account info.
func collectLinuxUsers(ctx context.Context) ([]UserAccount, error) {
	out, err := exec.CommandContext(ctx, "cat", "/etc/passwd").Output()
	if err != nil {
		return nil, err
	}

	var users []UserAccount
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 7)
		if len(parts) < 7 {
			continue
		}

		// Skip system accounts (uid < 1000) for cleanliness, but include them
		// if needed. Include all for now.
		users = append(users, UserAccount{
			Username:   parts[0],
			Uid:        parts[2],
			HomeDir:    parts[5],
			Shell:      parts[6],
			Enabled:    true, // Can't easily determine from /etc/passwd
			AccountType: "user",
		})
	}

	return users, nil
}

// collectWindowsUsers queries Windows user accounts.
func collectWindowsUsers(ctx context.Context) ([]UserAccount, error) {
	// Implemented in windows-specific file
	return []UserAccount{}, nil
}

// collectDarwinUsers queries macOS user accounts.
func collectDarwinUsers(ctx context.Context) ([]UserAccount, error) {
	// Implemented in darwin-specific file
	return []UserAccount{}, nil
}