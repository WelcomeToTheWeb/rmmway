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
  // POST /api/login -> { token, expiry }
  login: (username, password) =>
    request("/api/login", { method: "POST", body: { username, password } }),

  // GET /api/devices -> Device[] (each has id, hostname, os, arch,
  // agent_version, interfaces[], tags[], online, first_seen, last_seen)
  devices: (token) => request("/api/devices", { token }),

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
};
