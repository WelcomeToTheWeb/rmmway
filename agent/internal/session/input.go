// Remote session input relay — phase 2 (two-way remote control).
// Receives mouse/keyboard events from the server and injects them into the
// local desktop environment.
package session

import (
	"context"
	"log/slog"
)

// InputRelayer relays remote input events to the local desktop.
type InputRelayer interface {
	// MouseEvent relays a mouse event.
	MouseEvent(ctx context.Context, eventType string, x, y, button, wheelDelta int) error
	// KeyboardEvent relays a keyboard event.
	KeyboardEvent(ctx context.Context, eventType string, codepoint uint32, modifiers uint32) error
	// Close releases resources.
	Close() error
}

// InputRelayerConfig configures an input relayer.
type InputRelayerConfig struct {
	Logger *slog.Logger
}

// NewInputRelayer creates a platform-appropriate input relayer.
func NewInputRelayer(cfg InputRelayerConfig) (InputRelayer, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return newPlatformInputRelayer(cfg)
}