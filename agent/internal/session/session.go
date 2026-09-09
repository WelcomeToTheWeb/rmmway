// Package session is the agent side of gap #1a (remote session, phase 1:
// view + files, no input). A Driver runs ONE capture loop at a time, driven
// by SessionControl downlink frames from the server: open starts capturing
// the screen at the hinted frame rate and streaming SessionFrame uplink
// frames; close stops it. Phase 1 ships JPEG over the existing mTLS Stream
// (the server relays only the latest frame per session, drop-old).
//
// Capture backends are per-OS (see capture_*.go). RMMWAY_SESSION_SOURCE
// selects a non-default backend; "test" forces the synthetic animated
// backend so the whole pipeline is e2e-testable on headless machines and in
// CI.
package session

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// Default/MaxFPS bound the SessionControl.open fps hint (0 = default).
const (
	DefaultFPS = 2
	MaxFPS     = 30
	// statusFrameInterval throttles repeated identical STATUS frames (a
	// headless box reports vnc_required once, then at most this often —
	// the viewer keeps the banner fresh without the agent crowding real
	// frames).
	statusFrameInterval = 5 * time.Second
)

// Frame is one captured screen (or a capture-unavailable status).
type Frame struct {
	JPEG   []byte
	Width  int
	Height int
	TSMS   int64
	// Status non-empty = status frame (JPEG empty): "vnc_required" (no
	// local display) or "unavailable" (backend error).
	Status string
}

// Capturer produces frames. Capture is called once per tick from the
// driver's loop and should return promptly; when no display can be
// captured it returns a status frame (Status set, JPEG empty) rather than
// an error — errors are reserved for backend breakage.
type Capturer interface {
	Name() string
	Capture(ctx context.Context) (*Frame, error)
	Close() error
}

// DriverConfig wires a Driver. SendFrame ships one SessionFrame uplink
// frame (the uplink's PushSessionFrame); NewCapturer is injectable for
// tests (nil = the RMMWAY_SESSION_SOURCE / per-OS default).
type DriverConfig struct {
	SendFrame   func(ctx context.Context, f *agentv1.SessionFrame) error
	NewCapturer func() (Capturer, error)
	Logger      *slog.Logger
}

// Driver is the capture-loop controller. It is safe for concurrent Control
// calls (the downlink reader) and Stop (shutdown).
type Driver struct {
	cfg DriverConfig

	mu   sync.Mutex
	live *liveSession
}

type liveSession struct {
	mu     sync.Mutex
	done   chan struct{}
	cancel context.CancelFunc

	sessionID string
	fps       int
	capturer  Capturer
	// status throttle state
	lastStatusAt time.Time
	lastStatus   string
}

// NewDriver builds a Driver.
func NewDriver(cfg DriverConfig) *Driver {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.NewCapturer == nil {
		cfg.NewCapturer = NewCapturer
	}
	return &Driver{cfg: cfg}
}

// Control applies one SessionControl downlink frame: open (create or
// live-adjust the capture loop), close (stop it).
func (d *Driver) Control(ctx context.Context, sc *agentv1.SessionControl) {
	switch a := sc.GetAction().(type) {
	case *agentv1.SessionControl_Open:
		d.open(ctx, a.Open.GetSessionId(), int(a.Open.GetFps()))
	case *agentv1.SessionControl_Close:
		d.close(a.Close.GetSessionId())
	}
}

// ActiveSessionID reports the live session ("" = none) — used by tests.
func (d *Driver) ActiveSessionID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.live == nil {
		return ""
	}
	return d.live.sessionID
}

// Stop closes the live session (agent shutdown; the server's stop route is
// the operator-facing twin).
func (d *Driver) Stop() {
	d.mu.Lock()
	ls := d.live
	d.live = nil
	d.mu.Unlock()
	if ls != nil {
		ls.cancel()
		ls.waitDone()
	}
}

func (d *Driver) open(ctx context.Context, sessionID string, fpsHint int) {
	if sessionID == "" {
		return
	}
	fps := clampFPS(fpsHint)
	d.mu.Lock()
	if ls := d.live; ls != nil && ls.sessionID == sessionID {
		if ls.fps == fps {
			d.mu.Unlock() // already exactly this
			return
		}
		// Live fps adjust: stop the old loop, reuse the capturer.
		d.live = nil
		d.mu.Unlock()
		d.stopLoop(ctx, ls)
		d.startLoop(ctx, sessionID, fps, ls.capturer)
		return
	}
	if d.live != nil {
		// Phase 1 = one session per device: a new open supersedes the old
		// (the server closes the old viewer's stream with it).
		old := d.live
		d.live = nil
		d.mu.Unlock()
		d.stopLoop(ctx, old)
		d.mu.Lock()
	}
	d.live = &liveSession{sessionID: sessionID, fps: fps}
	d.mu.Unlock()
	cap, err := d.cfg.NewCapturer()
	if err != nil {
		d.cfg.Logger.Warn("session: capture backend unavailable", "err", err)
		// The loop emits "unavailable" status frames so the server-side
		// session stays honest (the viewer sees the degraded state).
		d.startLoop(ctx, sessionID, fps, errCapturer{err})
		return
	}
	d.mu.Lock()
	d.live.capturer = cap
	d.mu.Unlock()
	d.startLoop(ctx, sessionID, fps, cap)
}

func (d *Driver) close(sessionID string) {
	d.mu.Lock()
	ls := d.live
	if ls != nil && (sessionID == "" || ls.sessionID == sessionID) {
		d.live = nil
	} else {
		ls = nil
	}
	d.mu.Unlock()
	if ls != nil {
		d.stopLoop(context.Background(), ls)
	}
}

// stopLoop cancels the session's loop and waits for it to exit.
func (d *Driver) stopLoop(ctx context.Context, ls *liveSession) {
	ls.cancel()
	ls.waitDone()
	_ = ctx
}

// startLoop spawns the capture goroutine for a published session record.
func (d *Driver) startLoop(ctx context.Context, sessionID string, fps int, cap Capturer) {
	lctx, cancel := context.WithCancel(ctx)
	ls := d.sessionFor(sessionID, fps)
	ls.mu.Lock()
	ls.cancel = cancel
	ls.fps = fps
	if ls.done != nil {
		<-ls.done // previous loop exited (fps adjust path)
	}
	ls.done = make(chan struct{})
	ls.mu.Unlock()
	d.cfg.Logger.Info("session: capture started", "session", sessionID, "fps", fps, "backend", cap.Name())
	go d.runLoop(lctx, ls, cap)
}

// sessionFor returns the published record for sessionID (created on the
// open() paths before startLoop runs).
func (d *Driver) sessionFor(sessionID string, fps int) *liveSession {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.live != nil && d.live.sessionID == sessionID {
		return d.live
	}
	return &liveSession{sessionID: sessionID, fps: fps}
}

func (ls *liveSession) waitDone() {
	ls.mu.Lock()
	done := ls.done
	ls.mu.Unlock()
	if done != nil {
		<-done
	}
}

// runLoop captures at the session's rate until canceled.
func (d *Driver) runLoop(ctx context.Context, ls *liveSession, cap Capturer) {
	defer func() {
		if cerr := cap.Close(); cerr != nil {
			d.cfg.Logger.Warn("session: capturer close", "err", cerr)
		}
		ls.mu.Lock()
		done := ls.done
		ls.mu.Unlock()
		if done != nil {
			close(done)
		}
	}()
	interval := time.Second / time.Duration(ls.fps)
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	var seq uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		f, err := cap.Capture(ctx)
		if err != nil {
			if d.emitStatus(ctx, ls, "unavailable", err) {
				continue
			}
		} else if f != nil && f.Status != "" {
			if d.emitStatus(ctx, ls, f.Status, nil) {
				continue
			}
		}
		frame := &agentv1.SessionFrame{
			SessionId:   ls.sessionID,
			Seq:         seq,
			Codec:       "jpeg",
			CaptureTsMs: time.Now().UnixMilli(),
		}
		if f != nil {
			frame.Jpeg = f.JPEG
			frame.Width = uint32(f.Width)
			frame.Height = uint32(f.Height)
		}
		seq++
		if err := d.cfg.SendFrame(ctx, frame); err != nil {
			// Send failed: the stream is likely dead. The uplink's
			// reconnect loop re-establishes it and the server re-sends the
			// open control, so stop this loop cleanly.
			d.cfg.Logger.Warn("session: frame send failed; loop stopping", "err", err)
			return
		}
	}
}

// emitStatus sends a throttled status frame. It returns true when the
// status was sent (or suppressed by the throttle) — i.e. no real frame was
// produced this tick.
func (d *Driver) emitStatus(ctx context.Context, ls *liveSession, status string, cause error) bool {
	ls.mu.Lock()
	now := time.Now()
	if status == ls.lastStatus && now.Sub(ls.lastStatusAt) < statusFrameInterval {
		ls.mu.Unlock()
		return true
	}
	ls.lastStatus = status
	ls.lastStatusAt = now
	ls.mu.Unlock()
	if cause != nil {
		d.cfg.Logger.Warn("session: capture error", "err", cause)
	}
	if err := d.cfg.SendFrame(ctx, &agentv1.SessionFrame{
		SessionId:   ls.sessionID,
		Codec:       "jpeg",
		CaptureTsMs: now.UnixMilli(),
		Status:      status,
	}); err != nil {
		return false
	}
	return true
}

func clampFPS(fps int) int {
	if fps <= 0 {
		return DefaultFPS
	}
	if fps > MaxFPS {
		return MaxFPS
	}
	return fps
}

// errCapturer surfaces a capturer-factory failure as status frames.
type errCapturer struct{ err error }

func (e errCapturer) Name() string             { return "unavailable" }
func (e errCapturer) Capture(context.Context) (*Frame, error) {
	return &Frame{Status: "unavailable"}, nil
}
func (e errCapturer) Close() error { return nil }

// NewCapturer picks the backend: RMMWAY_SESSION_SOURCE overrides the
// per-OS default ("test" = synthetic animated frames).
func NewCapturer() (Capturer, error) {
	if src := os.Getenv("RMMWAY_SESSION_SOURCE"); src == "test" {
		return newTestCapturer(), nil
	}
	return defaultCapturer()
}

// ErrNoDisplay marks a backend that cannot capture (no display) — the
// driver turns it into the "vnc_required" status path.
var ErrNoDisplay = errors.New("no display available")
