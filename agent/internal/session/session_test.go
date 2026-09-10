package session

import (
	"context"
	"sync"
	"testing"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// mockInputRelayer records input events for test assertions.
type mockInputRelayer struct {
	mu          sync.Mutex
	mouse       []mockMouseEvent
	keyboard    []mockKeyboardEvent
	mouseErr    error
	keyboardErr error
}

type mockMouseEvent struct {
	Type       string
	X, Y       int
	Button     int
	WheelDelta int
}

type mockKeyboardEvent struct {
	Type      string
	Codepoint uint32
	Modifiers uint32
}

func (m *mockInputRelayer) MouseEvent(ctx context.Context, eventType string, x, y, button, wheelDelta int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mouse = append(m.mouse, mockMouseEvent{
		Type: eventType, X: x, Y: y, Button: button, WheelDelta: wheelDelta,
	})
	return m.mouseErr
}

func (m *mockInputRelayer) KeyboardEvent(ctx context.Context, eventType string, codepoint uint32, modifiers uint32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keyboard = append(m.keyboard, mockKeyboardEvent{
		Type: eventType, Codepoint: codepoint, Modifiers: modifiers,
	})
	return m.keyboardErr
}

func (m *mockInputRelayer) Close() error { return nil }

func (m *mockInputRelayer) mouseEvents() []mockMouseEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mouse
}

func (m *mockInputRelayer) keyboardEvents() []mockKeyboardEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.keyboard
}

// TestDriverInputRouting verifies that the session driver routes
// mouse and keyboard events from downlink frames to the input relayer.
func TestDriverInputRouting(t *testing.T) {
	var sentFrames []*agentv1.SessionFrame
	d := NewDriver(DriverConfig{
		SendFrame: func(ctx context.Context, f *agentv1.SessionFrame) error {
			sentFrames = append(sentFrames, f)
			return nil
		},
		NewCapturer: func() (Capturer, error) {
			return newTestCapturer(), nil
		},
	})
	d.input = &mockInputRelayer{}

	ctx := context.Background()

	// Send a mouse move event.
	d.Control(ctx, &agentv1.SessionControl{
		Action: &agentv1.SessionControl_MouseEvent_{
			MouseEvent: &agentv1.SessionControl_MouseEvent{
				SessionId:  "test-session",
				EventType:  "move",
				X:          100,
				Y:          200,
				Button:     0,
				WheelDelta: 0,
			},
		},
	})

	// Send a mouse down event.
	d.Control(ctx, &agentv1.SessionControl{
		Action: &agentv1.SessionControl_MouseEvent_{
			MouseEvent: &agentv1.SessionControl_MouseEvent{
				SessionId:  "test-session",
				EventType:  "down",
				X:          100,
				Y:          200,
				Button:     0,
				WheelDelta: 0,
			},
		},
	})

	// Send a mouse up event.
	d.Control(ctx, &agentv1.SessionControl{
		Action: &agentv1.SessionControl_MouseEvent_{
			MouseEvent: &agentv1.SessionControl_MouseEvent{
				SessionId:  "test-session",
				EventType:  "up",
				X:          100,
				Y:          200,
				Button:     0,
				WheelDelta: 0,
			},
		},
	})

	// Send a keyboard event.
	d.Control(ctx, &agentv1.SessionControl{
		Action: &agentv1.SessionControl_KeyboardEvent_{
			KeyboardEvent: &agentv1.SessionControl_KeyboardEvent{
				SessionId: "test-session",
				EventType: "down",
				Codepoint: 65,
				Modifiers: 0,
			},
		},
	})

	// Verify events were relayed.
	relayer := d.input.(*mockInputRelayer)
	mouseEvents := relayer.mouseEvents()
	keyboardEvents := relayer.keyboardEvents()

	if len(mouseEvents) != 3 {
		t.Fatalf("expected 3 mouse events, got %d", len(mouseEvents))
	}
	if len(keyboardEvents) != 1 {
		t.Fatalf("expected 1 keyboard event, got %d", len(keyboardEvents))
	}

	// Verify first event was a move.
	if mouseEvents[0].Type != "move" || mouseEvents[0].X != 100 || mouseEvents[0].Y != 200 {
		t.Errorf("unexpected first mouse event: %+v", mouseEvents[0])
	}

	// Verify keyboard event.
	if keyboardEvents[0].Codepoint != 65 || keyboardEvents[0].Modifiers != 0 {
		t.Errorf("unexpected keyboard event: %+v", keyboardEvents[0])
	}

	d.Stop()
}

// TestDriverInputRelayerErrors verifies that relayer errors are logged, not
// propagated (so a failed event doesn't stop the control loop).
func TestDriverInputRelayerErrors(t *testing.T) {
	relayer := &mockInputRelayer{mouseErr: context.DeadlineExceeded, keyboardErr: context.DeadlineExceeded}
	d := NewDriver(DriverConfig{
		SendFrame:   func(ctx context.Context, f *agentv1.SessionFrame) error { return nil },
		NewCapturer: func() (Capturer, error) { return newTestCapturer(), nil },
	})
	d.input = relayer

	ctx := context.Background()

	// Fire both types of events — the driver must not panic or stop.
	d.Control(ctx, &agentv1.SessionControl{
		Action: &agentv1.SessionControl_MouseEvent_{
			MouseEvent: &agentv1.SessionControl_MouseEvent{EventType: "move", X: 10, Y: 10},
		},
	})
	d.Control(ctx, &agentv1.SessionControl{
		Action: &agentv1.SessionControl_KeyboardEvent_{
			KeyboardEvent: &agentv1.SessionControl_KeyboardEvent{EventType: "down", Codepoint: 65},
		},
	})

	// Events should still be recorded despite the error.
	if len(relayer.mouseEvents()) != 1 {
		t.Errorf("expected mouse event to be recorded despite error")
	}
	if len(relayer.keyboardEvents()) != 1 {
		t.Errorf("expected keyboard event to be recorded despite error")
	}

	d.Stop()
}

// TestDriverInputRelayerNil verifies that events are silently dropped
// when no input relayer is configured (phase 1 view-only operation).
func TestDriverInputRelayerNil(t *testing.T) {
	d := NewDriver(DriverConfig{
		SendFrame:   func(ctx context.Context, f *agentv1.SessionFrame) error { return nil },
		NewCapturer: func() (Capturer, error) { return newTestCapturer(), nil },
	})
	// No input relayer set.

	ctx := context.Background()

	// Fire events — they should be dropped, not panic.
	d.Control(ctx, &agentv1.SessionControl{
		Action: &agentv1.SessionControl_MouseEvent_{
			MouseEvent: &agentv1.SessionControl_MouseEvent{EventType: "move", X: 10, Y: 10},
		},
	})
	d.Control(ctx, &agentv1.SessionControl{
		Action: &agentv1.SessionControl_KeyboardEvent_{
			KeyboardEvent: &agentv1.SessionControl_KeyboardEvent{EventType: "down", Codepoint: 65},
		},
	})

	// Just verify no panic occurred; nothing should have been recorded.
	d.Stop()
}

// TestClampFPS ensures the fps hint is clamped to valid range.
func TestClampFPS(t *testing.T) {
	tests := []struct {
		in, out int
	}{
		{0, DefaultFPS},
		{-5, DefaultFPS},
		{1, 1},
		{15, 15},
		{30, 30},
		{60, MaxFPS},
	}
	for _, tc := range tests {
		if got := clampFPS(tc.in); got != tc.out {
			t.Errorf("clampFPS(%d) = %d, want %d", tc.in, got, tc.out)
		}
	}
}
