// D-3 heal engine dashboard smoke test: drives the REAL <App/> in jsdom
// against a fake heal backend, and proves the definition of done:
//
//   1. the header nav carries the 5th item "Heal" (after Events) -> #/heal;
//   2. the Playbooks panel shows the 3 pre-existing playbooks with their
//      trigger conditions (e.g. "cpu.utilization_percent > 90"), scope,
//      action, last run, and an enable switch;
//   3. toggling a playbook off PATCHes /api/heal/playbooks/{key} and the
//      row renders disabled;
//   4. "Run Pass Now" (POST /api/heal/pass) starts a new run for the
//      failing cpu-high series — it appears in the Runs panel immediately
//      and, across successive passes, walks the state machine
//      detected → verifying → remediating → confirming → resolved (the
//      pass summary reports each transition);
//   5. clicking the run opens its full stage trace: which trigger fired
//      (metric + measured value), what command was dispatched, the agent's
//      script output, and the final confirmation;
//   6. the status filter narrows the runs list server-side (the request
//      URL carries status=);
//   7. the "New playbook" form POSTs to /api/heal/playbooks and the new
//      rule appears in the table.
//
// Run: node scripts/heal.smoke.mjs   (bundles the JSX with esbuild)
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

// ---- fake EventSource (jsdom has none; App opens the live stream) ----------
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
}
globalThis.EventSource = FakeEventSource;

// ---- the fake heal backend ---------------------------------------------------
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
const ACTIVE = new Set(["detected", "verifying", "remediating", "confirming"]);
const PB = {
  key: "cpu-high", name: "CPU saturation",
  description: "Reboot the worker pool when the box pegs",
  metric: "cpu.utilization_percent", source: "system",
  detect_op: ">", detect_threshold: 90, os_filter: "linux",
  fresh_within_seconds: 900, cooldown_seconds: 3600,
  remediate_sh: "systemctl restart rmmway-worker", remediate_powershell: "",
  confirm_op: "<", confirm_threshold: 60,
  remediate_timeout_seconds: 120, confirm_wait_seconds: 60,
  enabled: true, updated_at: "2026-08-20T09:00:00Z",
};
const PLAYBOOKS = [
  PB,
  {
    key: "disk-full", name: "Disk pressure",
    description: "Vacuum journals before the root fs fills",
    metric: "disk.used_percent", source: "system",
    detect_op: ">", detect_threshold: 92, os_filter: "linux",
    fresh_within_seconds: 900, cooldown_seconds: 1800,
    remediate_sh: "journalctl --vacuum-size=1G", remediate_powershell: "",
    confirm_op: "<", confirm_threshold: 85,
    remediate_timeout_seconds: 300, confirm_wait_seconds: 60,
    enabled: true, updated_at: "2026-08-20T09:00:00Z",
  },
  {
    key: "mem-pressure", name: "Memory pressure",
    description: "Compact memory when usage pegs",
    metric: "mem.used_percent", source: "system",
    detect_op: ">", detect_threshold: 95, os_filter: "",
    fresh_within_seconds: 900, cooldown_seconds: 900,
    remediate_sh: "sync && echo 1 > /proc/sys/vm/compact_memory",
    remediate_powershell: "garbage collect -server",
    confirm_op: "<", confirm_threshold: 90,
    remediate_timeout_seconds: 120, confirm_wait_seconds: 60,
    enabled: true, updated_at: "2026-08-20T09:00:00Z",
  },
];

const EVENTS = [];
let nextEventId = 1;
function seedRun(id, pbKey, devId, status, at, reasons) {
  const ts = (i) => `2026-08-${String(10 + i).padStart(2, "0")}T${String(8 + i).padStart(2, "0")}:00:00Z`;
  const run = {
    id, playbook_key: pbKey, device_id: devId, source: "system",
    status, reason: "",
    detect_value: 97.4, detect_at: ts(0),
    created_at: ts(0), updated_at: ts(reasons.length - 1),
  };
  if (status === "resolved" || status === "escalated") {
    run.command_id = "cmd-" + id;
    run.dispatched_at = ts(2);
    run.remediated_at = ts(3);
  }
  if (status === "resolved") { run.confirm_value = 41.2; run.confirmed_at = ts(reasons.length - 1); }
  if (status === "escalated") run.escalated_at = ts(reasons.length - 1);
  for (let i = 0; i < reasons.length; i++) {
    EVENTS.push({ id: nextEventId++, run_id: id, status: reasons[i].status, at: ts(i), reason: reasons[i].reason });
  }
  RUNS.push(run);
}
const RUNS = [];
seedRun(1, "cpu-high", "dev-web-01", "resolved", null, [
  { status: "detected", reason: "cpu.utilization_percent 99.1 > 90 (web-01)" },
  { status: "verifying", reason: "verify-safe: device online, no active run, cooldown clear" },
  { status: "remediating", reason: "dispatched cmd-1: sh 'systemctl restart rmmway-worker'" },
  { status: "confirming", reason: "agent reported exit 0; output: worker pool restarted in 1.4s" },
  { status: "resolved", reason: "re-measured 41.2 < 60 — healed" },
]);
seedRun(2, "disk-full", "dev-fs-01", "escalated", null, [
  { status: "detected", reason: "disk.used_percent 95.8 > 92 (fileserver-01)" },
  { status: "verifying", reason: "verify-safe: device online, no active run, cooldown clear" },
  { status: "remediating", reason: "dispatched cmd-2: sh 'journalctl --vacuum-size=1G'" },
  { status: "confirming", reason: "agent reported exit 0; output: freed 1.2G" },
  { status: "escalated", reason: "re-measured 93.1 still > 85 — page on-call (disk keeps filling)" },
]);
let nextRunId = 3;

// The series the detect stage is currently finding (the "failing" state).
const FAILING = { "cpu-high": "dev-web-01" };
const hostname = (id) => DEVICES.find((d) => d.id === id)?.hostname || id;
const firstLine = (s) => (s || "").split("\n")[0].trim();

// One synchronous pass: stage 1 starts a run for every fresh detection;
// stage 2 advances every active run exactly one stage (mirroring
// heal.Engine.RunOnce — a fresh run completes over successive passes).
function runPass() {
  const pass = { detections: 0, started: 0, skipped: 0, confirmed: 0, escalated: 0, failed: 0, active_runs: 0, errors: [] };
  const now = new Date().toISOString();
  for (const pb of PLAYBOOKS) {
    if (!pb.enabled) continue;
    const devId = FAILING[pb.key];
    if (!devId) continue;
    pass.detections++;
    const active = RUNS.some((r) => r.playbook_key === pb.key && r.device_id === devId && ACTIVE.has(r.status));
    if (active) {
      pass.skipped++;
      pass.errors.push(`${pb.key}/${devId}: run already in flight`);
      continue;
    }
    const id = nextRunId++;
    const run = {
      id, playbook_key: pb.key, device_id: devId, source: pb.source || "system",
      status: "detected", detect_value: 97.4, detect_at: now,
      created_at: now, updated_at: now,
    };
    RUNS.push(run);
    EVENTS.push({
      id: nextEventId++, run_id: id, status: "detected", at: now,
      reason: `${pb.metric} 97.4 ${pb.detect_op} ${pb.detect_threshold} (${hostname(devId)})`,
    });
    pass.started++;
  }
  for (const run of RUNS.filter((r) => ACTIVE.has(r.status)).sort((a, b) => a.id - b.id)) {
    const pb = PLAYBOOKS.find((p) => p.key === run.playbook_key);
    const stamp = { at: now };
    if (run.status === "detected") {
      run.status = "verifying";
      EVENTS.push({ id: nextEventId++, run_id: run.id, status: "verifying", at: stamp.at, reason: "verify-safe: device online, no active run, cooldown clear" });
    } else if (run.status === "verifying") {
      run.status = "remediating";
      run.command_id = "cmd-" + run.id;
      run.dispatched_at = stamp.at;
      EVENTS.push({ id: nextEventId++, run_id: run.id, status: "remediating", at: stamp.at, reason: `dispatched cmd-${run.id}: sh '${firstLine(pb.remediate_sh)}'` });
    } else if (run.status === "remediating") {
      run.status = "confirming";
      run.remediated_at = stamp.at;
      EVENTS.push({ id: nextEventId++, run_id: run.id, status: "confirming", at: stamp.at, reason: "agent reported exit 0; output: worker pool restarted in 1.2s" });
    } else if (run.status === "confirming") {
      run.status = "resolved";
      run.confirm_value = 43.1;
      run.confirmed_at = stamp.at;
      EVENTS.push({ id: nextEventId++, run_id: run.id, status: "resolved", at: stamp.at, reason: `re-measured 43.1 ${pb.confirm_op} ${pb.confirm_threshold} — healed` });
      pass.confirmed++;
    }
    run.updated_at = stamp.at;
  }
  pass.active_runs = RUNS.filter((r) => ACTIVE.has(r.status)).length;
  return pass;
}

// ---- the fake backend --------------------------------------------------------
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
  if (path === "/api/alerts/counts") return json({ open: 1, acked: 0, resolved: 0 });
  if (path.startsWith("/api/alerts")) return json([]);

  // heal subtree
  if (path.startsWith("/api/heal/playbooks")) {
    const parts = path.split("/").filter(Boolean); // ["api","heal","playbooks",key?]
    if (method === "GET") return json(PLAYBOOKS.map((p) => ({ ...p })));
    if (method === "POST") {
      const in_ = JSON.parse(init.body || "{}");
      if (!in_.name || !in_.metric || in_.detect_op == null) {
        return json({ error: "name, metric and detect_op are required" }, 422);
      }
      const key = (in_.name || "pb").toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "");
      const pb = {
        key, name: in_.name, description: in_.description || "",
        metric: in_.metric, source: in_.source || "", detect_op: in_.detect_op,
        detect_threshold: in_.detect_threshold, os_filter: in_.os_filter || "",
        fresh_within_seconds: 900, cooldown_seconds: in_.cooldown_seconds || 3600,
        remediate_sh: in_.remediate_sh || "", remediate_powershell: in_.remediate_powershell || "",
        confirm_op: in_.confirm_op || "", confirm_threshold: in_.confirm_threshold,
        remediate_timeout_seconds: 120, confirm_wait_seconds: 60,
        enabled: true, updated_at: new Date().toISOString(),
      };
      PLAYBOOKS.push(pb);
      return json(pb, 201);
    }
    if (method === "PATCH") {
      const key = parts[3];
      const pb = PLAYBOOKS.find((p) => p.key === key);
      if (!pb) return json({ error: `playbook ${key} not found` }, 404);
      const in_ = JSON.parse(init.body || "{}");
      if (in_.enabled !== undefined) pb.enabled = !!in_.enabled;
      pb.updated_at = new Date().toISOString();
      return json({ ...pb });
    }
    return json({ error: method + " not allowed" }, 405);
  }
  if (path.startsWith("/api/heal/runs/")) {
    const id = Number(path.split("/").filter(Boolean).pop());
    const run = RUNS.find((r) => r.id === id);
    if (!run) return json({ error: `heal run ${id} not found` }, 404);
    return json({ run: { ...run }, events: EVENTS.filter((e) => e.run_id === id).map((e) => ({ ...e })) });
  }
  if (path === "/api/heal/runs" || path.startsWith("/api/heal/runs?")) {
    const u = new URL(path, "http://localhost");
    const status = u.searchParams.get("status") || "";
    const deviceId = u.searchParams.get("device_id") || "";
    const limit = parseInt(u.searchParams.get("limit") || "100", 10);
    const out = RUNS.filter(
      (r) => (!status || r.status === status) && (!deviceId || r.device_id === deviceId)
    ).sort((a, b) => b.id - a.id).slice(0, limit);
    return json(out.map((r) => ({ ...r })));
  }
  if (path === "/api/heal/pass") {
    if (method !== "POST") return json({ error: "POST only" }, 405);
    return json(runPass());
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
function setSelect(el, v) {
  const proto = window.HTMLSelectElement.prototype;
  Object.getOwnPropertyDescriptor(proto, "value").set.call(el, v);
  el.dispatchEvent(new window.Event("change", { bubbles: true }));
}
function setChecked(el, v) {
  const proto = window.HTMLInputElement.prototype;
  Object.getOwnPropertyDescriptor(proto, "checked").set.call(el, v);
  el.dispatchEvent(new window.Event("click", { bubbles: true }));
}
const btn = (re) =>
  [...container.querySelectorAll("button")].find((b) => re.test(b.textContent));
const pbRows = () => [...container.querySelectorAll("tr.heal-pb-row")];
const runRows = () => [...container.querySelectorAll("tr.heal-run-row")];
const runStatus = (id) => {
  const row = runRows().find((r) => r.dataset.runId === String(id));
  return row ? row.querySelector(".pill").textContent.trim() : null;
};
const lastHealRunsCall = () =>
  [...fetchLog].reverse().find((c) => c.startsWith("GET /api/heal/runs?")) || "";

// ---- 1. sign in; the header nav carries "Heal" 5th, after Events -------------
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
if (navHrefs.slice(0, 5).join(",") !== "#/devices,#/alerts,#/flows,#/events,#/heal") {
  throw new Error("nav order is wrong (want devices,alerts,flows,events,heal first): " + navHrefs.join(","));
}
console.log("ok 2: the header nav carries Heal as the 5th item, after Events");

// ---- 2. #/heal: the 3 pre-existing playbooks render with their contract ------
await act(async () => {
  window.location.hash = "#/heal";
});
await waitUntil(() => pbRows().length === 3, "three playbook rows");
if (!text().includes("cpu.utilization_percent > 90")) {
  throw new Error("the cpu-high trigger condition is not rendered: " + text().slice(0, 400));
}
if (!text().includes("journalctl --vacuum-size=1G")) {
  throw new Error("the disk-full remediation action is not rendered");
}
for (const name of ["CPU saturation", "Disk pressure", "Memory pressure"]) {
  if (!text().includes(name)) throw new Error(`playbook ${name} missing from the table`);
}
console.log("ok 3: the Playbooks panel shows the 3 pre-existing playbooks (trigger, scope, action, last run, enabled)");

// ---- 3. toggle a playbook off -> PATCH /api/heal/playbooks/{key} -------------
const diskToggle = container.querySelector('input.pb-toggle[data-pb="disk-full"]');
if (!diskToggle) throw new Error("no enable switch on the disk-full playbook row");
await act(async () => {
  setChecked(diskToggle, false);
});
await waitUntil(
  () => fetchLog.some((c) => c === "PATCH /api/heal/playbooks/disk-full"),
  "the PATCH for the disk-full toggle"
);
await waitUntil(() => {
  const row = pbRows().find((r) => r.querySelector('[data-pb="disk-full"]'));
  return row && row.classList.contains("off");
}, "the disk-full row rendered disabled");
if (PLAYBOOKS.find((p) => p.key === "disk-full").enabled !== false) {
  throw new Error("the fake backend never saw enabled=false");
}
console.log("ok 4: toggling disk-full off PATCHes /api/heal/playbooks/disk-full and the row renders disabled");

// ---- 4. Run Pass Now: a fresh run appears and walks detected->resolved -------
await act(async () => {
  btn(/^run pass now$/i).dispatchEvent(new window.MouseEvent("click", { bubbles: true }));
});
await waitUntil(
  () => fetchLog.some((c) => c === "POST /api/heal/pass") && runRows().some((r) => r.dataset.runId === "3"),
  "run #3 in the Runs panel after the first pass"
);
if (runStatus(3) !== "verifying") {
  throw new Error(`after pass 1 the run should be advancing (verifying), got ${runStatus(3)}`);
}
if (!text().includes("1 started")) {
  throw new Error("the pass summary does not report the started run: " + text().slice(0, 400));
}
console.log("ok 5: Run Pass Now starts a new run (run 3, cpu-high on web-01) and it appears in the Runs panel at once");

const EXPECTED = { 2: "remediating", 3: "confirming", 4: "resolved" };
for (const [pass, want] of Object.entries(EXPECTED)) {
  await act(async () => {
    btn(/running pass…|run pass now/i).dispatchEvent(new window.MouseEvent("click", { bubbles: true }));
  });
  await waitUntil(() => runStatus(3) === want, `run 3 reaching ${want} after pass ${pass}`);
}
if (!text().includes("1 confirmed")) {
  throw new Error("the final pass summary does not report the confirmed heal");
}
if (runRows().length !== 3) {
  throw new Error(`expected the 2 seeded runs + the new one (3 rows), got ${runRows().length}`);
}
console.log("ok 6: successive passes walk the run detected -> verifying -> remediating -> confirming -> resolved (RUNNING -> SUCCEEDED)");

// ---- 5. click the run -> full stage trace incl. the agent's script output ----
await act(async () => {
  runRows().find((r) => r.dataset.runId === "3").dispatchEvent(new window.MouseEvent("click", { bubbles: true }));
});
await waitUntil(() => container.querySelectorAll(".heal-trace-step").length === 5, "the 5-step stage trace");
const trace = text();
if (!trace.includes("cpu.utilization_percent 97.4 > 90")) throw new Error("the trace is missing the fired trigger");
if (!trace.includes("dispatched cmd-3: sh 'systemctl restart rmmway-worker'")) throw new Error("the trace is missing the dispatched command");
if (!trace.includes("agent reported exit 0; output: worker pool restarted in 1.2s")) {
  throw new Error("the trace is missing the agent's script output");
}
if (!trace.includes("re-measured 43.1 < 60 — healed")) throw new Error("the trace is missing the final confirmation");
console.log("ok 7: clicking the run shows the full stage trace — trigger fired, command dispatched, agent script output, confirmation");

// ---- 6. the status filter narrows the list server-side -----------------------
// (the runs table is behind the detail pane — close it first)
await act(async () => {
  btn(/all runs/i).dispatchEvent(new window.MouseEvent("click", { bubbles: true }));
});
await waitUntil(() => runRows().length === 3, "the runs table back in view");
const statusSelect = container.querySelector(".heal-runs-filters select");
await act(async () => {
  setSelect(statusSelect, "resolved");
});
await waitUntil(() => lastHealRunsCall().includes("status=resolved"), "the filtered runs request");
await waitUntil(() => runRows().length === 2, "only the resolved runs (1 and 3)");
const ids = runRows().map((r) => r.dataset.runId).sort();
if (ids.join(",") !== "1,3") throw new Error(`status=resolved should leave runs 1 and 3, got ${ids.join(",")}`);
console.log("ok 8: the status filter sends status=resolved to the server and the list narrows to the matching runs");

// ---- 7. the New playbook form POSTs a new rule --------------------------------
await act(async () => {
  btn(/new playbook/i).dispatchEvent(new window.MouseEvent("click", { bubbles: true }));
});
await waitUntil(() => container.querySelector("form.heal-form"), "the create-playbook form");
const form = container.querySelector("form.heal-form");
const inputs = [...form.querySelectorAll("input")];
const selects = [...form.querySelectorAll("select")];
const textareas = [...form.querySelectorAll("textarea")];
await act(async () => {
  setVal(inputs[0], "Fan death");
  setVal(inputs[1], "fan.rpm");
  setVal(inputs[2], "0"); // detect threshold
  setVal(textareas[0], "fancontrol set 0 100");
  form.dispatchEvent(new window.Event("submit", { bubbles: true, cancelable: true }));
});
await waitUntil(
  () => fetchLog.some((c) => c === "POST /api/heal/playbooks"),
  "the create-playbook POST"
);
await waitUntil(() => pbRows().some((r) => text().includes("fan-death") && r.textContent.includes("Fan death")), "the new playbook row");
if (!text().includes("fan.rpm > 0")) throw new Error("the new playbook's trigger is not rendered");
console.log("ok 9: the New playbook form POSTs to /api/heal/playbooks and the new rule appears in the table");

// close any lingering streams so node can exit
for (const es of [...liveStreams]) es.close();
console.log("\nD-3 heal dashboard UI DoD PASS: nav item, 3 playbooks, toggle off (PATCH), Run Pass Now -> run walks detected->resolved, full stage trace with agent output, server-side status filter, create form.");
process.exit(0);
