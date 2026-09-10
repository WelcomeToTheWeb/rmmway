# Remote Session Protocol

The remote session feature enables two-way remote control of devices through
the RMMWay agent. This document describes the architecture, protocol, and
environment configuration.

## Architecture

```
Browser Viewer ←─ SSE frames ── Server Relay ── gRPC Stream ── Agent Driver
       │                                              │
       └───── Input Events (WebSocket) ───────────────┘
```

### Components

1. **Session Driver** (`agent/internal/session/session.go`)
   - Runs the capture loop for one active session per device
   - Driven by `SessionControl` downlink frames from the server
   - Streams `SessionFrame` uplink frames at the configured frame rate

2. **Capture Backends** (`agent/internal/session/capture_*.go`)
   - Platform-specific screen capture implementations
   - Linux: uses X11 or Wayland via `import`/`grim` commands
   - Windows: uses `MagickImage`/GDI via PowerShell
   - Darwin: uses `screencapture` CLI tool
   - Test backend (`capture_test_backend.go`): synthetic animated frames

3. **Input Relayer** (`agent/internal/session/input_*.go`)
   - Receives mouse/keyboard events from the server
   - Injects events into the local desktop environment
   - Phase 2 feature (two-way remote control)

## Protocol

### Frame Types (proto/rmmway/agent/v1/agent.proto)

#### Uplink: SessionFrame

```protobuf
message SessionFrame {
  string session_id = 1;   // Echo of SessionControl.open session_id
  uint64 seq = 2;          // Monotonic frame counter (resets per open)
  string codec = 3;        // Always "jpeg" in phase 1
  uint32 width = 4;
  uint32 height = 5;
  bytes jpeg = 6;          // The frame (empty on status frames)
  int64 capture_ts_ms = 7; // Agent wall clock at capture (Unix ms)
  string status = 8;       // Non-empty = status frame (see below)
}
```

Status frame values:

- `"vnc_required"` — No local display to capture (e.g., headless Linux box)
- `"unavailable"` — Capture backend errored

#### Downlink: SessionControl

```protobuf
message SessionControl {
  oneof action {
    OpenSession open = 1;
    CloseSession close = 2;
    MouseEvent mouse_event = 10;       // Phase 2
    KeyboardEvent keyboard_event = 11; // Phase 2
  }
}
```

##### OpenSession

```protobuf
message OpenSession {
  string session_id = 1;  // Server-minted session id
  int32 fps = 2;          // Frame rate hint (0 = agent default ~2 fps)
}
```

##### MouseEvent

```protobuf
message MouseEvent {
  string session_id = 1;
  string event_type = 2;  // "move" | "down" | "up" | "click" | "wheel"
  int32 x = 3;            // Absolute screen coordinates
  int32 y = 4;
  int32 button = 5;       // 0=left, 1=middle, 2=right (0 for move/wheel)
  int32 wheel_delta = 6;  // Positive=up, negative=down
}
```

##### KeyboardEvent

```protobuf
message KeyboardEvent {
  string session_id = 1;
  string event_type = 2;  // "down" | "up" | "key"
  uint32 codepoint = 3;   // Unicode codepoint (0 for modifier-only)
  uint32 modifiers = 4;   // Bitmask: shift=1, ctrl=2, alt=4, meta=8
}
```

### Session Lifecycle

1. **Open**: Server sends `SessionControl.Open` with session_id and fps hint
   - Agent validates fps (clamped to 1-30, default 2)
   - Agent selects capture backend
   - Agent starts capture loop, streaming `SessionFrame` uplink frames

2. **Active**: Agent streams frames at the configured rate
   - Server relays only the latest frame per session (drop-old)
   - Viewers receive frames via SSE
   - Input events from viewers are relayed as `MouseEvent`/`KeyboardEvent`

3. **Close**: Server sends `SessionControl.Close`
   - Agent stops capture loop
   - Agent releases capture backend
   - Session state cleared

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `RMMWAY_SESSION_SOURCE` | (platform default) | Capture backend override. Use `"test"` for synthetic animated frames (useful for headless testing). |

## Platform-Specific Implementation

### Windows Input (input_windows.go)

Uses the Windows SendInput API via `golang.org/x/sys/windows`:

- Mouse coordinates are normalized to 0-65535 range (SendInput's absolute space)
- Keyboard events use `VkKeyScanW` to map Unicode codepoints to virtual-key codes
- Each event is sent synchronously; SendInput's return value is verified
- Modifier keys are sent before/after character events

### Linux Input (input_linux.go)

Uses X11's XTest extension via the `xdotool` CLI:

- Mouse: `xdotool mousemove <x> <y>`, `click <button>`, `wheelup`/`wheeldown`
- Keyboard: `key <codepoint>` or `keydown`/`keyup` for modifiers

### Darwin Input (input_darwin.go)

Uses AppleScript/CGEvent via `osascript`:

- Mouse: `click at <x>,<y>` with modifier key state
- Keyboard: `keystroke "<char>" using {shift down}` etc.

## Testing

### Unit Tests

- `agent/internal/session/session_test.go` — Driver input routing with mock relayer
- Tests verify:
  - Mouse events are routed to the input relayer
  - Keyboard events are routed to the input relayer
  - Relayer errors are logged (not propagated)
  - Events are dropped when no relayer is configured (phase 1)
  - FPS clamping logic

### Integration Testing

- Set `RMMWAY_SESSION_SOURCE=test` to use the synthetic animated backend
- The test backend produces deterministic animated frames that can be verified
- Works on any platform (pure Go, no cgo)

## Operator View

For end-user operator documentation on using remote sessions, see
[Remote Session — Operator Guide](remote-session-operator.md).
