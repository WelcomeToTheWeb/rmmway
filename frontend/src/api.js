// Thin fetch wrapper for the RMMWay operator API.
// Every call is sent to the same origin; the Vite dev server proxies
// /api/* to the Go server on :8080 (see vite.config.js).
//
// The caller passes the JWT (from useAuth) on each request; on a 401 the
// wrapper returns {unauthorized: true} so the caller can bounce to /login.

export class ApiError extends Error {
  constructor(message, status, unauthorized = false) {
    super(message);
    this.status = status;
    this.unauthorized = unauthorized;
  }
}

async function request(path, { method = "GET", body, token } = {}) {
  const headers = { "Content-Type": "application/json" };
  if (token) headers["Authorization"] = `Bearer ${token}`;
  const res = await fetch(path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 401) throw new ApiError("unauthorized", 401, true);
  if (!res.ok) {
    let msg = `HTTP ${res.status}`;
    try {
      const j = await res.json();
      if (j && j.error) msg = j.error;
    } catch {
      /* ignore */
    }
    throw new ApiError(msg, res.status);
  }
  if (res.status === 204) return null;
  const ct = res.headers.get("content-type") || "";
  return ct.includes("application/json") ? res.json() : res.text();
}

export const api = {
  // ---- A-2: first-boot setup wizard --------------------------------------
  // GET /api/setup/status -> { available, setup, org_name?, admin_user?,
  // smtp_host?, smtp_configured }. available=false (in-memory server) or
  // setup=true -> the UI shows the normal login; setup=false -> the wizard.
  setupStatus: () => request("/api/setup/status"),

  // POST /api/setup/complete — one-time: mints the root admin, re-issues the
  // org CA under org_name, and persists the SMTP outbox. 409 = already done.
  completeSetup: (body) =>
    request("/api/setup/complete", { method: "POST", body }),

  // POST /api/setup/smtp/test — send the outbox verification mail.
  // body: { smtp: {host, port, from, username, password}, to? }.
  testSmtp: (body) => request("/api/setup/smtp/test", { method: "POST", body }),

  // POST /api/login -> { token, expiry }
  login: (username, password) =>
    request("/api/login", { method: "POST", body: { username, password } }),

  // GET /api/devices -> Device[] (each has id, hostname, os, arch,
  // agent_version, interfaces[], tags[], online, first_seen, last_seen)
  devices: (token) => request("/api/devices", { token }),

  // POST /api/bootstrap -> { bootstrap_token, device_id } ("Add a device",
  // auth-gated). Mints a one-time enroll token bound to a pre-allocated
  // device id; the UI turns it into a copy-paste install command. 401 = not
  // signed in.
  bootstrap: (token) =>
    request("/api/bootstrap", { method: "POST", token, body: {} }),

  // GET /api/search?q=... -> Meilisearch payload { hits[], estimatedTotalHits }.
  // Each hit is a device doc (id, hostname, ip[], tags[], os, arch, ...).
  // Backing for the Cmd-K palette.
  search: (token, q) =>
    request(`/api/search?q=${encodeURIComponent(q || "")}`, { token }),

  // POST /api/devices/{id}/commands -> { command_id } (200) or error status.
  // body: { action:"run_script"|"reboot", lang?, script? (base64), args? }
  dispatch: (token, deviceId, body) =>
    request(`/api/devices/${encodeURIComponent(deviceId)}/commands`, {
      method: "POST",
      token,
      body,
    }),

  // GET /api/devices/{id}/commands -> { device_id, pending[], results[] }
  // (D-1: the command audit view). pending[] = dispatched commands with no
  // final report yet — proto JSON (PascalCase): Id, IssuedAtMs, Action
  // (the oneof serializes as { RunScript: { lang, script_b64... } } or
  // { Reboot: {...} }). results[] = the agent's reports (snake_case):
  // command_id, status (NUMBER: 1=RECEIVED 2=RUNNING 3=SUCCEEDED 4=FAILED
  // 5=TIMED_OUT 6=UNSUPPORTED 7=REFUSED), exit_code, stdout_tail,
  // stderr_tail, error, completed_at_ms. A command leaves pending[] once
  // the agent reports a final status (3–7). 404 unknown device; 503 when
  // command state is unwired (in-memory server).
  commands: (token, deviceId, { limit = 100 } = {}) => {
    const q = new URLSearchParams();
    q.set("limit", String(limit));
    return request(
      `/api/devices/${encodeURIComponent(deviceId)}/commands?${q.toString()}`,
      { token },
    );
  },

  // PATCH /api/devices/{id} { tags: [...] } -> { device, indexed } (B-2:
  // replaces the device's whole tag list; the server normalizes tags and
  // best-effort re-syncs the search index — indexed=false means Meilisearch
  // was down, the next heartbeat re-covers it). 400 = invalid tag shape.
  setTags: (token, deviceId, tags) =>
    request(`/api/devices/${encodeURIComponent(deviceId)}`, {
      method: "PATCH",
      token,
      body: { tags },
    }),

  // POST /api/devices/bulk/commands (B-2: ONE capability-gated command
  // fanned out to every device carrying a tag — a "group" like web).
  // body: { tag, action:"run_script"|"reboot", lang?, script? (base64),
  // args?, timeout_s? } -> { tag, requested, pushed[{device_id,command_id}],
  // offline[], failed{device_id:err} }. 403 = the session lacks the
  // action's capability; 404 = no device carries the tag.
  bulkDispatch: (token, body) =>
    request("/api/devices/bulk/commands", { method: "POST", token, body }),

  // ---- D-2: global event journal -----------------------------------------
  // GET /api/events?after=&limit=&category=&device=&type= -> Envelope[]
  // (oldest first; journal entries with seq > after, up to limit — server
  // default 200, max 1000). category is one of alert|inventory|automation|
  // other (400 on unknown — command results are journaled as "automation");
  // device = exact device_id; type = exact bus subject. Each envelope:
  // { id (journal seq), version, source, category, type, device_id?, at,
  // event } where `event` is the full bus event (flow.Event JSON: type,
  // device_id, source?, value?, command_id?, status?, message?, data{...},
  // at). 503 when the webhook framework is unwired (in-memory server).
  eventJournal: (
    token,
    { after = 0, limit = 200, category = "", device = "", type = "" } = {},
  ) => {
    const q = new URLSearchParams();
    q.set("after", String(after));
    q.set("limit", String(limit));
    if (category) q.set("category", category);
    if (device) q.set("device", device);
    if (type) q.set("type", type);
    return request(`/api/events?${q.toString()}`, { token });
  },

  // GET /api/devices/{id}/events?limit=&level= -> { device_id, events[] }
  // (newest first). W6-1: the device's recent indexed agent-log events
  // (the Timescale copy of what also ships to Loki). Each event has
  // id, level, msg, attrs, timestamp_ms, time.
  events: (token, deviceId, { limit = 100, level = "" } = {}) => {
    const q = new URLSearchParams();
    q.set("limit", String(limit));
    if (level) q.set("level", level);
    return request(
      `/api/devices/${encodeURIComponent(deviceId)}/events?${q.toString()}`,
      { token },
    );
  },

  // Per-device metrics viewer: the (name, source) series the device has
  // reported over the window (the chart's metric picker). -> { device_id,
  // range, series: [{name, source, last, count}] }.
  metricsNames: (token, deviceId, range = "7d") =>
    request(
      `/api/devices/${encodeURIComponent(deviceId)}/metrics?range=${range}`,
      { token },
    ),

  // The bucketed samples of one series over a range (the chart). ->
  // { device_id, name, source, range, bucket_s, count, min, max, last,
  // points: [[ts_ms, value], ...] } (ascending).
  metricsSeries: (token, deviceId, name, source, range = "24h") => {
    const q = new URLSearchParams();
    q.set("name", name);
    if (source) q.set("source", source);
    q.set("range", range);
    return request(
      `/api/devices/${encodeURIComponent(deviceId)}/metrics/series?${q.toString()}`,
      { token },
    );
  },

  // ---- W2-4: baseline-driven alerts + inbox ---------------------------
  // GET /api/alerts?status=open|acked|resolved&device_id=...&limit=...
  // -> Alert[] (id, device_id, hostname, name, source, status, channel,
  // score, value, expected, events, first_at, last_at, resolved_at, acked_at)
  alerts: (token, { status = "", device_id = "", limit = 200 } = {}) => {
    const q = new URLSearchParams();
    if (status) q.set("status", status);
    if (device_id) q.set("device_id", device_id);
    if (limit) q.set("limit", String(limit));
    const qs = q.toString();
    return request(`/api/alerts${qs ? `?${qs}` : ""}`, { token });
  },

  // GET /api/alerts/counts -> { open, acked, resolved } (drives the nav badge)
  alertCounts: (token) => request("/api/alerts/counts", { token }),

  // PATCH /api/alerts/{id} { status: "acked" | "resolved" } -> the Alert.
  // Transitions: open -> acked | resolved, acked -> resolved. Re-opening
  // is refused by the server.
  setAlertStatus: (token, id, status) =>
    request(`/api/alerts/${id}`, { method: "PATCH", token, body: { status } }),

  // ---- W5-2: event-driven automation chains (visual composer) --------
  // GET /api/flows -> Flow[] (id, name, description, graph, enabled, ...)
  flows: (token) => request("/api/flows", { token }),

  // POST /api/flows { name, description, graph, cooldown_seconds?, enabled? }
  // -> 201 {flow}. graph = { nodes: [...] } (validated server-side).
  createFlow: (token, body) =>
    request("/api/flows", { method: "POST", token, body }),

  // PATCH /api/flows/{id} -> the updated flow (partial body ok).
  updateFlow: (token, id, body) =>
    request(`/api/flows/${id}`, { method: "PATCH", token, body }),

  // DELETE /api/flows/{id} -> 204.
  deleteFlow: (token, id) =>
    request(`/api/flows/${id}`, { method: "DELETE", token }),

  // POST /api/flows/{id}/trigger { device_id, source?, value? } -> 202.
  // Fires a SYNTHETIC trigger onto the NATS bus; the chain then proceeds
  // asynchronously (poll flowRuns to watch it).
  triggerFlow: (token, id, body) =>
    request(`/api/flows/${id}/trigger`, { method: "POST", token, body }),

  // GET /api/flows/runs?status=&flow_id=&device_id= -> Run[]
  flowRuns: (token, { status = "", flow_id = "", device_id = "" } = {}) => {
    const q = new URLSearchParams();
    if (status) q.set("status", status);
    if (flow_id) q.set("flow_id", String(flow_id));
    if (device_id) q.set("device_id", device_id);
    const qs = q.toString();
    return request(`/api/flows/runs${qs ? `?${qs}` : ""}`, { token });
  },

  // GET /api/flows/runs/{id} -> { run, events } (the node audit trail).
  flowRun: (token, id) => request(`/api/flows/runs/${id}`, { token }),

  // POST /api/flows/sweep -> { active_runs } (re-cover in-flight runs).
  sweepFlows: (token) => request("/api/flows/sweep", { method: "POST", token }),

  // ---- D-3: self-healing playbook engine (W5-1) -------------------------
  // GET /api/heal/playbooks[?enabled=false] -> Playbook[] (key, name,
  // description, metric, source, detect_op, detect_threshold, os_filter,
  // fresh_within_seconds, cooldown_seconds, remediate_sh,
  // remediate_powershell, confirm_op, confirm_threshold,
  // remediate_timeout_seconds, confirm_wait_seconds, enabled, updated_at).
  // The engine's declarative rules; 503 when unwired (in-memory server).
  // NOTE: the current server route answers GET only — the create form posts
  // to the same path per the W5-1 contract (a 405 until the server gains the
  // matching branch; the toggle PATCH below is the same story).
  healPlaybooks: (token) => request("/api/heal/playbooks", { token }),

  // POST /api/heal/playbooks { name, metric, detect_op, detect_threshold,
  // confirm_op?, confirm_threshold?, source?, os_filter?, cooldown_seconds?,
  // remediate_sh?, remediate_powershell? } -> 201 the playbook.
  healCreatePlaybook: (token, body) =>
    request("/api/heal/playbooks", { method: "POST", token, body }),

  // PATCH /api/heal/playbooks/{key} { enabled? } -> the updated playbook
  // (the dashboard's enable/disable toggle; 404 unknown key).
  healUpdatePlaybook: (token, key, body) =>
    request(`/api/heal/playbooks/${encodeURIComponent(key)}`, {
      method: "PATCH",
      token,
      body,
    }),

  // GET /api/heal/runs?status=&device_id=&limit= -> Run[] newest first
  // (id, playbook_key, device_id, source, status — detected|verifying|
  // remediating|confirming|resolved|escalated|failed|skipped — reason?,
  // detect_value?, detect_at?, command_id?, dispatched_at?, remediated_at?,
  // confirm_value?, confirmed_at?, escalated_at?, created_at, updated_at).
  healRuns: (token, { status = "", device_id = "", limit = 100 } = {}) => {
    const q = new URLSearchParams();
    if (status) q.set("status", status);
    if (device_id) q.set("device_id", device_id);
    q.set("limit", String(limit));
    return request(`/api/heal/runs?${q.toString()}`, { token });
  },

  // GET /api/heal/runs/{id} -> { run, events[] } where events[] is the
  // run's stage audit trail oldest first ({ id, run_id, status, reason?,
  // at }) — which trigger fired, what was dispatched, what the agent
  // reported, where it ended. 404 unknown run.
  healRun: (token, id) => request(`/api/heal/runs/${id}`, { token }),

  // POST /api/heal/pass -> one synchronous detect + advance pass (the same
  // pass the background loop runs): { detections, started, skipped,
  // confirmed, escalated, failed, active_runs, errors? }. A newly detected
  // series starts a run in `detected`; the same pass then advances every
  // active run one stage, so a fresh heal completes over successive passes.
  healPass: (token) =>
    request("/api/heal/pass", { method: "POST", token, body: {} }),

  // ---- D-4: webhook endpoint management (W3-2) ---------------------------
  // GET /api/webhooks -> Endpoint[] (id, name, url, categories — empty =
  // all; enabled, max_attempts, timeout_ms, last_seq (the delivery cursor —
  // the last seq delivered with a 2xx), attempts (consecutive failures),
  // next_retry_at, status — "ok" | "failing", created_at, updated_at). The
  // HMAC secret is never serialized. 503 when the framework is unwired
  // (in-memory server).
  webhooks: (token) => request("/api/webhooks", { token }),

  // POST /api/webhooks { name, url, secret, categories?, enabled?,
  // max_attempts?, timeout_ms? } -> 201 the endpoint (name/url/secret
  // required; an empty category list subscribes to ALL; the delivery cursor
  // starts at the current journal tail — a new hook gets only NEW events).
  // 400 unknown URL shape; 422 unknown category.
  webhookCreate: (token, body) =>
    request("/api/webhooks", { method: "POST", token, body }),

  // PATCH /api/webhooks/{id} — partial: { name?, url?, categories?,
  // enabled? } -> the updated endpoint. Re-enabling clears a dead-lettered
  // endpoint's failure state (attempts reset, status back to "ok").
  // 404 unknown id.
  webhookUpdate: (token, id, body) =>
    request(`/api/webhooks/${id}`, { method: "PATCH", token, body }),

  // DELETE /api/webhooks/{id} -> 204 (the journaled events it received stay
  // in the global journal). 404 unknown id.
  webhookDelete: (token, id) =>
    request(`/api/webhooks/${id}`, { method: "DELETE", token }),

  // GET /api/webhooks/{id}/events?after=&limit=&category= -> the journaled
  // events this endpoint is subscribed to (seq > after, oldest first,
  // { id (journal seq), category, type, device_id?, at, event }).
  webhookEvents: (
    token,
    id,
    { after = 0, limit = 200, category = "" } = {},
  ) => {
    const q = new URLSearchParams();
    q.set("after", String(after));
    q.set("limit", String(limit));
    if (category) q.set("category", category);
    return request(`/api/webhooks/${id}/events?${q.toString()}`, { token });
  },

  // POST /api/webhooks/{id}/replay { from_seq } -> { webhook_id, from_seq,
  // last_seq, status }: resets the delivery cursor, so the sweeper
  // re-delivers every journaled event from that sequence forward (0 = the
  // whole journal). 400 negative from_seq; 404 unknown id.
  webhookReplay: (token, id, { from_seq = 0 } = {}) =>
    request(`/api/webhooks/${id}/replay`, {
      method: "POST",
      token,
      body: { from_seq },
    }),

  // ---- D-5: baseline anomaly explorer (W2-4) --------------------------------
  // GET /api/baseline/anomalies[?limit=] -> StoredAnomaly[] newest first
  // (id, device_id, name, source, at, value, score — the max z of the fired
  // scoring channels, NOT 0-1 — channel "seasonal"|"trend", seasonal_z?/
  // trend_z?, detected_at). NOTE: the current server honors only `limit`;
  // the device_id/name/min_score params are still sent (per the W2-4
  // contract) and applied client-side until the server filters them.
  baselineAnomalies: (
    token,
    { device_id = "", name = "", min_score = "", limit = 200 } = {},
  ) => {
    const q = new URLSearchParams();
    if (device_id) q.set("device_id", device_id);
    if (name) q.set("name", name);
    if (min_score !== "") q.set("min_score", String(min_score));
    q.set("limit", String(limit));
    return request(`/api/baseline/anomalies?${q.toString()}`, { token });
  },

  // POST /api/baseline/run -> one synchronous scoring pass: { anomalies
  // (baseline.Anomaly[] for this pass, score = max z of the fired channels,
  // seasonal/trend CellScore {z, median, mad, ewma, cells}), series (the
  // number of device-metric series scored), runs }. 503 when the engine is
  // unwired (in-memory server).
  baselineRun: (token, { device_id = "" } = {}) => {
    const body = device_id ? { device_id } : {};
    return request("/api/baseline/run", { method: "POST", token, body });
  },

  // ---- D-6: one-click client export (W4-3) -----------------------------------
  // GET /api/devices/{id}/export -> a ZIP attachment (application/zip) —
  // the client's full bundle: manifest.json (the self-verification
  // contract), device.json, metrics.parquet, metrics_1m.parquet,
  // alerts.json. v1 exports the full history (the server also accepts
  // ?since=RFC3339&until=RFC3339; the UI doesn't offer a range picker yet).
  // Resolves to a Blob — fetch directly, since the JSON `request()` helper
  // would try to parse the ZIP. 400 bad window, 404 unknown device,
  // 503 export unwired (in-memory mode).
  exportDevice: async (token, id, { since = "", until = "" } = {}) => {
    const q = new URLSearchParams();
    if (since) q.set("since", since);
    if (until) q.set("until", until);
    const qs = q.toString();
    const res = await fetch(`/api/devices/${id}/export${qs ? "?" + qs : ""}`, {
      headers: { Authorization: "Bearer " + token },
    });
    if (!res.ok) {
      const message = (await res.text().catch(() => "")) || "export failed";
      const err = new Error(message);
      err.status = res.status;
      if (res.status === 401) err.unauthorized = true;
      throw err;
    }
    return res.blob();
  },

  // ---- gap #4: deep inventory (lane A, wave 3) -----------------------------
  // GET /api/devices/{id}/inventory -> { hardware: {...}, software: [...], collected_at: "..." }
  deviceInventory: (token, id) => request(`/api/devices/${id}/inventory`, { token }),

  // POST /api/devices/{id}/inventory/collect -> { command_id }
  collectDeviceInventory: (token, id) => request(`/api/devices/${id}/inventory/collect`, {
    token,
    method: "POST",
  }),

  // ---- C #10a: settings (wave 1) -------------------------------------------
  // Operator-gated (JWT required) recurring settings surface — the setup
  // wizard's /api/setup/* routes are pre-setup only.
  // GET /api/settings -> { org_name, smtp: {host, port, from, username,
  // pass_set, configured}, profile: {username, mfa_enabled} }. The SMTP
  // password is never returned; pass_set only indicates one is on file.
  getSettings: (token) => request("/api/settings", { token }),

  // PATCH /api/settings { smtp: {host, port, from, username, password} }
  // -> { ok, smtp: <same read model> }. 400 invalid config, 503 in-memory
  // mode (no database). A blank password with a non-empty host keeps the
  // stored password (the read model never returns it); a blank host clears
  // the outbox.
  patchSettings: (token, smtp) =>
    request("/api/settings", { method: "PATCH", token, body: { smtp } }),

  // POST /api/settings/smtp/test { to? } -> { ok, to }: sends via the
  // STORED config (not the draft on screen). 400 bad body, 502 unreachable /
  // refused / not configured, 503 in-memory mode.
  testSMTP: (token, { to = "" } = {}) =>
    request("/api/settings/smtp/test", {
      method: "POST",
      token,
      body: { to: to || undefined },
    }),

  // POST /api/settings/profile/password { current_password, new_password }
  // -> { ok }. 400 wrong current password or new-password length, 503
  // in-memory mode. Once the account has a database row (wizard run or
  // first password change), the old env pair no longer signs in.
  changePassword: (token, { current_password, new_password }) =>
    request("/api/settings/profile/password", {
      method: "POST",
      token,
      body: { current_password, new_password },
    }),

  // ---- MSP clients/tenants (gap #2, wave 1, lane B) -----------------------
  // Every route 503s in in-memory mode (no client registry wired).

  // GET /api/clients -> Client[] {id, name, description, device_count,
  // created_at, updated_at}, name-ordered. Always includes the seeded
  // "Unassigned" default client (id "unassigned") that unassigned devices
  // roll up under.
  clients: (token) => request("/api/clients", { token }),

  // POST /api/clients {name, description?} -> the created client (201,
  // server-minted "clt-…" id). 409 = name already taken (case-insensitive).
  createClient: (token, body) =>
    request("/api/clients", { method: "POST", token, body }),

  // PATCH /api/clients/{id} {name?, description?} -> the updated client.
  // 404 unknown client, 409 new name taken.
  updateClient: (token, id, body) =>
    request(`/api/clients/${id}`, { method: "PATCH", token, body }),

  // GET /api/clients/{id}/devices -> Device[] owned by the client
  // ("unassigned" also returns unassigned devices).
  clientDevices: (token, id) =>
    request(`/api/clients/${id}/devices`, { token }),

  // PATCH /api/devices/{id}/client {client_id: string|null} -> {device}.
  // client_id null (or absent) moves the device to the Unassigned client.
  assignDeviceClient: (token, id, clientId) =>
    request(`/api/devices/${id}/client`, {
      method: "PATCH",
      token,
      body: { client_id: clientId || null },
    }),

  // GET /api/devices?client=<id> — the client-scoped device list. ""
  // returns every device; "unassigned" also matches unassigned devices.
  clientScopedDevices: (token, client = "") => {
    const q = new URLSearchParams();
    if (client) q.set("client", client);
    const qs = q.toString();
    return request(`/api/devices${qs ? `?${qs}` : ""}`, { token });
  },

  // GET /api/alerts?client=<id> — the client-scoped alert inbox (same
  // status/device_id/limit params as alerts()).
  clientScopedAlerts: (token, client, { status = "", limit = 200 } = {}) => {
    const q = new URLSearchParams();
    q.set("client", client);
    if (status) q.set("status", status);
    if (limit) q.set("limit", String(limit));
    return request(`/api/alerts?${q.toString()}`, { token });
  },

  // ---- gap #3: operator accounts, RBAC, MFA, API tokens (wave 2, B) ------

  // POST /api/login with the MFA code (own fetch — the 401 body must stay
  // readable: {error:"mfa_required"} drives the code field in Login.jsx).
  // unauthorized=false on the mfa challenge so the caller doesn't bounce.
  loginFull: async (username, password, totp) => {
    const res = await fetch("/api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username, password, totp }),
    });
    if (res.status === 401) {
      let err = "unauthorized";
      try {
        const j = await res.json();
        if (j && j.error) err = j.error;
      } catch {
        /* ignore */
      }
      throw new ApiError(err, 401, err !== "mfa_required");
    }
    if (!res.ok) throw new ApiError(`HTTP ${res.status}`, res.status);
    return res.json();
  },

  // GET /api/users -> User[] {id, username, role, enabled, totp_enrolled,
  // totp_verified, last_login_at, clients, created_at, updated_at}.
  // 503 = in-memory server (no account model).
  users: (token) => request("/api/users", { token }),

  // POST /api/users {username, password, role, clients?} -> the created
  // user (201, server-minted "usr-…" id). 400 short password / bad role,
  // 409 username exists (case-insensitive).
  createUser: (token, body) =>
    request("/api/users", { method: "POST", token, body }),

  // PATCH /api/users/{id} {role?, enabled?, clients?, password?} -> the
  // updated user. clients []string replaces the grant list (null = keep).
  updateUser: (token, id, body) =>
    request(`/api/users/${id}`, { method: "PATCH", token, body }),

  // POST /api/users/{id}/totp/start -> {secret, uri}. Enrollment begins
  // the moment this returns — from here on the account needs the code.
  startUserTotp: (token, id) =>
    request(`/api/users/${id}/totp/start`, { method: "POST", token }),

  // POST /api/users/{id}/totp/confirm {code} -> {ok}. 401 = wrong code.
  confirmUserTotp: (token, id, code) =>
    request(`/api/users/${id}/totp/confirm`, {
      method: "POST",
      token,
      body: { code },
    }),

  // DELETE /api/users/{id}/totp -> {ok} (2FA reset).
  clearUserTotp: (token, id) =>
    request(`/api/users/${id}/totp`, { method: "DELETE", token }),

  // POST /api/users/{id}/tokens {name, ttl_days?} -> {token, prefix,
  // expires_at}. The full "rmm_…" token appears EXACTLY once (the hash is
  // what's stored) — the UI must show a copy affordance.
  createUserToken: (token, id, body) =>
    request(`/api/users/${id}/tokens`, { method: "POST", token, body }),

  // GET /api/users/{id}/tokens -> [{id, name, prefix, created_at,
  // expires_at, last_used_at}] (never the full token).
  listUserTokens: (token, id) => request(`/api/users/${id}/tokens`, { token }),

  // DELETE /api/users/{id}/tokens/{tid} -> {ok} (revoke; 404 unknown).
  revokeUserToken: (token, id, tid) =>
    request(`/api/users/${id}/tokens/${tid}`, { method: "DELETE", token }),

  // ---- gap #8b: reports (wave 3, lane C) --------------------------------
  // GET /api/reports/schedules -> { schedules: [] }
  reportSchedules: (token) => request("/api/reports/schedules", { token }),

  // POST /api/reports/schedules { name, report_type, schedule, client_id?, output_format? }
  createReportSchedule: (token, body) =>
    request("/api/reports/schedules", { method: "POST", token, body }),

  // DELETE /api/reports/schedules/{id} -> { ok }
  deleteReportSchedule: (token, id) =>
    request(`/api/reports/schedules/${id}`, { method: "DELETE", token }),

  // GET /api/reports/runs?limit= -> { runs: [] }
  reportRuns: (token) => request("/api/reports/runs", { token }),

  // POST /api/reports/generate { report_type, client_id?, device_id?, output_format? }
  generateReport: (token, body) =>
    request("/api/reports/generate", { method: "POST", token, body }),

  // ---- gap #1a: remote session (Lane A owns backend, Lane C owns viewer) ---
  // POST /api/devices/{id}/session/start { fps? } -> { session_id, fps, device_id }
  startSession: (token, deviceID, body) =>
    request(`/api/devices/${deviceID}/session/start`, { method: "POST", token, body }),

  // POST /api/devices/{id}/session/stop { session_id } -> { stopped: true }
  stopSession: (token, deviceID, body) =>
    request(`/api/devices/${deviceID}/session/stop`, { method: "POST", token, body }),

  // SSE stream: GET /api/devices/{id}/session/stream?token=... -> text/event-stream
  // (handled directly by EventSource in SessionViewer.jsx)
};
