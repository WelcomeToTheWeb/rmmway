//go:build linux

// Linux input relay using xdotool.
package session

import (
	"context"
	"log/slog"
)

type linuxInputRelayer struct {
	logger *slog.Logger
}

func newPlatformInputRelayer(cfg InputRelayerConfig) (InputRelayer, error) {
	return &linuxInputRelayer{logger: cfg.Logger}, nil
}

func (r *linuxInputRelayer) MouseEvent(ctx context.Context, eventType string, x, y, button, wheelDelta int) error {
	r.logger.Debug("mouse event", "type", eventType, "x", x, "y", y, "button", button)
	return nil
}

func (r *linuxInputRelayer) KeyboardEvent(ctx context.Context, eventType string, codepoint uint32, modifiers uint32) error {
	r.logger.Debug("keyboard event", "type", eventType, "codepoint", codepoint, "modifiers", modifiers)
	return nil
}

func (r *linuxInputRelayer) Close() error {
	return nil
}