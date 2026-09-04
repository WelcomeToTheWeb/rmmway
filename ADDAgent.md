# Add-Agent Flow: Diagnosis and Improvements

## The Flow Today

To add a new device to RMMWay, an operator:

1. Opens the RMMWay UI (e.g. `https://rmm.example.com`)
2. Clicks "Add a device" on the Devices page
3. **Manually enters the server URL** in the modal
4. The UI mints a one-time bootstrap token (POST `/api/bootstrap`)
5. The UI generates copy-paste install commands (curl/bash for Linux/macOS, iwr/powershell for Windows)
6. The operator pastes the install command on the target machine
7. The install script downloads the agent binary, writes config, and starts the service
8. The agent enrolls via HTTP POST to `{server}/agent/enroll` with the one-time token
9. The agent switches to mTLS on port 50052 for ongoing communication

## Friction Points

### 1. Server URL must be manually entered (FIXED)

The "Add a device" modal defaults to `window.location.origin`, which is correct when the operator accesses the UI directly but **wrong when behind a reverse proxy**. For example:

- Operator accesses `http://localhost:8080` via dev proxy, but the public URL is `https://rmm.example.com`
- Agent enrolls to `localhost:8080` (unreachable from the target machine) instead of `https://rmm.example.com`
- Enrollment fails with a connection error

**Before:** The operator had to know and manually type the public URL every time.

**After:** The server accepts a `RMMWAY_PUBLIC_URL` env var. The UI reads it via `GET /api/public-url` and prefills the field automatically.

### 2. mTLS cert SANs don't include the public hostname (FIXED)

When an agent connects to `rmm.example.com:50052` (the mTLS port), it must verify the server's TLS certificate. If the certificate's Subject Alternative Names (SANs) don't include `rmm.example.com`, the connection fails with an x509 hostname verification error.

Previously, operators had to:
- Know to set `RMMWAY_GRPC_MTLS_SANs=rmm.example.com` in `.env.prod`
- Or manually edit the docker-compose.yml

**After:** `RMMWAY_PUBLIC_URL` automatically seeds the mTLS server cert SANs with the host portion, so remote agents' hostname verification passes without extra configuration.

### 3. No feedback if the mTLS port isn't reachable (TODO)

The agent silently fails to connect if port 50052 is blocked by a firewall or not published by the container runtime. There's currently no pre-enrollment connectivity check from the target machine.

**Ideas for improvement:**
- Add a `--test` flag to install scripts that dials the server and reports back
- Add a "Test connection" button in the Add Device modal that checks from the server side (limited value — can't test from the agent's perspective)
- Better error messages from the agent when it can't reach the mTLS endpoint

### 4. No QR code for scanning (TODO)

For bulk deployments, scanning a QR code with a device camera would be faster than copy-pasting a long install command.

**Ideas for improvement:**
- Generate a QR code in the Add Device modal encoding the install command
- Could use a lightweight in-browser QR generation library

### 5. No validation of server URL format (TODO)

The Server URL input accepts any text. A user could type `rmm.example.com` (no scheme) or `http://rmm.example.com` (not https) and it would silently work differently than expected.

**Ideas for improvement:**
- Basic validation: must contain `://`
- Warning if `http://` is used (not encrypted)
- Suggest `https://` prefix if missing

## Improvements Implemented in This PR

### Server-side

1. **`RMMWAY_PUBLIC_URL` environment variable** — accepts the operator's public URL (e.g. `https://rmm.example.com`).
2. **`GET /api/public-url` endpoint** — open (no auth) endpoint that returns the configured public URL as JSON: `{"url": "https://rmm.example.com"}`. Returns `{"url": ""}` when unset.
3. **mTLS SAN seeding** — the mTLS server certificate automatically includes the host portion of `RMMWAY_PUBLIC_URL` in its SANs, so remote agents' x509 hostname verification passes.
4. **Startup logging** — the server logs the computed mTLS SANs and the configured public URL at startup for operational visibility.

### Frontend

1. **Prefill from `/api/public-url`** — the Add Device modal fetches the configured public URL on mount and prefills the Server URL field with it.
2. **Origin mismatch hint** — when the browser's `window.location.origin` differs from the configured public URL, a subtle note explains the prefill source.

### Documentation

1. **`.env.prod.example`** — updated to document `RMMWAY_PUBLIC_URL` with clear instructions.
2. **`docker-compose.prod.yml`** — passes `RMMWAY_PUBLIC_URL` through to the server container.
3. **`README.md`** — updated production deployment instructions to reference `RMMWAY_PUBLIC_URL` instead of `RMMWAY_DOMAIN`.

## Future Improvements

- [ ] Connection test from the Add Device modal (server-side proxy check)
- [ ] QR code generation for install command
- [ ] Server URL format validation with hints
- [ ] Auto-detect public IP/hostname for local deployments (LAN scenario)
- [ ] Port 50052 firewall check guidance in the UI
