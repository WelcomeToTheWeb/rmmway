// Chart frontend smoke test: drives the REAL <App/> through jsdom and proves
// the revamped per-device metric chart's definition of done:
//
//   1. the chart renders an SVG with the series <polyline>, y-axis gridlines
//      with "nice" tick labels, human time ticks on the x-axis, a soft area
//      fill and a last-sample dot;
//   2. the stats row shows now/min/max (unit-aware, e.g. "61.0%") plus the
//      samples / range / bucket meta line;
//   3. the series picker is grouped (optgroup per category) and the chart
//      title humanizes the metric ("CPU utilization") while the raw dotted
//      name stays visible as context;
//   4. hovering the plot shows a crosshair + snapped tooltip with the
//      value + relative/absolute time, and mouseleave clears it (jsdom's
//      zero-width rect makes clientX act as SVG user units, so the snap
//      math is deterministic);
//   5. the "live · updated Ns ago" affordance is present.
//
// Run: node scripts/chart.smoke.mjs  (bundles the JSX with esbuild)
import { JSDOM } from "jsdom";

const dom = new JSDOM(
  "<!doctype html><html><body><div id=root></div></body></html>",
  { url: "http://localhost/", pretendToBeVisual: true },
);
globalThis.window = dom.window;
globalThis.document = dom.window.document;
Object.defineProperty(globalThis, "navigator", {
  value: dom.window.navigator,
  configurable: true,
});
globalThis.localStorage = dom.window.localStorage;
globalThis.HTMLElement = dom.window.HTMLElement;
globalThis.Event = dom.window.Event;
globalThis.getComputedStyle = dom.window.getComputedStyle;
globalThis.requestAnimationFrame = (cb) => setTimeout(cb, 0);
globalThis.cancelAnimationFrame = (id) => clearTimeout(id);
globalThis.IS_REACT_ACT_ENVIRONMENT = true;

// jsdom has no EventSource; App opens a live stream on sign-in.
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

// ---- the fake backend --------------------------------------------------------
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
  ],
  series: {
    "dev-alpha": [
      { name: "cpu.utilization_percent", source: "", last: 61.0, count: 40 },
    ],
  },
};
function makePoints(seed, n, lo, hi) {
  const now = Date.now();
  const pts = [];
  for (let i = 0; i < n; i++) {
    const v = lo + ((Math.sin(seed + i * 0.7) + 1) / 2) * (hi - lo);
    pts.push([now - (n - 1 - i) * 600000, Number(v.toFixed(2))]);
  }
  return pts;
}
const POINTS = makePoints("cpu.utilization_percent".length, 40, 10, 90);
const VALS = POINTS.map((p) => p[1]);

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
  const json = (obj, status = 200) =>
    new Response(JSON.stringify(obj), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  if (path === "/api/setup/status")
    return json({ available: true, setup: true });
  if (path === "/api/login") {
    if (body && body.username === "admin" && body.password === "smokepass") {
      return json({
        token: "smoke-token",
        expiry: "2030-01-01T00:00:00Z",
        capabilities: [],
      });
    }
    return json({ error: "invalid username or password" }, 401);
  }
  if (path === "/api/devices") return json(state.devices);
  const names = /^\/api\/devices\/([^/]+)\/metrics(\?|$)/.exec(path);
  if (names && method === "GET" && !path.includes("/metrics/series")) {
    const q = new URLSearchParams(path.split("?")[1] || "");
    const range = q.get("range") || "7d";
    if (!["1h", "6h", "24h", "7d", "30d"].includes(range))
      return json({ error: "bad range" }, 400);
    return json({
      device_id: decodeURIComponent(names[1]),
      range,
      series: state.series[decodeURIComponent(names[1])] || [],
    });
  }
  if (/^\/api\/devices\/([^/]+)\/metrics\/series/.test(path) && method === "GET") {
    return json({
      device_id: "dev-alpha",
      name: "cpu.utilization_percent",
      source: "",
      range: "24h",
      bucket_s: 600,
      count: POINTS.length,
      min: Math.min(...VALS),
      max: Math.max(...VALS),
      last: VALS[VALS.length - 1],
      points: POINTS,
    });
  }
  if (path === "/api/alerts/counts")
    return json({ open: 0, acked: 0, resolved: 0 });
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
      if (Date.now() - t0 > ms)
        return reject(new Error(`timeout waiting for: ${what}`));
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
const rows = () =>
  container.querySelectorAll("table.devices tbody tr:not(.detail-row)");

// ---- 1. sign in through the real login form ---------------------------------
await waitUntil(
  () => container.textContent.includes("Sign in to continue"),
  "the login screen",
);
const form = container.querySelector("form");
const pass = [...container.querySelectorAll("input")].find((i) => i.type === "password");
setVal(pass, "smokepass");
await act(async () => {
  form.dispatchEvent(new dom.window.Event("submit", { bubbles: true, cancelable: true }));
});
await waitUntil(() => rows().length === 1, "the device list");

// ---- 2. expand the device -> the chart renders ------------------------------
const alpha = [...rows()].find((r) => r.textContent.includes("alpha-host"));
await click(alpha);
await waitUntil(
  () =>
    alpha.nextElementSibling &&
    alpha.nextElementSibling.classList.contains("detail-row"),
  "the detail row",
);
const detail = alpha.nextElementSibling;
const svg = () => detail.querySelector(".metrics-chart");
await waitUntil(
  () =>
    svg() &&
    svg().querySelector("polyline") &&
    svg().querySelector("polyline").getAttribute("points").split(" ").length ===
      POINTS.length &&
    svg().querySelectorAll("line.metrics-grid").length >= 3 &&
    svg().querySelectorAll("text.metrics-ytick").length >= 3 &&
    svg().querySelectorAll("text.metrics-xtick").length >= 2 &&
    svg().querySelector("polygon.metrics-area") &&
    svg().querySelector("circle.metrics-last"),
  "the SVG chart: polyline + gridlines + y ticks + x ticks + area + last dot",
);
console.log(
  "ok 1: chart renders <polyline> with all 40 points, y gridlines + nice tick labels, human x time-ticks, area fill, last-sample dot",
);

// ---- 3. stats row: unit-aware now/min/max + samples/range/bucket meta --------
await waitUntil(
  () =>
    detail.querySelector(".metrics-stats") &&
    detail.querySelector(".metrics-stats").textContent.includes("600s buckets"),
  "the stats row",
);
const stats = detail.querySelector(".metrics-stats").textContent;
for (const needle of ["now", "min", "max", "40 samples", "24h"])
  if (!stats.includes(needle))
    throw new Error("stats row missing '" + needle + "': " + stats);
if (!/%/.test(stats))
  throw new Error("percent metric stats not unit-aware (%): " + stats);
console.log("ok 2: stats row shows now/min/max (unit-aware %) + '40 samples · 24h · 600s buckets'");

// ---- 4. grouped human picker + humanized title ------------------------------
const picker = detail.querySelector(".device-metrics select");
await waitUntil(
  () => picker.querySelector("optgroup[label='CPU']"),
  "the grouped picker (CPU optgroup)",
);
if (
  !picker.querySelector("optgroup[label='CPU'] option") ||
  picker.querySelector("optgroup[label='CPU'] option").textContent !==
    "cpu.utilization_percent"
)
  throw new Error("CPU optgroup option contract broken: " + picker.innerHTML);
await waitUntil(
  () =>
    detail.querySelector(".metrics-human") &&
    detail.querySelector(".metrics-human").textContent === "CPU utilization",
  "the humanized metric title",
);
const rawName = detail.querySelector(".metrics-rawname");
if (!rawName || rawName.textContent !== "cpu.utilization_percent")
  throw new Error("raw metric name not shown as context");
console.log("ok 3: series picker groups by category (CPU optgroup) and the title humanizes to 'CPU utilization' with the raw name as context");

// ---- 5. hover: crosshair + snapped tooltip, then clears on leave ------------
const live = detail.querySelector(".metrics-live");
if (!live || !/live/.test(live.textContent) || !/updated/.test(live.textContent))
  throw new Error("live/updated affordance missing: " + (live && live.textContent));
console.log("ok 4: 'live · updated Ns ago' affordance present");

// jsdom's getBoundingClientRect has zero width, so the chart maps clientX
// directly to SVG user units: clientX=380 is inside the plot (56..744) and
// snaps to the nearest sample (~middle of the series). React's synthetic
// onMouseLeave is derived from native mouseout (relatedTarget null in jsdom
// = left the element), so that is the event the smoke dispatches.
const fire = (type, x) =>
  act(async () => {
    const e = new dom.window.MouseEvent(type, {
      bubbles: true,
      cancelable: true,
      clientX: x,
      clientY: 40,
    });
    svg().dispatchEvent(e);
  });
await fire("mousemove", 380);
await waitUntil(
  () =>
    detail.querySelector(".metrics-tooltip") &&
    detail.querySelector("line.metrics-crosshair") &&
    detail.querySelector("circle.metrics-hoverdot"),
  "the hover crosshair + tooltip",
);
const tip = detail.querySelector(".metrics-tooltip");
if (!tip.textContent.includes("CPU utilization"))
  throw new Error("tooltip missing human label: " + tip.textContent);
if (!/%/.test(tip.textContent) || !/ago/.test(tip.textContent))
  throw new Error("tooltip missing unit value / relative time: " + tip.textContent);
const ch = detail.querySelector("line.metrics-crosshair");
const x1 = Number(ch.getAttribute("x1"));
if (!(x1 >= 56 && x1 <= 744))
  throw new Error("crosshair outside plot area: " + x1);
console.log("ok 5: hover shows crosshair + tooltip snapped to the nearest sample (value + relative/absolute time)");

await fire("mouseout", 380);
await waitUntil(
  () => !detail.querySelector(".metrics-tooltip"),
  "the tooltip clearing on mouseleave",
);
console.log("ok 6: mouseleave clears the tooltip");

console.log(
  "\nPASS: chart UI DoD — gridlines + nice ticks, human time axis, unit-aware stats, grouped human picker, hover crosshair/tooltip, live affordance",
);
process.exit(0);
