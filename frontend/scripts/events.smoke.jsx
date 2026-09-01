// D-2 event journal frontend smoke test: drives the REAL <App/> in jsdom
// against a fake backend whose journal holds 500 entries, and proves the
// definition of done:
//
//   1. the header nav carries the 4th item "Events" (after Flows) -> #/events;
//   2. first visit pages FORWARD to the end of the journal and displays the
//      final batch — the 100 most recent entries (seq 401-500), newest first,
//      with color-coded category badges and resolved device hostnames;
//   3. category=alert + device=web-01 filters server-side (the request URL
//      carries both params) and isolates that device's alert events (125);
//   4. clicking a row expands the detail pane: the full envelope JSON plus a
//      "Go to device" link that lands on the filtered device view;
//   5. "Load earlier" pages back one batch (after = first_seq - PAGE_SIZE - 1
//      = 200 -> seq 201-400, adjacent to the 401-500 page), "Jump to latest"
//      returns to the tail;
//   6. a new envelope arriving on the live SSE stream appears at the top of
//      the view WITHOUT a re-fetch of /api/events.
//
// Run: node scripts/events.smoke.mjs   (bundles the JSX with esbuild)
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

// ---- fake EventSource (jsdom has none) -------------------------------------
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

// ---- the fake journal (500 entries, the exact Envelope wire shape) ---------
const DEVICES = [
  {
    id: "dev-web-01", hostname: "web-01", os: "linux", arch: "amd64",
    agent_version: "0.1.0", interfaces: ["10.0.0.11"], tags: ["web"],
    online: true, first_seen: "2026-07-01T00:00:00Z", last_seen: "2026-08-25T11:59:00Z",
  },
  {
    id: "dev-fs-01", hostname: "fileserver-01", os: "linux", arch: "amd64",
    agent_version: "0.1.0", interfaces: ["10.0.0.21"], tags: ["files"],
    online: true, first_seen: "2026-07-01T00:00:00Z", last_seen: "2026-08-25T11:59:00Z",
  },
];
const SUBJECTS = [
  ["alert", "rmmway.events.alert"],
  ["inventory", "rmmway.events.device"],
  ["automation", "rmmway.events.command.result"],
  ["other", "rmmway.events.flow.step"],
];
const BASE_TS = Date.UTC(2026, 7, 25, 12, 0, 0);

function busEvent(i, category, type, dev) {
  const at = new Date(BASE_TS + i * 1000).toISOString();
  const base = { type, device_id: dev.id, at };
  if (category === "alert") {
    return {
      ...base, source: "system",
      data: {
        action: "fired", name: "cpu.utilization_percent", source: "system",
        value: 90 + (i % 10), score: 12.5, channel: "seasonal", status: "open", at,
      },
    };
  }
  if (category === "inventory") {
    return {
      ...base, message: dev.hostname + " online",
      data: { action: "online", hostname: dev.hostname, os: dev.os, arch: dev.arch },
    };
  }
  if (category === "automation" && type === "rmmway.events.command.result") {
    return { ...base, command_id: "cmd-" + i, status: i % 3 === 0 ? "FAILED" : "SUCCEEDED" };
  }
  return { ...base, run_id: 1000 + i, node_id: "notify", message: "step executed" };
}

const journal = [];
for (let i = 1; i <= 500; i++) {
  const [category, type] = SUBJECTS[(i - 1) % 4];
  const dev = DEVICES[(i - 1) % 2];
  journal.push({
    id: i,
    version: "rmmway-event/v1",
    source: "rmmway",
    category,
    type,
    device_id: dev.id,
    at: new Date(BASE_TS + i * 1000).toISOString(),
    event: busEvent(i, category, type, dev),
  });
}

// publishEvent pushes one envelope onto every open stream (id 501+ — beyond
// the journal, like a brand-new event the operator hasn't paged to yet).
let liveSeq = 500;
function publishEvent(category, type, dev, event) {
  liveSeq += 1;
  const env = {
    id: liveSeq,
    version: "rmmway-event/v1",
    source: "rmmway",
    category,
    type,
    device_id: dev.id,
    at: new Date(BASE_TS + liveSeq * 1000).toISOString(),
    event,
  };
  for (const es of [...liveStreams]) es.dispatchMessage(JSON.stringify(env));
  return env;
}

// ---- the fake backend -------------------------------------------------------
const fetchLog = []; // "METHOD path" (incl. query) in call order
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
  if (path === "/api/devices") return json(DEVICES);
  if (path === "/api/alerts/counts") return json({ open: 0, acked: 0, resolved: 0 });
  if (path.startsWith("/api/alerts")) return json([]);
  if (path.startsWith("/api/events?") || path === "/api/events") {
    const u = new URL(path, "http://localhost");
    const after = parseInt(u.searchParams.get("after") || "0", 10);
    const limit = parseInt(u.searchParams.get("limit") || "200", 10);
    const category = u.searchParams.get("category") || "";
    const device = u.searchParams.get("device") || "";
    const type = u.searchParams.get("type") || "";
    // The server validates the category (400 on unknown) and answers
    // seq > after, oldest first, up to limit — mirrored here.
    if (category && !SUBJECTS.some(([c]) => c === category)) {
      return json({ error: `unknown category ${category}` }, 400);
    }
    const out = journal.filter(
      (e) =>
        e.id > after &&
        (!category || e.category === category) &&
        (!device || e.device_id === device) &&
        (!type || e.type === type)
    ).slice(0, limit);
    return json(out);
  }
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
  const proto = window.HTMLInputElement.prototype;
  Object.getOwnPropertyDescriptor(proto, "value").set.call(el, v);
  el.dispatchEvent(new window.Event("input", { bubbles: true }));
}
function setSelect(el, v) {
  const proto = window.HTMLSelectElement.prototype;
  Object.getOwnPropertyDescriptor(proto, "value").set.call(el, v);
  el.dispatchEvent(new window.Event("change", { bubbles: true }));
}
const btn = (re) =>
  [...container.querySelectorAll("button")].find((b) => re.test(b.textContent));
const journalRows = () => [...container.querySelectorAll("tbody tr.journal-row")];
const firstSeq = () => {
  const r = journalRows()[0];
  return r ? Number(r.dataset.seq) : null;
};
const lastSeq = () => {
  const rows = journalRows();
  return rows.length ? Number(rows[rows.length - 1].dataset.seq) : null;
};
const lastEventsCall = () =>
  [...fetchLog].reverse().find((c) => c.startsWith("GET /api/events?")) || "";

// ---- 1. sign in; the header nav carries "Events" 4th, after Flows ----------
await waitUntil(() => text().includes("Sign in to continue"), "login screen");
await act(async () => {
  setVal(container.querySelector('input[type="password"]'), "smokepass");
  container.querySelector("form").dispatchEvent(
    new window.Event("submit", { bubbles: true, cancelable: true })
  );
});
await waitUntil(() => text().includes("web-01") && text().includes("fileserver-01"), "devices view");
console.log("ok 1: sign-in lands on the Devices view");

const navHrefs = [...container.querySelectorAll("nav.nav a")].map((a) => a.getAttribute("href"));
if (navHrefs.slice(0, 4).join(",") !== "#/devices,#/alerts,#/flows,#/events") {
  throw new Error("nav order is wrong (want devices,alerts,flows,events first): " + navHrefs.join(","));
}
console.log("ok 2: the header nav carries Events as the 4th item, after Flows");

// ---- 2. first visit: page forward to the end, show the final batch ----------
await act(async () => {
  window.location.hash = "#/events";
});
await waitUntil(() => journalRows().length === 100, "the journal's final batch (100 rows)");
if (firstSeq() !== 500 || lastSeq() !== 401) {
  throw new Error(`expected newest-first tail page 500..401, got ${firstSeq()}..${lastSeq()}`);
}
if (!text().includes("seq 401–500")) {
  throw new Error("page header does not report the window 401–500: " + text().slice(0, 300));
}
for (const cls of ["cat-alert", "cat-inventory", "cat-automation", "cat-other"]) {
  if (!container.querySelector(`.pill.${cls}`)) {
    throw new Error(`missing category badge .${cls} in the journal table`);
  }
}
// Every row resolves its device hostname (the journal only stores device_id).
const badHost = journalRows().find(
  (r) => !/^(web-01|fileserver-01)$/.test(r.children[2].textContent.trim())
);
if (badHost) throw new Error("a row failed to resolve a device hostname: " + badHost.children[2].textContent);
console.log(
  "ok 3: first visit pages forward to the end of the journal and shows the 100 most recent entries (401–500), newest first, category badges + hostnames"
);

// ---- 3. category=alert + device=web-01 isolates one device's alerts ---------
const selects = [...container.querySelectorAll(".view-head select")];
if (selects.length < 2) throw new Error("expected category + device filter selects");
await act(async () => {
  setSelect(selects[0], "alert");
  setSelect(selects[1], "dev-web-01");
  btn(/^apply$/i).dispatchEvent(new window.MouseEvent("click", { bubbles: true }));
});
if (!lastEventsCall().includes("category=alert") || !lastEventsCall().includes("device=dev-web-01")) {
  throw new Error("filter request did not carry both params: " + lastEventsCall());
}
await waitUntil(() => journalRows().length === 125, "125 alert events for web-01");
if (firstSeq() !== 497) throw new Error(`filtered page should end at seq 497, got first=${firstSeq()}`);
const notAlert = journalRows().find((r) => !r.querySelector(".pill.cat-alert"));
if (notAlert) throw new Error("a non-alert row slipped through the category filter");
const wrongDevice = journalRows().find((r) => r.children[2].textContent.trim() !== "web-01");
if (wrongDevice) throw new Error("a row for another device slipped through the device filter");
console.log("ok 4: category=alert + device=web-01 filters server-side and isolates that device's 125 alert events");

// ---- 4. click a row -> full envelope JSON + "Go to device" -------------------
await act(async () => {
  journalRows()[0].dispatchEvent(new window.MouseEvent("click", { bubbles: true }));
});
await waitUntil(() => container.querySelector("pre.journal-json"), "the expanded detail pane");
const pane = container.querySelector("pre.journal-json").textContent;
if (!pane.includes('"id": 497') || !pane.includes("cpu.utilization_percent")) {
  throw new Error("detail pane does not show the full envelope JSON: " + pane.slice(0, 200));
}
const goDev = btn(/go to device/i);
if (!goDev) throw new Error("no 'Go to device' action in the detail pane");
await act(async () => {
  goDev.dispatchEvent(new window.MouseEvent("click", { bubbles: true }));
});
await waitUntil(
  () => window.location.hash.startsWith("#/devices") && text().includes("web-01"),
  "the filtered device view after 'Go to device'"
);
console.log("ok 5: clicking a row shows the full envelope JSON; 'Go to device' lands on the device view");

// ---- 5. "Load earlier" pages back; "Jump to latest" returns to the tail -----
await act(async () => {
  window.location.hash = "#/events";
});
await waitUntil(() => journalRows().length === 100 && firstSeq() === 500, "fresh mount back at the tail");
await act(async () => {
  btn(/load earlier/i).dispatchEvent(new window.MouseEvent("click", { bubbles: true }));
});
if (!lastEventsCall().includes("after=200")) {
  throw new Error(
    `'Load earlier' did not request the preceding batch (after=200); last=${lastEventsCall()}; all=${fetchLog
      .filter((c) => c.startsWith("GET /api/events"))
      .join(" | ")}`
  );
}
await waitUntil(() => firstSeq() === 400 && journalRows().length === 200, "the preceding batch (201–400)");
if (!btn(/jump to latest/i)) throw new Error("'Jump to latest' is not offered while viewing history");
await act(async () => {
  btn(/jump to latest/i).dispatchEvent(new window.MouseEvent("click", { bubbles: true }));
});
await waitUntil(() => firstSeq() === 500, "back at the journal tail");
if (!lastEventsCall().includes("after=400")) {
  throw new Error("'Jump to latest' did not re-walk to the end (after=400): " + lastEventsCall());
}
console.log("ok 6: 'Load earlier' pages back one batch (after=200 -> 201–400); 'Jump to latest' re-walks to the tail");

// ---- 6. a live SSE event appears at the top WITHOUT a re-fetch --------------
const callsBefore = fetchLog.length;
await act(async () => {
  publishEvent("alert", "rmmway.events.alert", DEVICES[0], busEvent(501, "alert", "rmmway.events.alert", DEVICES[0]));
});
await waitUntil(() => journalRows().some((r) => r.dataset.seq === "501"), "live event 501 in the table");
if (firstSeq() !== 501) throw new Error("the live event did not land at the TOP of the view");
const newJournalCalls = fetchLog.slice(callsBefore).filter((c) => c.startsWith("GET /api/events?"));
if (newJournalCalls.length > 0) {
  throw new Error("live event triggered a journal re-fetch (should be stream-only): " + newJournalCalls.join(" | "));
}
console.log("ok 7: a new event on the SSE stream appears at the top of the journal without a page refresh or re-fetch");

console.log("\nD-2 event journal UI DoD PASS: nav item, newest-first tail page, category+device filtering, envelope detail + go-to-device, load-earlier paging, live SSE prepend.");
process.exit(0);
