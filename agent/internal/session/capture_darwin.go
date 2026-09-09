//go:build darwin

// macOS capture (gap #1a, phase 1 stub): real screen capture on macOS needs
// ScreenCaptureKit, which requires cgo/objc — incompatible with the
// static-binary constraint (make verify-agent runs go vet per target with
// CGO_ENABLED=0). Phase 1 therefore reports the "unavailable" status frame
// (the viewer shows the honest degraded state); the SCK-backed backend
// lands in phase 2. RMMWAY_SESSION_SOURCE=test still gives the full
// pipeline on any macOS box (and in CI) for e2e purposes.
package session

import "context"

func defaultCapturer() (Capturer, error) {
	return &darwinCapturer{}, nil
}

type darwinCapturer struct{}

func (d *darwinCapturer) Name() string { return "darwin-stub" }

func (d *darwinCapturer) Capture(context.Context) (*Frame, error) {
	return &Frame{Status: "unavailable"}, nil
}

func (d *darwinCapturer) Close() error { return nil }
