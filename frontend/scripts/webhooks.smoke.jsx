// D-4 webhook management smoke test: drives the REAL <App/> in jsdom
// against a fake webhook backend, and proves the definition of done:
//
//   1. the header nav carries a 6th item "Webhooks" -> #/webhooks;
//   2. the endpoint list shows the pre-existing endpoint (name, URL,
//      subscribed categories, delivery cursor, consecutive failures,
//      enabled state);
//   3. "Add webhook" posts name/URL/secret + the checked categories
//      (subscribed to `alert`) to POST /api/webhooks and the new row
//      appears with its cursor at the journal tail;
//   4. toggling the new endpoint off PATCHes /api/webhooks/{id};
//   5. "Deliveries" opens the per-endpoint journal: the events it is
//      subscribed to, with sequence numbers and timestamps, each row
//      colored delivered (seq <= cursor) or pending (seq > cursor);
//   6. "Replay all" asks for confirmation, then POSTs from_seq=0 and
//      shows the cursor reset confirmation (from_seq=0, last_seq
//      reported);
//   7. deleting the endpoint issues DELETE /api/webhooks/{id} and the
//      row disappears from the list.
//
// Run: node scripts/webhooks.smoke.mjs   (bundles the JSX with esbuild)
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

// ---- the fake journal + endpoints ------------------------------------------
const CATS = ["alert", "inventory", "automation", "other"];
const DEVICES = [
  { id: "dev-web-01", hostname: "web-01", os: "linux", arch: "amd64", agent_version: "0.1.0", interfaces: ["10.0.0.11"], tags: ["web"], online: true, first_seen: "2026-07-01T00:00:00Z", last_seen: "2026-08-25T11:59:00Z" },
  { id: "dev-fs-01", hostname: "fileserver-01", os: "linux", arch: "amd64", agent_version: "0.1.0", interfaces: ["10.0.0.21"], tags: ["files"], online: true, first_seen: "2026-07-01T00:00:00Z", last_seen: "2026-08-25T11:59:00Z" },
];
const JOURNAL = [];
for (let seq = 1; seq <= 60; seq++) {
  JOURNAL.push({
    id: seq,
    category: CATS[(seq - 1) % 4],
    type: seq % 2 === 0 ? "resolved" : "fired",
    device_id: seq % 3 === 0 ? "dev-fs-01" : "dev-web-01",
    at: `2026-08-25T10:${String(Math.floor((seq * 37) % 60)).padStart(2, "0")}:${String(seq % 60).padStart(2, "0")}Z`,
    event: { seq, hello: "envelope" },
  });
}
const MAX_SEQ = JOURNAL.length;
const ALL_CATS = [...CATS];

const ENDPOINTS = [
  {
    id: 1,
    name: "ops-pager",
    url: "https://ops.example.com/hooks/rmmway",
    categories: ["alert"],
    enabled: true,
    max_attempts: 5,
    timeout_ms: 5000,
    last_seq: 40, // a backlog exists: seq 41..60 were never 2xx'd
    attempts: 3,
    next_retry_at: "2026-08-25T12:05:00Z",
    status: "failing",
    created_at: "2026-08-01T00:00:00Z",
    updated_at: "2026-08-25T11:57:00Z",
  },
];
let nextId = 2;

const fetchLog = []; // { method, path, body }
async function fakeFetch(path, init = {}) {
  const method = init.method || "GET";
  const body = init.body ? JSON.parse(init.body) : null;
  fetchLog.push({ method, path, body });
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

  if (path === "/api/webhooks") {
    if (method === "GET") return json(ENDPOINTS.map((e) => ({ ...e })));
    if (method === "POST") {
      if (!body.name || !body.url || !body.secret) return json({ error: "name, url and secret are required" }, 422);
      for (const c of body.categories || []) {
        if (!ALL_CATS.includes(c)) return json({ error: `unknown category ${c}`, valid: ALL_CATS }, 422);
      }
      const ep = {
        id: nextId++,
        name: body.name,
        url: body.url,
        categories: body.categories && body.categories.length ? body.categories : ALL_CATS,
        enabled: body.enabled !== false,
        max_attempts: body.max_attempts || 5,
        timeout_ms: body.timeout_ms || 5000,
        last_seq: MAX_SEQ, // a new endpoint starts at the journal tail
        attempts: 0,
        next_retry_at: "2026-08-25T12:00:00Z",
        status: "ok",
        created_at: "2026-08-25T11:58:00Z",
        updated_at: "2026-08-25T11:58:00Z",
      };
      ENDPOINTS.push(ep);
      return json({ ...ep }, 201);
    }
    return json({ error: method + " not allowed" }, 405);
  }
  // strip the query string before matching (the regex anchors at $)
  const qmark = path.indexOf("?");
  const bare = qmark === -1 ? path : path.slice(0, qmark);
  const m = bare.match(/^\/api\/webhooks\/(\d+)(\/(events|replay))?$/);
  if (m) {
    const id = Number(m[1]);
    const ep = ENDPOINTS.find((e) => e.id === id);
    if (!ep) return json({ error: `webhook ${id} not found` }, 404);
    const sub = m[3];
    if (sub === "events") {
      if (method !== "GET") return json({ error: "GET only" }, 405);
      const u = new URL(path, "http://localhost");
      const after = Number(u.searchParams.get("after") || "0");
      const limit = Math.min(Number(u.searchParams.get("limit") || "200"), 1000);
      const category = u.searchParams.get("category") || "";
      const out = JOURNAL.filter(
        (e) =>
          e.id > after &&
          ep.categories.includes(e.category) &&
          (!category || e.category === category)
      ).sort((a, b) => a.id - b.id).slice(0, limit);
      return json(out.map((e) => ({ ...e })));
    }
    if (sub === "replay") {
      if (method !== "POST") return json({ error: "POST only" }, 405);
      const fromSeq = body ? body.from_seq : 0;
      if (fromSeq < 0) return json({ error: "from_seq must be >= 0" }, 400);
      ep.last_seq = fromSeq;
      ep.attempts = 0;
      ep.status = "ok";
      ep.updated_at = "2026-08-25T11:59:00Z";
      return json({ webhook_id: ep.id, from_seq: fromSeq, last_seq: fromSeq, status: ep.status });
    }
    if (method === "PATCH") {
      if (body.name !== undefined) ep.name = body.name;
      if (body.url !== undefined) ep.url = body.url;
      if (body.categories !== undefined) ep.categories = body.categories;
      if (body.enabled !== undefined) {
        ep.enabled = !!body.enabled;
        if (ep.enabled) { ep.attempts = 0; ep.status = "ok"; }
      }
      return json({ ...ep });
    }
    if (method === "DELETE") {
      ENDPOINTS.splice(ENDPOINTS.findIndex((e) => e.id === id), 1);
      return new Response(null, { status: 204 });
    }
    if (method === "GET") return json({ ...ep });
    return json({ error: method + " not allowed" }, 405);
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
function setChecked(el, v) {
  const proto = window.HTMLInputElement.prototype;
  Object.getOwnPropertyDescriptor(proto, "checked").set.call(el, v);
  el.dispatchEvent(new window.Event("click", { bubbles: true }));
}
const btn = (re) => [...container.querySelectorAll("button")].find((b) => re.test(b.textContent));
const hookRows = () => [...container.querySelectorAll("tr.webhook-row")];
const rowFor = (name) => hookRows().find((r) => r.textContent.includes(name));
const lastCall = (re) => [...fetchLog].reverse().find((c) => re.test(c.method + " " + c.path)) || null;

// ---- 1. sign in; the header nav carries "Webhooks" as the 6th item ----------
await waitUntil(() => text().includes("Sign in to continue"), "login screen");
await act(async () => {
  setVal(container.querySelector('input[type="password"]'), "smokepass");
  container.querySelector("form").dispatchEvent(
    new window.Event("submit", { bubbles: true, cancelable: true })
  );
});
await waitUntil(() => text().includes("web-01") && text().includes("fileserver-01"), "devices view");
const navHrefs = [...container.querySelectorAll("nav.nav a")].map((a) => a.getAttribute("href"));
if (navHrefs.slice(0, 6).join(",") !== "#/devices,#/alerts,#/flows,#/events,#/heal,#/webhooks") {
  throw new Error("nav order is wrong (want devices,alerts,flows,events,heal,webhooks first): " + navHrefs.join(","));
}
console.log("ok 1: the header nav carries Webhooks as the 6th item, after Heal");

// ---- 2. #/webhooks lists the pre-existing endpoint with its delivery state --
await act(async () => {
  window.location.hash = "#/webhooks";
});
await waitUntil(() => rowFor("ops-pager"), "the ops-pager endpoint row");
const pager = rowFor("ops-pager");
if (!pager.textContent.includes("https://ops.example.com/hooks/rmmway")) {
  throw new Error("the endpoint URL is not rendered");
}
if (!pager.textContent.includes("seq 40")) throw new Error("the delivery cursor (seq 40) is not rendered");
if (!/3 consecutive/.test(pager.textContent) || !/failing/.test(pager.textContent)) {
  throw new Error("the consecutive-failure count is not rendered");
}
if (!pager.querySelector(".cat-alert")) throw new Error("the subscribed category badge is missing");
console.log("ok 2: the endpoint list shows ops-pager — URL, alert subscription, cursor seq 40, 3 consecutive failures (failing), enabled");

// ---- 3. Add webhook: name/URL/secret + alert category -> POST ----------------
await act(async () => {
  btn(/add webhook/i).dispatchEvent(new window.MouseEvent("click", { bubbles: true }));
});
await waitUntil(() => container.querySelector("form.webhook-form"), "the add-webhook form");
const form = container.querySelector("form.webhook-form");
const finputs = [...form.querySelectorAll("input:not([type='checkbox'])")];
const cats = [...form.querySelectorAll(".webhook-cats input[type='checkbox']")];
await act(async () => {
  setVal(finputs[0], "slack-ops");
  setVal(finputs[1], "https://hooks.slack.com/T000/B000/000");
  setVal(finputs[2], "whsec_smoke_secret");
  // "alert" is pre-checked (cats[0]) — leave it; drop the other three.
  for (let i = 1; i < cats.length; i++) if (cats[i].checked) setChecked(cats[i], false);
  form.dispatchEvent(new window.Event("submit", { bubbles: true, cancelable: true }));
});
await waitUntil(() => lastCall(/^POST /), "the webhook create POST") .catch(() => {});
await waitUntil(() => rowFor("slack-ops"), "the slack-ops row");
const createCall = fetchLog.find((c) => c.method === "POST" && c.path === "/api/webhooks");
if (!createCall || !createCall.body) throw new Error("the create POST was not sent with a body");
if (
  createCall.body.name !== "slack-ops" ||
  createCall.body.url !== "https://hooks.slack.com/T000/B000/000" ||
  createCall.body.secret !== "whsec_smoke_secret" ||
  JSON.stringify(createCall.body.categories) !== JSON.stringify(["alert"])
) {
  throw new Error("the create POST body is wrong: " + JSON.stringify(createCall.body));
}
const slackRow = rowFor("slack-ops");
if (!/seq 60/.test(slackRow.textContent)) {
  throw new Error("the new endpoint's cursor should start at the journal tail (seq 60): " + slackRow.textContent);
}
console.log("ok 3: Add webhook posts {name, url, secret, categories:[alert]} to POST /api/webhooks; the new row appears with its cursor at the journal tail");

// ---- 4. toggle the new endpoint off -> PATCH /api/webhooks/{id} ---------------
const slackToggle = rowFor("slack-ops").querySelector('input[type="checkbox"]');
await act(async () => {
  setChecked(slackToggle, false);
});
const patchCall = lastCall(/^PATCH /);
if (!patchCall) throw new Error("no PATCH was sent for the enable toggle");
if (!/\/api\/webhooks\/2$/.test(patchCall.path)) throw new Error("PATCH hit the wrong endpoint: " + patchCall.path);
if (patchCall.body.enabled !== false) throw new Error("the PATCH body is not {enabled:false}: " + JSON.stringify(patchCall.body));
await waitUntil(() => rowFor("slack-ops").classList.contains("off"), "the slack-ops row rendered disabled");
console.log("ok 4: toggling the new endpoint off PATCHes /api/webhooks/2 with {enabled:false} and the row renders disabled");

// ---- 5. Deliveries: the subscribed journal, seq-colored against the cursor ---
await act(async () => {
  [...rowFor("ops-pager").querySelectorAll("button")].find((b) => /deliveries/i.test(b.textContent))
    .dispatchEvent(new window.MouseEvent("click", { bubbles: true }));
});
await waitUntil(() => container.querySelector(".webhook-journal-meta"), "the deliveries panel");
await waitUntil(() => container.querySelectorAll("tr.journal-row").length === 15, "15 alert events (the endpoint's category)");
const dRows = [...container.querySelectorAll("tr.journal-row")];
if (!dRows.every((r) => r.querySelector(".cat-alert"))) throw new Error("the journal shows non-alert events");
const delivered = dRows.filter((r) => r.querySelector(".cat-ok")).length;
const pending = dRows.filter((r) => r.querySelector(".cat-pending")).length;
if (delivered !== 10 || pending !== 5) {
  throw new Error(`cursor split is wrong: delivered=${delivered}, pending=${pending} (want 10/5 at cursor 40)`);
}
const firstSeq = dRows[dRows.length - 1].querySelector("td").textContent;
const lastSeq = dRows[0].querySelector("td").textContent;
if (firstSeq !== "1" || lastSeq !== "57") {
  throw new Error(`journal seqs are wrong: newest=${lastSeq}, oldest=${firstSeq} (want 57/1)`);
}
{
  const when = dRows[0].querySelector("td:nth-child(5)").textContent;
  if (!when.trim() || /invalid/i.test(when)) {
    throw new Error("journal rows are missing valid timestamps: " + when);
  }
}
console.log("ok 5: Deliveries shows the endpoint's subscribed events (15 alert rows, seqs 1..57 with timestamps) — 10 delivered up to the cursor, 5 pending in the backlog");

// ---- 6. Replay all -> confirm -> cursor reset confirmation --------------------
await act(async () => {
  btn(/replay all/i).dispatchEvent(new window.MouseEvent("click", { bubbles: true }));
});
await waitUntil(() => btn(/yes, replay all/i), "the replay-all confirmation");
await act(async () => {
  btn(/yes, replay all/i).dispatchEvent(new window.MouseEvent("click", { bubbles: true }));
});
await waitUntil(() => {
  const c = lastCall(/^POST /);
  return c && /replay/.test(c.path) && c.body && c.body.from_seq === 0;
}, "the replay POST with from_seq=0");
const replayCall = lastCall(/^POST /);
if (!/^\/api\/webhooks\/1\/replay$/.test(replayCall.path)) {
  throw new Error("replay hit the wrong path: " + replayCall.path);
}
await waitUntil(() => text().includes("from_seq=0") && text().includes("last_seq=0"), "the cursor reset confirmation");
await waitUntil(
  () => [...container.querySelectorAll("tr.journal-row")].every((r) => r.querySelector(".cat-pending")),
  "the whole journal re-marked pending after the cursor reset"
);
console.log("ok 6: Replay all confirms, POSTs {from_seq:0} to /api/webhooks/1/replay, shows the cursor reset (from_seq=0, last_seq=0) and the whole journal is pending again");

// ---- 7. delete the slack endpoint -> it disappears ----------------------------
await act(async () => {
  btn(/all webhooks/i).dispatchEvent(new window.MouseEvent("click", { bubbles: true }));
});
await waitUntil(() => hookRows().length === 2, "back to the list with both endpoints");
await act(async () => {
  [...rowFor("slack-ops").querySelectorAll("button")].find((b) => /delete/i.test(b.textContent)).dispatchEvent(
    new window.MouseEvent("click", { bubbles: true })
  );
});
await waitUntil(() => lastCall(/^DELETE /), "the DELETE call") .catch(() => {});
await waitUntil(() => hookRows().length === 1 && !text().includes("slack-ops"), "slack-ops gone from the list");
const delCall = lastCall(/^DELETE /);
if (!/^\/api\/webhooks\/2$/.test(delCall.path)) throw new Error("DELETE hit the wrong path: " + delCall.path);
console.log("ok 7: Delete issues DELETE /api/webhooks/2 and the endpoint disappears from the list");

for (const es of [...liveStreams]) es.close();
console.log("\nD-4 webhook manager UI DoD PASS: nav item, endpoint list (cursor + failures), add (POST with secret + categories), disable (PATCH), deliveries journal with delivered/pending split, Replay all with confirm + cursor reset, delete.");
process.exit(0);
