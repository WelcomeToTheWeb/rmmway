// D-1 command-commands frontend smoke test: drives the REAL <App/> through
// jsdom and proves the command results & history definition of done:
//
//   1. expanding a device row shows the Commands panel seeded from
//      GET /api/devices/{id}/commands — newest first, pending commands as
//      PENDING, a command the agent acked as RUNNING, a finished one as
//      SUCCEEDED;
//   2. clicking a finished row expands the agent's reported output (exit
//      code + stdout tail);
//   3. a command-category envelope on the live SSE stream re-fetches the
//      command list WITHOUT a page refresh — the final result appears in
//      the panel (SSE auto-refresh, not the 5s device poll);
//   4. the manual "↻ refresh" button re-fetches as the fallback.
//
// The fake backend mirrors the server's command-state semantics: pending[]
// is proto-JSON (PascalCase Id/IssuedAtMs/Action oneof), results[] is
// snake_case (command_id, numeric status, stdout_tail, …).
//
// Run: node scripts/commands.smoke.mjs  (bundles the JSX with esbuild)
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
// fake with a shared stream registry (like sse.smoke) lets the test push
// envelopes onto every open session, the way the server's fan-out would.
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
      if (this.readyState === 2) return; // closed
      this.readyState = 1;
      if (this.onopen) this.onopen();
    }, 0);
  }
  close() {
    this.readyState = 2;
    liveStreams.delete(this);
  }
  dispatchMessage(json) {
    const ev = new dom.window.Event("message");
    ev.data = json;
    ev.lastEventId = "";
    if (this.onmessage) this.onmessage(ev);
  }
}
globalThis.EventSource = FakeEventSource;

// ---- the fake backend (mirrors the server's command-state semantics) --------
const T = Date.now();
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
      last_seen: new Date(T - 5000).toISOString(),
    },
  ],
  // pending: dispatched, no FINAL report yet (proto JSON — PascalCase,
  // the Action oneof serializes under its wrapper name).
  pending: [
    {
      Id: "cmd-4",
      IssuedAtMs: T - 10000,
      Action: { RunScript: { Lang: "sh", ScriptB64: "ZWNobyBoaQ==", CapabilityToken: "cap-4" } },
    },
    {
      Id: "cmd-3",
      IssuedAtMs: T - 30000,
      Action: { RunScript: { Lang: "python", ScriptB64: "cHJpbnQoKQ==", CapabilityToken: "cap-3" } },
    },
    {
      Id: "cmd-2",
      IssuedAtMs: T - 60000,
      Action: { Reboot: {} },
    },
  ],
  // results: agent reports (snake_case, numeric status — 2=RUNNING,
  // 3=SUCCEEDED). cmd-2 only got the non-final ack; cmd-1 is finished.
  results: [
    {
      command_id: "cmd-2",
      status: 2,
      completed_at_ms: T - 55000,
    },
    {
      command_id: "cmd-1",
      status: 3,
      exit_code: 0,
      stdout_tail: "initiating clean reboot — see you on the other side",
      completed_at_ms: T - 115000,
    },
  ],
};
const calls = [];
let seq = 0;

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
  const cmds = /^\/api\/devices\/([^/]+)\/commands/.exec(path);
  if (cmds && method === "GET") {
    const dev = state.devices.find((d) => d.id === decodeURIComponent(cmds[1]));
    if (!dev) return json({ error: "unknown device" }, 404);
    return json({ device_id: dev.id, pending: state.pending, results: state.results });
  }
  if (path === "/api/alerts/counts") return json({ open: 0, acked: 0, resolved: 0 });
  if (path.startsWith("/api/alerts")) return json([]);
  return json({});
}
globalThis.fetch = fakeFetch;

// publishEvent pushes one journaled envelope onto every open stream — the
// exact Envelope shape the server's SSE route writes.
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
await waitUntil(() => rows().length === 1, "the device list");
console.log("ok 1: signed in, device list rendered");

// ---- 2. expand alpha -> the Commands panel lists the full history -----------
const alpha = [...rows()].find((r) => r.textContent.includes("alpha-host"));
await click(alpha);
await waitUntil(
  () => alpha.nextElementSibling && alpha.nextElementSibling.classList.contains("detail-row"),
  "the detail row for alpha-host"
);
const detail = alpha.nextElementSibling;
await waitUntil(() => detail.querySelector(".device-commands"), "the Commands panel");
await waitUntil(
  () =>
    [...detail.querySelectorAll(".device-commands tbody tr.cmd-row")].map((r) =>
      r.textContent
    ).join("|").includes("cmd-4") &&
    detail.querySelector(".device-commands").textContent.includes("cmd-3") &&
    detail.querySelector(".device-commands").textContent.includes("cmd-2") &&
    detail.querySelector(".device-commands").textContent.includes("cmd-1"),
  "all four seeded commands in the panel"
);
// Newest first: cmd-4 (10s ago) must be the FIRST row, cmd-1 (oldest) last.
const firstRowId = detail.querySelector(".device-commands tbody tr.cmd-row");
if (!firstRowId || !firstRowId.textContent.includes("cmd-4"))
  throw new Error("newest command (cmd-4) is not the first row");
// Statuses: cmd-4 PENDING (no report), cmd-3 PENDING (no report),
// cmd-2 RUNNING (non-final ack), cmd-1 SUCCEEDED (final).
const statusText = detail.querySelector(".device-commands").textContent;
for (const want of ["PENDING", "RUNNING", "SUCCEEDED"]) {
  if (!statusText.includes(want)) throw new Error(`status ${want} not rendered`);
}
// The action column resolves the oneof (run_script with lang, reboot).
if (!statusText.includes("run_script (sh)")) throw new Error("run_script (sh) action not shown");
if (!statusText.includes("reboot")) throw new Error("reboot action not shown");
console.log("ok 2: Commands panel — newest first, PENDING/RUNNING/SUCCEEDED statuses, action types from the Action oneof");

// ---- 3. expand a finished row -> the agent's reported output is visible -----
const row1 = [...detail.querySelectorAll(".device-commands tbody tr.cmd-row")].find((r) =>
  r.textContent.includes("cmd-1")
);
await click(row1);
await waitUntil(
  () => detail.querySelector(".device-commands .cmd-detail"),
  "the expanded output detail"
);
if (!detail.querySelector(".device-commands").textContent.includes("exit code: 0"))
  throw new Error("exit code not shown in the expanded detail");
if (!detail.querySelector(".device-commands").textContent.includes("initiating clean reboot"))
  throw new Error("stdout tail not shown in the expanded detail");
console.log("ok 3: expanding a finished row reveals the agent's exit code + stdout output");

// ---- 4. a command-category SSE event re-fetches -> the final result appears --
const fetchesBefore = calls.filter((c) => c.path.startsWith("/api/devices/dev-alpha/commands")).length;
// The agent finishes cmd-4: the server records the FINAL result (it leaves
// pending[]) and journals+streams a command-category envelope.
state.pending = state.pending.filter((c) => c.Id !== "cmd-4");
state.results.push({
  command_id: "cmd-4",
  status: 3,
  exit_code: 0,
  stdout_tail: "hello from the smoke script",
  completed_at_ms: Date.now(),
});
publishEvent(
  "command",
  "rmmway.events.command.result",
  "dev-alpha",
  { type: "command.result", command_id: "cmd-4", status: 3, device_id: "dev-alpha" }
);
await waitUntil(
  () =>
    calls.filter((c) => c.path.startsWith("/api/devices/dev-alpha/commands")).length > fetchesBefore,
  "a re-fetch of the command list after the SSE event"
);
await waitUntil(
  () => {
    const row = [...detail.querySelectorAll(".device-commands tbody tr.cmd-row")].find((r) =>
      r.textContent.includes("cmd-4")
    );
    return row && row.textContent.includes("SUCCEEDED");
  },
  "cmd-4's row flipping to SUCCEEDED (no page refresh)"
);
const row4 = [...detail.querySelectorAll(".device-commands tbody tr.cmd-row")].find((r) =>
  r.textContent.includes("cmd-4")
);
await click(row4);
await waitUntil(
  () => detail.querySelector(".device-commands").textContent.includes("hello from the smoke script"),
  "cmd-4's final output in the panel (no page refresh)"
);
console.log("ok 4: a command result on the SSE stream re-fetched the list and the row flipped PENDING -> SUCCEEDED live, output on expand");

// ---- 5. the manual refresh button re-fetches (fallback) ---------------------
const afterSse = calls.filter((c) => c.path.startsWith("/api/devices/dev-alpha/commands")).length;
const refresh = [...detail.querySelectorAll(".device-commands button")].find((b) =>
  b.textContent.includes("refresh")
);
if (!refresh) throw new Error("manual refresh button not found");
await click(refresh);
await waitUntil(
  () => calls.filter((c) => c.path.startsWith("/api/devices/dev-alpha/commands")).length > afterSse,
  "a re-fetch after clicking the manual refresh button"
);
console.log("ok 5: the manual '↻ refresh' button re-fetches the command list");

console.log("\nPASS: D-1 command results & history UI DoD — newest-first history, expandable agent output, SSE auto-refresh, manual refresh fallback");
process.exit(0);
