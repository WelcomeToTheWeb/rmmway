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
};
