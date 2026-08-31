// Metrics-viewer frontend smoke test: drives the REAL <App/> through jsdom
// and proves the per-device metrics definition of done end to end:
//
//   1. expanding a device row shows the Metrics panel with a series picker
//      populated from GET /api/devices/{id}/metrics;
//   2. the chart renders the bucketed series from
//      GET /api/devices/{id}/metrics/series (SVG polyline + now/min/max
//      stats + sample/range/bucket line);
//   3. switching the range selector re-requests the series with the new
//      range (the request carries it);
//   4. picking a per-source series (disk used % of sda1) sends both name
//      AND source in the request.
//
// The fake backend is a small in-process stand-in mirroring the server's
// metrics-viewer semantics (series picker list + bucketed points with
// min/max/last, ascending).
//
// Run: node scripts/metrics.smoke.mjs  (bundles the JSX with esbuild)
import { JSDOM } from "jsdom";

const dom = new JSDOM(
  "<!doctype html><html><body><div id=root></div></body></html>",
  { url: "http://localhost/", pretendToBeVisual: true }
);
globalThis.window = dom.window;
globalThis.document = dom.window.document;
Object.defineProperty(globalThis, "navigator", { value: dom.window.navigator, configurable: true });
globalThis.localStorage = dom.window.localStorage;
globalThis.HTMLElement = dom.window.HTMLElement;
globalThis.Event = dom.window.Event;
globalThis.getComputedStyle = dom.window.getComputedStyle;
globalThis.requestAnimationFrame = (cb) => setTimeout(cb, 0);
globalThis.cancelAnimationFrame = (id) => clearTimeout(id);
globalThis.IS_REACT_ACT_ENVIRONMENT = true;

// jsdom has no EventSource; App (B-1) opens a live stream on sign-in. A
// silent stand-in keeps the stream path exercised without firing events.
class FakeEventSource {
  constructor(url) {
    this.url = url;
    this.readyState = 0;
    setTimeout(() => {
      this.readyState = 1;
      if (this.onopen) this.onopen();
    }, 0);
  }
  close() {
    this.readyState = 2;
  }
}
globalThis.EventSource = FakeEventSource;

// ---- the fake backend (mirrors the server's metrics-viewer semantics) ------
const state = {
  devices: [
    {
      id: "dev-alpha",
      hostname: "alpha-host",
      os: "linux",
      arch: "amd64",
      agent_version: "0.0.0-smoke",
      interfaces: ["10.0.0.11"],
      tags: ["base"],
      online: true,
      first_seen: "2026-08-01T00:00:00Z",
      last_seen: new Date(Date.now() - 5000).toISOString(),
    },
    {
      id: "dev-beta",
      hostname: "beta-host",
      os: "windows",
      arch: "amd64",
      agent_version: "0.0.0-smoke",
      interfaces: ["10.0.0.12"],
      tags: [],
      online: false,
      first_seen: "2026-08-01T00:00:00Z",
      last_seen: new Date(Date.now() - 600000).toISOString(),
    },
  ],
  // (name, source) series per device: the picker list + one shape of
  // bucketed points each (ascending), min/max/last derived like the server.
  series: {
    "dev-alpha": [
      { name: "cpu.utilization_percent", source: "", last: 61.0, count: 144 },
      { name: "disk.used_percent", source: "sda1", last: 78.5, count: 144 },
    ],
    "dev-beta": [], // nothing reported yet -> the empty state
  },
};
const calls = []; // { method, path, body }
function makePoints(seed, n, lo, hi) {
  const now = Date.now();
  const pts = [];
  for (let i = 0; i < n; i++) {
    const v = lo + ((Math.sin(seed + i * 0.7) + 1) / 2) * (hi - lo);
    pts.push([now - (n - 1 - i) * 600000, Number(v.toFixed(2))]);
  }
  return pts;
}
function seriesPayload(deviceId, name, source, range) {
  const dev = state.devices.find((d) => d.id === deviceId);
  if (!dev) return null;
  const list = state.series[deviceId] || [];
  const hit = list.find((m) => m.name === name && m.source === source);
  if (!hit) return null;
  const pts = makePoints(name.length + source.length, 40, 10, 90);
  const vals = pts.map((p) => p[1]);
  return {
    device_id: deviceId,
    name,
    source,
    range,
    bucket_s: 600,
    count: pts.length,
    min: Math.min(...vals),
    max: Math.max(...vals),
    last: vals[vals.length - 1],
    points: pts,
  };
}

async function fakeFetch(path, init = {}) {
  const method = init.method || "GET";
  let body = null;
  if (init.body) {
    try {
      body = JSON.parse(init.body);
    } catch {
      body = init.body;
    }
  }
  calls.push({ method, path, body });
  const json = (obj, status = 200) =>
    new Response(JSON.stringify(obj), { status, headers: { "Content-Type": "application/json" } });
  if (path === "/api/setup/status") return json({ available: true, setup: true });
  if (path === "/api/login") {
    if (body && body.username === "admin" && body.password === "smokepass") {
      return json({ token: "smoke-token", expiry: "2030-01-01T00:00:00Z", capabilities: [] });
    }
    return json({ error: "invalid username or password" }, 401);
  }
  if (path === "/api/devices") return json(state.devices);
  const names = /^\/api\/devices\/([^/]+)\/metrics(\?|$)/.exec(path);
  if (names && method === "GET" && !path.includes("/metrics/series")) {
    const dev = state.devices.find((d) => d.id === decodeURIComponent(names[1]));
    if (!dev) return json({ error: "unknown device" }, 404);
    const q = new URLSearchParams(path.split("?")[1] || "");
    const range = q.get("range") || "7d";
    if (!["1h", "6h", "24h", "7d", "30d"].includes(range)) return json({ error: "bad range" }, 400);
    return json({ device_id: dev.id, range, series: state.series[dev.id] || [] });
  }
  const series = /^\/api\/devices\/([^/]+)\/metrics\/series/.exec(path);
  if (series && method === "GET") {
    const dev = state.devices.find((d) => d.id === decodeURIComponent(series[1]));
    if (!dev) return json({ error: "unknown device" }, 404);
    const q = new URLSearchParams(path.split("?")[1] || "");
    const name = q.get("name") || "";
    if (!name) return json({ error: "name is required" }, 400);
    const range = q.get("range") || "24h";
    if (!["1h", "6h", "24h", "7d", "30d"].includes(range)) return json({ error: "bad range" }, 400);
    const payload = seriesPayload(dev.id, name, q.get("source") || "", range);
    if (!payload) return json({ error: "no such series" }, 404);
    return json(payload);
  }
  if (path === "/api/alerts/counts") return json({ open: 0, acked: 0, resolved: 0 });
  if (path.startsWith("/api/alerts")) return json([]);
  return json({});
}
globalThis.fetch = fakeFetch;

// ---- render the real App ----------------------------------------------------
const React = (await import("react")).default;
const { createRoot } = await import("react-dom/client");
const { act } = await import("react");
const App = (await import("../src/App.jsx")).default;

const container = document.getElementById("root");
const root = createRoot(container);
await act(async () => {
  root.render(React.createElement(App));
});

function waitUntil(cond, what, ms = 5000) {
  return new Promise((resolve, reject) => {
    const t0 = Date.now();
    (function poll() {
      if (cond()) return resolve();
      if (Date.now() - t0 > ms) return reject(new Error(`timeout waiting for: ${what}`));
      setTimeout(poll, 20);
    })();
  });
}
function setVal(el, v) {
  const proto = window.HTMLInputElement.prototype;
  Object.getOwnPropertyDescriptor(proto, "value").set.call(el, v);
  el.dispatchEvent(new window.Event("input", { bubbles: true }));
}
function setSelect(el, v) {
  const proto = window.HTMLSelectElement.prototype;
  Object.getOwnPropertyDescriptor(proto, "value").set.call(el, v);
  el.dispatchEvent(new window.Event("change", { bubbles: true }));
}
const click = (el) =>
  act(async () => {
    el.dispatchEvent(new dom.window.Event("click", { bubbles: true, cancelable: true }));
  });
const rows = () => container.querySelectorAll("table.devices tbody tr:not(.detail-row)");

// ---- 1. sign in through the real login form ---------------------------------
await waitUntil(() => container.textContent.includes("Sign in to continue"), "the login screen");
const form = container.querySelector("form");
const pass = [...container.querySelectorAll("input")].find((i) => i.type === "password");
setVal(pass, "smokepass");
await act(async () => {
  form.dispatchEvent(new dom.window.Event("submit", { bubbles: true, cancelable: true }));
});
await waitUntil(() => rows().length === 2, "the device list (2 devices)");
console.log("ok 1: signed in, device list shows both devices");

// ---- 2. expand alpha -> the Metrics panel renders the chart -----------------
const alpha = [...rows()].find((r) => r.textContent.includes("alpha-host"));
await click(alpha);
await waitUntil(
  () => alpha.nextElementSibling && alpha.nextElementSibling.classList.contains("detail-row"),
  "the detail row for alpha-host"
);
const detail = alpha.nextElementSibling;
await waitUntil(() => detail.querySelector(".device-metrics select"), "the metric picker");
// The picker is populated from /api/devices/{id}/metrics and defaults to
// the host-wide CPU series.
const picker = detail.querySelector(".device-metrics select");
await waitUntil(
  () =>
    picker.options.length === 2 &&
    [...picker.options].some((o) => o.textContent === "cpu.utilization_percent") &&
    [...picker.options].some((o) => o.textContent === "disk.used_percent (sda1)"),
  "the picker lists both series (host-wide CPU + sda1 disk)"
);
if (picker.value !== "0") throw new Error("picker did not default to the CPU series: " + picker.value);
await waitUntil(
  () =>
    detail.querySelector(".metrics-chart polyline") &&
    detail.querySelector(".metrics-chart polyline").getAttribute("points").split(" ").length >= 40,
  "the SVG chart with the bucketed points"
);
await waitUntil(
  () =>
    detail.textContent.includes("now") &&
    detail.textContent.includes("min") &&
    detail.textContent.includes("max") &&
    detail.textContent.includes("24h") &&
    detail.textContent.includes("600s buckets"),
  "the now/min/max stats + range/bucket line"
);
const cpuCall = calls.find((c) =>
  c.path.startsWith("/api/devices/dev-alpha/metrics/series") && c.path.includes("name=cpu.utilization_percent")
);
if (!cpuCall) throw new Error("no series request for the CPU metric seen");
if (!cpuCall.path.includes("range=24h")) throw new Error("default range 24h not sent: " + cpuCall.path);
console.log("ok 2: expanded device shows the Metrics panel — picker from /metrics, SVG chart from /metrics/series with now/min/max stats");

// ---- 3. switch the range to 7d -> the new range is requested ----------------
const rangeSel = [...detail.querySelectorAll(".device-metrics select")].find(
  (s) => [...s.options].some((o) => o.textContent === "7d")
);
if (!rangeSel) throw new Error("range selector not found");
await act(async () => setSelect(rangeSel, "7d"));
await waitUntil(
  () =>
    calls.some(
      (c) => c.path.startsWith("/api/devices/dev-alpha/metrics/series") && c.path.includes("range=7d")
    ),
  "a series request with range=7d"
);
console.log("ok 3: range selector re-requests the series with the new range (range=7d)");

// ---- 4. pick the per-source disk series -> name AND source are sent ---------
await act(async () => setSelect(picker, "1"));
await waitUntil(
  () =>
    calls.some(
      (c) =>
        c.path.startsWith("/api/devices/dev-alpha/metrics/series") &&
        c.path.includes("name=disk.used_percent") &&
        c.path.includes("source=sda1")
    ),
  "a series request with name=disk.used_percent&source=sda1"
);
console.log("ok 4: picking 'disk.used_percent (sda1)' sends both name and source");

// ---- 5. a device with no samples shows the empty state ----------------------
const beta = [...rows()].find((r) => r.textContent.includes("beta-host"));
await click(beta);
await waitUntil(
  () => beta.nextElementSibling && beta.nextElementSibling.classList.contains("detail-row"),
  "the detail row for beta-host"
);
const betaDetail = beta.nextElementSibling;
await waitUntil(
  () => betaDetail.querySelector(".device-metrics") && betaDetail.textContent.includes("No samples"),
  "the empty state for the device with no metric history"
);
console.log("ok 5: a device with no reported series shows the 'no samples in this window' state");

console.log("\nPASS: metrics-viewer UI DoD — series picker, bucketed chart, range switch, per-source series, empty state");
process.exit(0);
