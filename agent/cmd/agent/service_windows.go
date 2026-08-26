//go:build windows

package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"golang.org/x/sys/windows/svc"

	"github.com/welcometotheweb/rmmway/agent/internal/logship"
)

// serviceName is the Windows service the installer registers. It MUST match the
// $svc used by scripts/install.ps1 (`sc create $svc ...`). Windows service
// names are case-insensitive, but keep them identical to avoid confusion.
const serviceName = "RmmWayAgent"

// isWindowsService reports whether this process was started by the Service
// Control Manager (i.e. it should speak the Windows service-control protocol
// rather than run as an ordinary foreground process).
func isWindowsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// runWindowsService runs the agent under the SCM, blocking until the service
// is told to stop. It must be called from the main goroutine.
func runWindowsService(cfg agentConfig, log *slog.Logger, jsonl *logship.JSONLHandler, logFile string) error {
	return svc.Run(serviceName, &agentService{cfg: cfg, log: log, jsonl: jsonl, logFile: logFile})
}

// agentService adapts runAgent to the Windows service-control protocol. It
// reports SERVICE_START_PENDING -> SERVICE_RUNNING to the SCM and then runs
// the agent, retrying the connect/enroll with a short backoff until it
// succeeds or the service is stopped. Reporting SERVICE_RUNNING (and staying
// alive across a transient connect failure) is what makes `net start` /
// Start-Service succeed and keeps the device from wedging when the server is
// not yet reachable at boot.
type agentService struct {
	cfg     agentConfig
	log     *slog.Logger
	jsonl   *logship.JSONLHandler
	logFile string
}

// Execute implements svc.Handler.
func (s *agentService) Execute(_ []string, c <-chan svc.ChangeRequest, status chan<- svc.Status) (svcSpecificEC bool, exitCode uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending, Accepts: accepts}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{}, 1)
	go func() {
		defer close(done)
		s.runLoop(ctx)
	}()

	status <- svc.Status{State: svc.Running, Accepts: accepts}

	for req := range c {
		switch req.Cmd {
		case svc.Stop, svc.Shutdown:
			cancel()
			status <- svc.Status{State: svc.StopPending, Accepts: 0}
			// Give the uplink / rotator / logship a bounded grace to wind down.
			select {
			case <-done:
			case <-time.After(15 * time.Second):
			}
			status <- svc.Status{State: svc.Stopped}
			return false, 0
		case svc.Interrogate:
			status <- req.CurrentStatus
		case svc.Pause:
			status <- svc.Status{State: svc.Paused, Accepts: svc.AcceptPauseAndContinue}
		case svc.Continue:
			status <- svc.Status{State: svc.Running, Accepts: accepts}
		default:
			cancel()
			status <- svc.Status{State: svc.Stopped}
			// Service-specific, non-zero exit code: the SCM treats it as a
			// failed start/stop worth reporting.
			return true, 1067
		}
	}
	return false, 0
}

// runLoop runs one agent pass; on a non-fatal failure it waits and retries so
// a not-yet-ready server at boot doesn't leave the service down. It stops when
// ctx is canceled (service stop) or a pass completes cleanly.
func (s *agentService) runLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := runAgent(ctx, s.log, s.cfg, s.jsonl, s.logFile); err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return
			}
			s.log.Error("agent pass failed; retrying in 30s", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
		} else {
			return // clean stop
		}
	}
}
