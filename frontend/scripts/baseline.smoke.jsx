// D-5 baseline anomaly explorer smoke test: drives the REAL <App/> in
// jsdom against a fake baseline backend, and proves the definition of
// done:
//
//   1. the header nav carries a 7th item "Baseline" -> #/baseline;
//   2. the table shows the current anomaly landscape sorted worst-first
//      (the 12.8σ reading leads);
//   3. filtering min score >= 4 narrows the view to the 3 significant
//      deviations (the query carries min_score=4);
//   4. the device filter narrows to that device's series (device_id in
//      the query);
//   5. "Recompute" (POST /api/baseline/run) shows the working state and,
//      when the pass completes, reports the pass summary and the table
//      reflects the freshly scored landscape (the new 14.2σ leads);
//   6. clicking a device hostname jumps to that device's detail page.
//
// Run: node scripts/baseline.smoke.mjs   (bundles the JSX with esbuild)
import { JSDOM } from "jsdom";

const dom = new JSDOM("<!doctype html><html><body><div id=root></div></body></html>", {
  url: "http://localhost/",
  pretendToBeVisual: true,
});
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

const liveStreams = new Set();
class FakeEventSource {
  constructor(url) {
    this.url = url;
    this.readyState = 0;
    this.onopen = null;
    this.onerror = null;
    this.onmessage = null;
    liveStreams.add(this);
    setTimeout(() => {
      if (this.readyState === 2) return;
      this.readyState = 1;
      if (this.onopen) this.onopen();
    }, 0);
  }
  close() {
    this.readyState = 2;
    liveStreams.delete(this);
  }
}
globalThis.EventSource = FakeEventSource;

// ---- the fake baseline store -------------------------------------------------
const DEVICES = [
  { id: "dev-web-01", hostname: "web-01", os: "linux", arch: "amd64", agent_version: "0.1.0", interfaces: ["10.0.0.11"], tags: ["web"], online: true, first_seen: "2026-07-01T00:00:00Z", last_seen: "2026-08-25T11:59:00Z" },
  { id: "dev-fs-01", hostname: "fileserver-01", os: "linux", arch: "amd64", agent_version: "0.1.0", interfaces: ["10.0.0.21"], tags: ["files"], online: true, first_seen: "2026-07-01T00:00:00Z", last_seen: "2026-08-25T11:59:00Z" },
];

const ANOMALIES = [
  { id: 1, device_id: "dev-web-01", name: "cpu.utilization_percent", source: "system", at: "2026-08-25T11:40:00Z", value: 97.4, score: 12.8, channel: "seasonal", seasonal_z: 12.8, detected_at: "2026-08-25T11:41:00Z" },
  { id: 2, device_id: "dev-fs-01", name: "disk.used_percent", source: "system", at: "2026-08-25T11:35:00Z", value: 95.8, score: 8.9, channel: "seasonal", seasonal_z: 8.9, detected_at: "2026-08-25T11:36:00Z" },
  { id: 3, device_id: "dev-web-01", name: "mem.used_percent", source: "system", at: "2026-08-25T11:30:00Z", value: 93.1, score: 4.2, channel: "trend", trend_z: 4.2, detected_at: "2026-08-25T11:31:00Z" },
  { id: 4, device_id: "dev-web-01", name: "fan.rpm", source: "system", at: "2026-08-25T11:20:00Z", value: 0, score: 2.4, channel: "trend", trend_z: 2.4, detected_at: "2026-08-25T11:21:00Z" },
  { id: 5, device_id: "dev-fs-01", name: "net.rx_bytes", source: "system", at: "2026-08-25T11:10:00Z", value: 512000000, score: 1.5, channel: "trend", trend_z: 1.5, detected_at: "2026-08-25T11:11:00Z" },
  { id: 6, device_id: "dev-fs-01", name: "cpu.utilization_percent", source: "system", at: "2026-08-25T11:00:00Z", value: 48.2, score: 0.8, channel: "seasonal", seasonal_z: 0.8, detected_at: "2026-08-25T11:01:00Z" },
];
let nextId = 7;

const fetchLog = [];
async function fakeFetch(path, init = {}) {
  const method = init.method || "GET";
  fetchLog.push(`${method} ${path}`);
  const json = (obj, status = 200) =>
    new Response(JSON.stringify(obj), { status, headers: { "Content-Type": "application/json" } });
  if (path === "/api/setup/status") return json({ available: true, setup: true });
  if (path === "/api/login") {
    const b = JSON.parse(init.body || "{}");
    if (b.username === "admin" && b.password === "smokepass") {
      return json({ token: "smoke-token", expiry: "2030-01-01T00:00:00Z", capabilities: [] });
    }
    return json({ error: "invalid username or password" }, 401);
  }
  if (path === "/healthz") return json({ ok: true, probes: {} });
  if (path === "/api/devices") return json(DEVICES);
  if (path === "/api/alerts/counts") return json({ open: 1, acked: 0, resolved: 0 });
  if (path.startsWith("/api/alerts")) return json([]);

  if (path.startsWith("/api/baseline/anomalies")) {
    // The current server honors only limit — return the stored landscape,
    // newest first (the UI re-sorts by severity).
    return json(ANOMALIES.map((a) => ({ ...a })));
  }
  if (path === "/api/baseline/run") {
    if (method !== "POST") return json({ error: "POST only" }, 405);
    // a real scoring pass takes a moment — hold long enough that the UI's
    // "working" state is a distinct, observable render before it resolves.
    await new Promise((r) => setTimeout(r, 30));
    const now = "2026-08-25T11:55:00Z";
    const fresh = [
      { id: nextId++, device_id: "dev-web-01", name: "cpu.utilization_percent", source: "system", at: now, value: 99.8, score: 14.2, channel: "seasonal", seasonal_z: 14.2, detected_at: "2026-08-25T11:56:00Z" },
      { id: nextId++, device_id: "dev-fs-01", name: "disk.used_percent", source: "system", at: now, value: 97.1, score: 9.5, channel: "seasonal", seasonal_z: 9.5, detected_at: "2026-08-25T11:56:00Z" },
    ];
    ANOMALIES.unshift(...fresh);
    const cell = (z) => ({ z, median: 50, mad: 2.1, ewma: 51, cells: 1260 });
    return json({
      anomalies: [
        { at: now, device_id: "dev-web-01", name: "cpu.utilization_percent", source: "system", value: 99.8, seasonal: cell(14.2), score: 14.2 },
        { at: now, device_id: "dev-fs-01", name: "disk.used_percent", source: "system", value: 97.1, seasonal: cell(9.5), score: 9.5 },
      ],
      series: 24,
      runs: 2,
    });
  }
  return json({});
}
globalThis.fetch = fakeFetch;

// ---- render the real App -----------------------------------------------------
const React = (await import("react")).default;
const { createRoot } = await import("react-dom/client");
const { act } = await import("react");
const App = (await import("../src/App.jsx")).default;

const container = document.getElementById("root");
const root = createRoot(container);
await act(async () => {
  root.render(React.createElement(App));
});

const text = () => container.textContent;
function waitUntil(cond, what, ms = 6000) {
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
  const proto = el.tagName === "TEXTAREA"
    ? window.HTMLTextAreaElement.prototype
    : window.HTMLInputElement.prototype;
  Object.getOwnPropertyDescriptor(proto, "value").set.call(el, v);
  el.dispatchEvent(new window.Event("input", { bubbles: true }));
}
const btn = (re) => [...container.querySelectorAll("button")].find((b) => re.test(b.textContent));
const rows = () => [...container.querySelectorAll("tr.baseline-row")];
const topScore = () => {
  const first = rows()[0];
  if (!first) return null;
  const m = first.querySelector(".pill.score")?.textContent.match(/([\d.]+)σ/);
  return m ? Number(m[1]) : null;
};
const lastAnomaliesCall = () =>
  [...fetchLog].reverse().find((c) => c.startsWith("GET /api/baseline/anomalies")) || "";

// ---- 1. sign in; the header nav carries "Baseline" 7th -----------------------
await waitUntil(() => text().includes("Sign in to continue"), "login screen");
await act(async () => {
  setVal(container.querySelector('input[type="password"]'), "smokepass");
  container.querySelector("form").dispatchEvent(
    new window.Event("submit", { bubbles: true, cancelable: true })
  );
});
await waitUntil(() => text().includes("web-01") && text().includes("fileserver-01"), "devices view");
const navHrefs = [...container.querySelectorAll("nav.nav a")].map((a) => a.getAttribute("href"));
if (navHrefs.slice(0, 7).join(",") !== "#/devices,#/alerts,#/flows,#/events,#/heal,#/webhooks,#/baseline") {
  throw new Error("nav order is wrong (want the 7 items in order): " + navHrefs.join(","));
}
console.log("ok 1: the header nav carries Baseline as the 7th item, after Webhooks");

// ---- 2. the landscape renders sorted worst-first ------------------------------
await act(async () => {
  window.location.hash = "#/baseline";
});
await waitUntil(() => rows().length === 6, "six anomaly rows");
if (topScore() !== 12.8) {
  throw new Error(`the table should lead with the worst reading (12.8), got ${topScore()}`);
}
if (!text().includes("cpu.utilization_percent")) throw new Error("metric names are not rendered");
console.log("ok 2: the anomaly landscape renders sorted worst-first (12.8σ cpu on web-01 leads)");

// ---- 3. min score filter: only significant deviations -------------------------
await act(async () => {
  setVal(container.querySelector('label.check input[type="number"]'), "4");
});
await waitUntil(() => lastAnomaliesCall().includes("min_score=4"), "the anomalies query carrying min_score=4");
await waitUntil(() => rows().length === 3, "three rows at min score 4");
const scores = rows().map((r) => r.querySelector(".pill.score").textContent);
if (!/12\.8/.test(scores[0]) || !/8\.9/.test(scores[1]) || !/4\.2/.test(scores[2])) {
  throw new Error(`min-score filtering kept the wrong rows: ${scores.join(",")}`);
}
console.log("ok 3: min score >= 4 narrows the view to the 3 significant deviations (12.8, 8.9, 4.2), worst first, with min_score=4 in the query");

// ---- 4. the device filter narrows to that device's series ---------------------
await act(async () => {
  const sel = container.querySelector(".baseline-filters select");
  const proto = window.HTMLSelectElement.prototype;
  Object.getOwnPropertyDescriptor(proto, "value").set.call(sel, "dev-web-01");
  sel.dispatchEvent(new window.Event("change", { bubbles: true }));
});
await waitUntil(() => lastAnomaliesCall().includes("device_id=dev-web-01"), "the anomalies query carrying device_id");
await waitUntil(() => rows().length === 2, "only web-01's rows above the threshold");
if (!rows().every((r) => r.textContent.includes("web-01"))) {
  throw new Error("the device filter leaked another device's row");
}
console.log("ok 4: the device filter narrows the view to that device's series (device_id in the query)");

// ---- 5. Recompute: working state, pass summary, refreshed landscape -----------
await act(async () => {
  btn(/^recompute$/i).dispatchEvent(new window.MouseEvent("click", { bubbles: true }));
});
await waitUntil(() => text().includes("Scoring fleet…"), "the recompute working state");
await waitUntil(
  () => fetchLog.some((c) => c === "POST /api/baseline/run") && text().includes("24 series"),
  "the pass summary after recompute"
);
await waitUntil(() => topScore() === 14.2, "the refreshed landscape leading with 14.2");
if (!text().includes("2 anomalies")) throw new Error("the pass summary is missing the anomaly count");
console.log("ok 5: Recompute POSTs /api/baseline/run, shows the working state, reports the pass (2 anomalies across 24 series) and the table now leads with the fresh 14.2σ reading");

// ---- 6. clicking a hostname jumps to the device detail ------------------------
await act(async () => {
  container.querySelector("a.baseline-devlink").dispatchEvent(
    new window.MouseEvent("click", { bubbles: true })
  );
});
await waitUntil(() => window.location.hash.startsWith("#/devices"), "navigation to the devices view");
await waitUntil(() => text().includes("web-01"), "the device detail for web-01");
console.log("ok 6: clicking a device hostname jumps to that device's detail page");

for (const es of [...liveStreams]) es.close();
console.log("\nD-5 baseline explorer UI DoD PASS: nav item, worst-first landscape, min-score filter, device filter, recompute with summary + refreshed scores, hostname -> device detail.");
process.exit(0);
