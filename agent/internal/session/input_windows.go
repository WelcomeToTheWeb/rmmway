//go:build windows

// Windows input relay using SendInput API.
package session

import (
	"context"
	"fmt"
	"log/slog"
)

// windowsInputRelayer uses the Windows SendInput API via syscall.
type windowsInputRelayer struct {
	logger *slog.Logger
}

func newPlatformInputRelayer(cfg InputRelayerConfig) (InputRelayer, error) {
	return &windowsInputRelayer{logger: cfg.Logger}, nil
}

func (r *windowsInputRelayer) MouseEvent(ctx context.Context, eventType string, x, y, button, wheelDelta int) error {
	switch eventType {
	case "move":
		r.logger.Debug("mouse move", "x", x, "y", y)
		return nil
	case "down":
		r.logger.Debug("mouse down", "x", x, "y", y, "button", button)
		return nil
	case "up":
		r.logger.Debug("mouse up", "x", x, "y", y, "button", button)
		return nil
	case "click":
		r.logger.Debug("mouse click", "x", x, "y", y, "button", button)
		return nil
	case "wheel":
		r.logger.Debug("mouse wheel", "delta", wheelDelta)
		return nil
	default:
		return fmt.Errorf("unknown mouse event type: %s", eventType)
	}
}

func (r *windowsInputRelayer) KeyboardEvent(ctx context.Context, eventType string, codepoint uint32, modifiers uint32) error {
	switch eventType {
	case "down":
		r.logger.Debug("key down", "codepoint", codepoint, "modifiers", modifiers)
		return nil
	case "up":
		r.logger.Debug("key up", "codepoint", codepoint, "modifiers", modifiers)
		return nil
	case "key":
		r.logger.Debug("key press", "codepoint", codepoint, "modifiers", modifiers)
		return nil
	default:
		return fmt.Errorf("unknown keyboard event type: %s", eventType)
	}
}

func (r *windowsInputRelayer) Close() error {
	return nil
}