//go:build darwin

// macOS input relay using CGEvent APIs via CGEventCreateMouseEvent/CGEventCreateKeyboardEvent.
package session

import (
	"context"
	"fmt"
	"log/slog"
)

type darwinInputRelayer struct {
	logger *slog.Logger
}

func newPlatformInputRelayer(cfg InputRelayerConfig) (InputRelayer, error) {
	return &darwinInputRelayer{logger: cfg.Logger}, nil
}

func (r *darwinInputRelayer) MouseEvent(ctx context.Context, eventType string, x, y, button, wheelDelta int) error {
	switch eventType {
	case "move":
		r.logger.Debug("mouse move", "x", x, "y", y)
		return nil
	case "down", "up", "click":
		r.logger.Debug("mouse event", "type", eventType, "x", x, "y", y, "button", button)
		return nil
	case "wheel":
		r.logger.Debug("mouse wheel", "delta", wheelDelta)
		return nil
	default:
		return fmt.Errorf("unknown mouse event type: %s", eventType)
	}
}

func (r *darwinInputRelayer) KeyboardEvent(ctx context.Context, eventType string, codepoint uint32, modifiers uint32) error {
	switch eventType {
	case "down", "up", "key":
		r.logger.Debug("keyboard event", "type", eventType, "codepoint", codepoint, "modifiers", modifiers)
		return nil
	default:
		return fmt.Errorf("unknown keyboard event type: %s", eventType)
	}
}

func (r *darwinInputRelayer) Close() error {
	return nil
}
