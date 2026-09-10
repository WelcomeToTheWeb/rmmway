# Remote Session — Operator Guide

RMMWay remote sessions provide live screen viewing of managed devices.
Phase 1 (v1.0.0) supports view-only sessions; phase 2 adds two-way
mouse/keyboard input.

## Opening a Session

1. Navigate to the **Devices** list
2. Select the target device
3. Click the **Remote Session** button in the device toolbar

The session viewer opens in a new tab. The agent begins capturing screen
frames at approximately 2 frames per second (configurable up to 30 fps).

## Session Viewer Controls

- **Fullscreen:** Click the fullscreen icon in the viewer toolbar
- **Frame rate:** Adjust via the viewer settings (requires session close/reopen)
- **Close:** Click the X button or navigate away

## Status Indicators

The viewer shows one of the following statuses:

| Status | Meaning |
| ------ | ------- |
| **Connected** | Frames are being received |
| **Connecting** | Waiting for first frame from agent |
| **vnc_required** | Device has no local display; open a local VNC session on the device to capture its screen |
| **unavailable** | Agent's capture backend errored |

## Session Duration

Sessions persist until explicitly closed by the operator. The agent
continues capturing frames while the session is open. Long-running
sessions (hours) are supported but may consume additional device resources.

## Permissions

Operators need the `remote_session` capability to open sessions. By default,
this is granted to the `admin` and `technician` roles. Viewers cannot open
sessions.

## Security Model

- Screen frames are transmitted over the same mTLS gRPC stream as heartbeats
- Frames are dropped old (only the latest is relayed) to prevent backpressure
- Session IDs are server-minted and echoed in every frame for validation
- No session state is persisted; closing the viewer ends the session

## Troubleshooting

### No frames appear

1. Verify the agent is online (check the device list)
2. Check for the `vnc_required` status (headless Linux devices need a local VNC session)
3. Check agent logs for capture backend errors
4. Verify network connectivity between agent and server

### Frames are stale or slow

1. The viewer only displays the latest frame; stale frames indicate network issues
2. Check device CPU usage (high load slows capture)
3. Try reducing the frame rate

### Capture backend errors

Common errors:

- `no display found` (Linux headless) → open local VNC session
- `capture timeout` → device under heavy load
- `permissions` (macOS) → grant screen recording permission to the agent

On macOS, the agent requires Screen Recording permission in System Preferences → Privacy & Security → Screen Recording.

## Phase 2 Roadmap

The following features are planned for phase 2:

- Mouse click and movement input
- Keyboard input
- Multi-monitor support
- Session recording
- Co-viewer mode (multiple operators viewing simultaneously)

---

*Remote session documentation, v1.0.0 release.*
