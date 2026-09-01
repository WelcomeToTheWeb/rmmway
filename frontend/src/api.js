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
    } catch { /* ignore */ }
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
  testSmtp: (body) =>
    request("/api/setup/smtp/test", { method: "POST", body }),

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
      { token }
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
  eventJournal: (token, { after = 0, limit = 200, category = "", device = "", type = "" } = {}) => {
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
      { token }
    );
  },

  // Per-device metrics viewer: the (name, source) series the device has
  // reported over the window (the chart's metric picker). -> { device_id,
  // range, series: [{name, source, last, count}] }.
  metricsNames: (token, deviceId, range = "7d") =>
    request(
      `/api/devices/${encodeURIComponent(deviceId)}/metrics?range=${range}`,
      { token }
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
      { token }
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
  healPass: (token) => request("/api/heal/pass", { method: "POST", token, body: {} }),
};
