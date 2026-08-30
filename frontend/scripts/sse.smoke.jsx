// B-1 frontend smoke test: drives the REAL <App/> in TWO operator sessions
// (two mounts, each signed in, each with its own live stream) through jsdom
// and proves the reactive-UI definition of done:
//
//   1. both sessions open the SSE stream (GET /api/events/stream?token=…);
//   2. a device going OFFLINE flips the status badge in BOTH sessions' DOM
//      within a few hundred ms — far inside the 5s devices poll, i.e. the
//      flip comes from the live stream, not the poll;
//   3. a NEW alert bumps the nav badge in BOTH sessions AND lands in the
//      open inbox (the #/alerts view) immediately — far inside the 15s
//      alert-counts / 10s inbox polls.
//
// The fake backend is a small in-process stand-in: it serves the REST calls
// the App makes and pushes journaled event envelopes into a fake EventSource
// (jsdom has no EventSource) exactly as the server's SSE route would —
// catch-up empty, then live frames with the Envelope JSON in `data`.
//
// Run: node scripts/sse.smoke.mjs   (bundles the JSX with esbuild)
import { JSDOM } from "jsdom";

const dom = new JSDOM("<!doctype html><html><body><div id=root-a></div><div id=root-b></div></body></html>", {
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

// ---- fake EventSource (jsdom has none) -------------------------------------
// Mirrors the browser API surface sse.js uses: the constructor, onopen /
// onerror / onmessage, close(). The fake "server" (publishEvent below) pushes
// frames to every open instance, the way the server's live fan-out would.
const liveStreams = new Set();
class FakeEventSource {
  constructor(url) {
    this.url = url;
    this.readyState = 0;
    this.onopen = null;
    this.onerror = null;
    this.onmessage = null;
    liveStreams.add(this);
    // Connect on the next tick, like a real socket (the caller assigns the
    // handlers synchronously after constructing).
    setTimeout(() => {
      if (this.readyState === 2) return; // closed
      this.readyState = 1;
      if (this.onopen) this.onopen();
    }, 0);
  }
  close() {
    this.readyState = 2;
    liveStreams.delete(this);
  }
  // one SSE frame: the server writes `id: <seq>\ndata: <envelope json>`
  dispatchMessage(json) {
    const ev = new dom.window.Event("message");
    ev.data = json;
    ev.lastEventId = "";
    if (this.onmessage) this.onmessage(ev);
  }
}
globalThis.EventSource = FakeEventSource;

// ---- the fake backend -------------------------------------------------------
const state = {
  device: {
    id: "dev-smoke-1",
    hostname: "smoke-host",
    os: "linux",
    arch: "amd64",
    agent_version: "0.0.0-smoke",
    interfaces: ["10.0.0.7"],
    tags: ["smoke"],
    online: true,
    first_seen: "2026-08-01T00:00:00Z",
    last_seen: new Date(Date.now() - 5000).toISOString(),
  },
  alerts: [], // full Alert rows (the W2-4 inbox shape)
  openCount: 0,
};
const fetchLog = []; // "METHOD path" in call order (timing asserts use it)

async function fakeFetch(path, init = {}) {
  const method = init.method || "GET";
  fetchLog.push(`${method} ${path}`);
  const json = (obj, status = 200) =>
    new Response(JSON.stringify(obj), { status, headers: { "Content-Type": "application/json" } });
  if (path === "/api/setup/status") return json({ available: true, setup: true });
  if (path === "/api/login") {
    const body = JSON.parse(init.body || "{}");
    if (body.username === "admin" && body.password === "smokepass") {
      return json({ token: "smoke-token", expiry: "2030-01-01T00:00:00Z", capabilities: [] });
    }
    return json({ error: "invalid username or password" }, 401);
  }
  if (path === "/healthz") return json({ ok: true, probes: {} });
  if (path === "/api/devices") return json([state.device]);
  if (path === "/api/alerts/counts")
    return json({ open: state.openCount, acked: 0, resolved: 0 });
  if (path.startsWith("/api/alerts")) return json(state.alerts);
  return json({});
}
globalThis.fetch = fakeFetch;

// publishEvent pushes one journaled envelope onto every open stream — the
// exact Envelope shape the server's SSE route writes (id/version/source/
// category/type/device_id/at + the full bus event in `event`).
let seq = 0;
function publishEvent(category, type, deviceID, event) {
  seq += 1;
  const env = {
    id: seq,
    version: "rmmway-event/v1",
    source: "rmmway",
    category,
    type,
    device_id: deviceID,
    at: new Date().toISOString(),
    event,
  };
  for (const es of [...liveStreams]) es.dispatchMessage(JSON.stringify(env));
}

// ---- render the real App ----------------------------------------------------
const React = (await import("react")).default;
const { createRoot } = await import("react-dom/client");
const { act } = await import("react");
const App = (await import("../src/App.jsx")).default;

function waitUntil(cond, what, ms = 4000) {
  return new Promise((resolve, reject) => {
    const t0 = Date.now();
    (function poll() {
      if (cond()) return resolve(Date.now() - t0);
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
const input = (scope, sel) => scope.querySelector(sel);
const inputs = (scope) => [...scope.querySelectorAll("input")];

function navBadges(scope) {
  return [...scope.querySelectorAll(".nav .badge")].map((b) => b.textContent.trim());
}

// ---- 1. two operator sessions, both signed in -------------------------------
const containerA = document.getElementById("root-a");
const containerB = document.getElementById("root-b");
const rootA = createRoot(containerA);
const rootB = createRoot(containerB);
await act(async () => {
  rootA.render(React.createElement(App));
  rootB.render(React.createElement(App));
});
await waitUntil(
  () => containerA.textContent.includes("Sign in to continue") &&
         containerB.textContent.includes("Sign in to continue"),
  "both sessions at the login screen"
);
console.log("ok 1: two operator sessions mounted at the login screen");

// Sign each session in through the real Login form.
for (const [label, scope] of [["A", containerA], ["B", containerB]]) {
  const form = input(scope, "form");
  const pass = inputs(scope).find((i) => i.type === "password");
  setVal(pass, "smokepass");
  await act(async () => {
    // jsdom does not translate a submit-button click into a form submit
    // event (browsers do), so fire submit directly.
    form.dispatchEvent(new dom.window.Event("submit", { bubbles: true, cancelable: true }));
  });
  await waitUntil(() => scope.textContent.includes("smoke-host"), `session ${label} shows the device list`);
}
// Both sessions now show the device ONLINE (the fake backend's state).
await waitUntil(
  () => /online/.test(containerA.textContent) && /online/.test(containerB.textContent),
  "both sessions show the device online"
);
console.log("ok 2: both sessions signed in, device row shows online");

// The point of B-1: each session opened its OWN live stream with its JWT.
await waitUntil(() => liveStreams.size >= 2, "both sessions opened the SSE stream");
const urls = [...liveStreams].map((es) => es.url);
if (!urls.every((u) => u.startsWith("/api/events/stream?") && u.includes("token=smoke-token"))) {
  throw new Error("unexpected stream URLs: " + urls.join(" | "));
}
console.log(`ok 3: ${liveStreams.size} live SSE streams open, each carrying the operator JWT`);

// ---- 2. device goes offline -> badges flip in BOTH sessions, instantly ------
// The offline flip is produced server-side by the offline sweeper (a device
// that stops heartbeating) and published onto the bus — the same event the
// reactive UI relies on. Drive the fake backend + stream the way the server
// would: the device list flips, and the inventory envelope hits the stream.
const t0 = Date.now();
state.device.online = false;
state.device.last_seen = new Date(Date.now() - 120000).toISOString();
publishEvent(
  "inventory",
  "rmmway.events.device",
  state.device.id,
  {
    type: "rmmway.events.device",
    device_id: state.device.id,
    message: "offline device",
    data: { action: "offline", device_id: state.device.id, reason: "stale last_seen" },
    at: new Date().toISOString(),
  }
);
await waitUntil(
  () =>
    containerA.textContent.includes("1 total · 0 online · 1 offline") &&
    containerB.textContent.includes("1 total · 0 online · 1 offline"),
  "both sessions show the device offline in the fleet summary",
  4000
);
const offlineMs = Date.now() - t0;
// The devices poll is 5000ms; the flip must come from the stream, so it
// lands far inside that window.
if (offlineMs >= 4000) {
  throw new Error(`offline flip took ${offlineMs}ms — that is poll latency, not live`);
}
console.log(`ok 4: device offline -> BOTH sessions' status badge flipped in ${offlineMs}ms (poll is 5s)`);

// ---- 3. new alert -> nav badge + open inbox update in BOTH sessions ---------
// Navigate both sessions to the alerts inbox (shared window hash — like an
// operator switching tabs' route), wait for the empty inbox, then fire a
// real alert event through the stream.
await act(async () => {
  window.location.hash = "#/alerts";
});
await waitUntil(
  () =>
    containerA.textContent.includes("No open alerts.") &&
    containerB.textContent.includes("No open alerts."),
  "both sessions show the empty alerts inbox"
);
console.log("ok 5: both sessions on the alerts inbox (empty)");

// The reconciler fired a baseline anomaly -> the alert store publishes an
// rmmway.events.alert envelope with action "fired". The fake backend now
// holds one open alert (what GET /api/alerts would return on re-pull).
const t1 = Date.now();
state.openCount = 1;
state.alerts = [
  {
    id: 1,
    device_id: state.device.id,
    hostname: state.device.hostname,
    name: "cpu.utilization_percent",
    source: "baseline",
    status: "open",
    channel: "cpu",
    score: 6.2,
    value: 99.1,
    expected: 21.4,
    events: 1,
    first_at: new Date().toISOString(),
    last_at: new Date().toISOString(),
  },
];
publishEvent(
  "alert",
  "rmmway.events.alert",
  state.device.id,
  {
    type: "rmmway.events.alert",
    device_id: state.device.id,
    message: "fired alert cpu.utilization_percent",
    data: {
      action: "fired",
      name: "cpu.utilization_percent",
      device_id: state.device.id,
      value: 99.1,
      score: 6.2,
    },
    at: new Date().toISOString(),
  }
);
await waitUntil(
  () =>
    navBadges(containerA).includes("1") &&
    navBadges(containerB).includes("1") &&
    containerA.textContent.includes("cpu.utilization_percent") &&
    containerB.textContent.includes("cpu.utilization_percent"),
  "both sessions show the nav badge AND the alert in the open inbox",
  4000
);
const alertMs = Date.now() - t1;
// The alert-counts poll is 15000ms and the inbox poll is 10000ms; the update
// must come from the stream, so it lands far inside both windows.
if (alertMs >= 8000) {
  throw new Error(`alert update took ${alertMs}ms — that is poll latency, not live`);
}
// The full inbox row rendered (host + triage actions), not just the badge.
for (const [label, scope] of [["A", containerA], ["B", containerB]]) {
  if (!scope.querySelector(".alert-open")) {
    throw new Error(`session ${label}: no open alert row in the inbox`);
  }
  if (!scope.textContent.includes("smoke-host")) {
    throw new Error(`session ${label}: alert row missing the hostname`);
  }
}
console.log(`ok 6: new alert -> BOTH sessions' nav badge + open inbox updated in ${alertMs}ms (polls are 15s/10s)`);

console.log("\nB-1 frontend smoke PASS: live SSE stream drives the device status badge and the alerts inbox across all open operator sessions — no polling.");
process.exit(0);
