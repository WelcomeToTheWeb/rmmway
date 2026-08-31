// B-2 frontend smoke test: drives the REAL <App/> through jsdom and proves
// the dynamic-device-grouping definition of done end to end:
//
//   1. the operator tags devices through the per-device tag editor
//      (PATCH /api/devices/{id} — the whole tag list each time);
//   2. the filter box's `tag:web` switches the device list to the EXACT
//      tag group (only the tagged devices remain);
//   3. "Dispatch to group" fires ONE bulk command
//      (POST /api/devices/bulk/commands) — the request carries the tag, the
//      action and the base64 script; the result panel reports matched /
//      pushed / offline from the server's fan-out.
//
// The fake backend is a small in-process stand-in mirroring the server's
// B-2 semantics (tag normalization, per-tag group, pushed/offline split).
//
// Run: node scripts/groups.smoke.mjs   (bundles the JSX with esbuild)
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

// ---- the fake backend (mirrors the server's B-2 semantics) -----------------
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
      online: false, // the fan-out must report it offline, not fake it
      first_seen: "2026-08-01T00:00:00Z",
      last_seen: new Date(Date.now() - 600000).toISOString(),
    },
    {
      id: "dev-gamma",
      hostname: "gamma-host",
      os: "linux",
      arch: "arm64",
      agent_version: "0.0.0-smoke",
      interfaces: ["10.0.0.13"],
      tags: ["other"],
      online: true,
      first_seen: "2026-08-01T00:00:00Z",
      last_seen: new Date(Date.now() - 5000).toISOString(),
    },
  ],
};
const calls = []; // { method, path, body }
function normalizeTags(inTags) {
  const out = [];
  for (const raw of inTags || []) {
    const t = String(raw).trim().toLowerCase();
    if (t && !out.includes(t)) out.push(t);
  }
  return out;
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
  const patch = /^\/api\/devices\/([^/]+)$/.exec(path);
  if (patch && method === "PATCH") {
    const dev = state.devices.find((d) => d.id === decodeURIComponent(patch[1]));
    if (!dev) return json({ error: "unknown device" }, 404);
    dev.tags = normalizeTags(body && body.tags);
    return json({ device: { ...dev }, indexed: true });
  }
  if (path === "/api/devices/bulk/commands" && method === "POST") {
    const tag = String((body && body.tag) || "").trim().toLowerCase();
    if (!tag) return json({ error: "tag is required" }, 400);
    const group = state.devices.filter((d) => d.tags.includes(tag));
    if (group.length === 0) return json({ error: `no devices carry tag ${tag}` }, 404);
    const pushed = group.filter((d) => d.online).map((d, i) => ({ device_id: d.id, command_id: `cmd-b2-${i}` }));
    const offline = group.filter((d) => !d.online).map((d) => d.id);
    return json({ tag, requested: group.length, pushed, offline, failed: {} });
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
const click = (el) =>
  act(async () => {
    el.dispatchEvent(new dom.window.Event("click", { bubbles: true, cancelable: true }));
  });
const btn = (scope, text) =>
  [...scope.querySelectorAll("button")].find((b) => b.textContent.includes(text));
const rows = () => container.querySelectorAll("table.devices tbody tr:not(.detail-row)");

// ---- 1. sign in through the real login form ---------------------------------
await waitUntil(() => container.textContent.includes("Sign in to continue"), "the login screen");
const form = container.querySelector("form");
const pass = [...container.querySelectorAll("input")].find((i) => i.type === "password");
setVal(pass, "smokepass");
await act(async () => {
  form.dispatchEvent(new dom.window.Event("submit", { bubbles: true, cancelable: true }));
});
await waitUntil(() => rows().length === 3, "the device list (3 devices)");
console.log("ok 1: signed in, device list shows all 3 devices");

// ---- 2. tag editor: add "web" to alpha and beta ------------------------------
async function expandAndTag(hostname, tag) {
  const row = [...container.querySelectorAll("table.devices tbody tr:not(.detail-row)")].find(
    (r) => r.textContent.includes(hostname)
  );
  await click(row);
  await waitUntil(
    () => row.nextElementSibling && row.nextElementSibling.classList.contains("detail-row"),
    `the detail row for ${hostname}`
  );
  const detail = row.nextElementSibling;
  const input = detail.querySelector('input[placeholder^="add a tag"]');
  if (!input) throw new Error(`tag editor not shown for ${hostname}`);
  setVal(input, tag);
  await click(btn(detail, "+ add"));
}
await expandAndTag("alpha-host", "web");
await waitUntil(
  () => calls.some((c) => c.path === "/api/devices/dev-alpha" && c.method === "PATCH" &&
    JSON.stringify(c.body.tags) === JSON.stringify(["base", "web"])),
  "PATCH dev-alpha with tags [base, web]"
);
await waitUntil(() => container.textContent.includes("web"), "alpha's new tag chip");
console.log("ok 2: tag editor added 'web' to alpha (PATCH with the full tag list)");

await expandAndTag("beta-host", "web");
await waitUntil(
  () => calls.some((c) => c.path === "/api/devices/dev-beta" && c.method === "PATCH" &&
    JSON.stringify(c.body.tags) === JSON.stringify(["web"])),
  "PATCH dev-beta with tags [web]"
);
console.log("ok 3: tag editor added 'web' to beta (PATCH with the full tag list)");

// ---- 3. `tag:web` filters the list to the exact group ------------------------
const filter = [...container.querySelectorAll("input[type=search]")].find(
  (i) => i.placeholder && i.placeholder.includes("tag:")
);
setVal(filter, "tag:web");
await waitUntil(
  () => {
    const rs = rows();
    return rs.length === 2 && rs[0].textContent.includes("alpha-host") && rs[1].textContent.includes("beta-host");
  },
  "the tag:web filter shows exactly alpha + beta"
);
console.log("ok 4: `tag:web` in the filter box narrowed the list to the exact group (2 of 3)");

// ---- 4. ONE command fans out to the whole group ------------------------------
await click(btn(container, "Dispatch to group"));
await waitUntil(() => container.querySelector(".modal.bulk"), "the group dispatch modal");
const modal = container.querySelector(".modal.bulk");
const tagField = modal.querySelector(".bulk-tag");
if (tagField.value !== "web") {
  throw new Error(`expected the modal's tag pre-filled from the filter, got "${tagField.value}"`);
}
// Default action is run_script with the default script; submit as-is.
await click(btn(modal, "Dispatch to whole group"));
const bulkCall = calls.find((c) => c.path === "/api/devices/bulk/commands");
if (!bulkCall) throw new Error("no POST /api/devices/bulk/commands seen");
if (bulkCall.body.tag !== "web" || bulkCall.body.action !== "run_script" || bulkCall.body.lang !== "sh") {
  throw new Error("unexpected bulk body: " + JSON.stringify(bulkCall.body));
}
const script = atob(bulkCall.body.script);
if (!script.includes("RMMWay group script")) {
  throw new Error("bulk script payload is not the editor's script (base64): " + script);
}
await waitUntil(
  () => modal.textContent.includes("2 matched") &&
        modal.textContent.includes("1 pushed") &&
        modal.textContent.includes("1 offline"),
  "the fan-out result panel (2 matched · 1 pushed · 1 offline)"
);
if (!modal.textContent.includes("dev-alpha") || !/cmd-b2-0/.test(modal.textContent)) {
  throw new Error("result panel missing the pushed device + command id: " + modal.textContent);
}
if (!/offline \(no live stream\): dev-beta/.test(modal.textContent)) {
  throw new Error("result panel missing the offline report: " + modal.textContent);
}
console.log("ok 5: ONE bulk command fanned out — request carried tag/action/base64 script, result reports 2 matched · 1 pushed (dev-alpha → cmd-b2-0) · 1 offline (dev-beta)");

console.log("\nPASS: B-2 UI DoD — tag a cohort, filter to the tag group, dispatch one command to the whole group");
process.exit(0);
