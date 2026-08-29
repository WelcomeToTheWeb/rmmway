//go:build !windows

package main

import (
	"context"
	"log/slog"

	"github.com/welcometotheweb/rmmway/agent/internal/logship"
)

// isWindowsService is always false off-Windows: the agent runs as an ordinary
// foreground process (the Windows service-control protocol is Windows-only).
func isWindowsService() bool { return false }

// runWindowsService is a fallback that is never reached off-Windows
// (isWindowsService() is false, so runCommand takes the interactive path).
func runWindowsService(cfg agentConfig, log *slog.Logger, jsonl *logship.JSONLHandler, logFile string) error {
	return runAgent(context.Background(), log, cfg, jsonl, logFile)
}
