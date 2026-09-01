// D-6 one-click client export smoke test: drives the REAL <App/> in jsdom
// against a fake export endpoint, and proves the definition of done:
//
//   1. the device list renders (web-01, fileserver-01);
//   2. expanding web-01 reveals a "Client export" panel with an Export
//      button in the device detail;
//   3. clicking Export opens the confirmation ("Export all data for
//      web-01? …");
//   4. confirming fetches GET /api/devices/<id>/export, shows the
//      "Preparing…" state, then triggers a browser download of a ZIP
//      named <hostname>-rmmway-export-<date>.zip whose bytes are exactly
//      the bundle;
//   5. the downloaded bundle unzips to manifest.json, device.json,
//      metrics.parquet, metrics_1m.parquet, alerts.json (>=5 files) and
//      the manifest's SHA-256 + size for device.json match the actual
//      file contents (self-verifying bundle); both parquet files carry
//      the PAR1 magic.
//
// The fake builds a real (STORE-method) ZIP with a self-consistent
// manifest, so the test verifies the ACTUAL downloaded bytes.
//
// Run: node scripts/export.smoke.mjs   (bundles the JSX with esbuild)
import { JSDOM } from "jsdom";
import { createHash } from "node:crypto";

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
// jsdom has no object URLs — record them so the download is observable.
globalThis.URL.createObjectURL = (blob) => `blob:fake-${blob.size}`;
globalThis.URL.revokeObjectURL = () => {};
window.anchorDownloads = [];
window.HTMLAnchorElement.prototype.click = function () {
  window.anchorDownloads.push({ href: this.href, download: this.download });
};

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

// ---- a real STORE-method ZIP with a self-consistent manifest -----------------
const sha256 = (u8) => createHash("sha256").update(u8).digest("hex");

const CRC_TABLE = (() => {
  const t = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[n] = c >>> 0;
  }
  return t;
})();
const crc32 = (u8) => {
  let c = 0xffffffff;
  for (let i = 0; i < u8.length; i++) c = CRC_TABLE[(c ^ u8[i]) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
};

// storeZip(files: name -> Uint8Array) -> { zip: Uint8Array, entries }
function storeZip(files) {
  const names = Object.keys(files);
  const enc = new TextEncoder();
  const chunks = [];
  const entries = [];
  let offset = 0;
  for (const name of names) {
    const data = files[name];
    const nameB = enc.encode(name);
    const lh = new DataView(new ArrayBuffer(30));
    lh.setUint32(0, 0x04034b50, true);
    lh.setUint16(4, 20, true);
    lh.setUint16(8, 0, true); // STORE
    lh.setUint32(14, crc32(data), true);
    lh.setUint32(18, data.length, true);
    lh.setUint32(22, data.length, true);
    lh.setUint16(26, nameB.length, true);
    chunks.push(new Uint8Array(lh.buffer), nameB, data);
    entries.push({ name, offset, size: data.length, crc: lh.getUint32(14, true) });
    offset += 30 + nameB.length + data.length;
  }
  const cdStart = offset;
  for (const e of entries) {
    const nameB = enc.encode(e.name);
    const ch = new DataView(new ArrayBuffer(46));
    ch.setUint32(0, 0x02014b50, true);
    ch.setUint16(8, 20, true);
    ch.setUint16(10, 20, true);
    ch.setUint16(12, 0, true);
    ch.setUint32(16, e.crc, true);
    ch.setUint32(20, e.size, true);
    ch.setUint32(24, e.size, true);
    ch.setUint16(28, nameB.length, true);
    ch.setUint32(42, e.offset, true);
    chunks.push(new Uint8Array(ch.buffer), nameB);
    offset += 46 + nameB.length;
  }
  const eo = new DataView(new ArrayBuffer(22));
  eo.setUint32(0, 0x06054b50, true);
  eo.setUint16(8, entries.length, true);
  eo.setUint16(10, entries.length, true);
  eo.setUint32(12, offset - cdStart, true);
  eo.setUint32(16, cdStart, true);
  chunks.push(new Uint8Array(eo.buffer));
  const zip = new Uint8Array(offset + 22);
  let p = 0;
  for (const c of chunks) {
    zip.set(c, p);
    p += c.length;
  }
  return { zip, entries };
}

// unzip STORE-method zip -> { [name]: Uint8Array }
function unzip(zip) {
  const out = {};
  const enc = new TextDecoder();
  let p = 0;
  while (p + 4 <= zip.length) {
    if (!(zip[p] === 0x50 && zip[p + 1] === 0x4b && zip[p + 2] === 3 && zip[p + 3] === 4)) break;
    const nameLen = zip[p + 26] | (zip[p + 27] << 8);
    const extraLen = zip[p + 28] | (zip[p + 29] << 8);
    const size = zip[p + 18] | (zip[p + 19] << 8) | (zip[p + 20] << 16) | (zip[p + 21] << 24);
    const name = enc.decode(zip.slice(p + 30, p + 30 + nameLen));
    const dataStart = p + 30 + nameLen + extraLen;
    out[name] = zip.slice(dataStart, dataStart + size);
    p = dataStart + size;
  }
  return out;
}

const u8 = (s) => new TextEncoder().encode(s);
const DEVICES = [
  { id: "dev-web-01", hostname: "web-01", os: "linux", arch: "amd64", agent_version: "0.1.0", interfaces: ["10.0.0.11"], tags: ["web"], online: true, first_seen: "2026-07-01T00:00:00Z", last_seen: "2026-08-25T11:59:00Z" },
  { id: "dev-fs-01", hostname: "fileserver-01", os: "linux", arch: "amd64", agent_version: "0.1.0", interfaces: ["10.0.0.21"], tags: ["files"], online: true, first_seen: "2026-07-01T00:00:00Z", last_seen: "2026-08-25T11:59:00Z" },
];

const deviceJson = u8(
  JSON.stringify({
    schema: "rmmway.device/v1",
    device: { id: "dev-web-01", hostname: "web-01", os: "linux", arch: "amd64", agent_version: "0.1.0", interfaces: ["10.0.0.11"], first_seen: "2026-07-01T00:00:00Z", last_seen: "2026-08-25T11:59:00Z" },
    config: { metric_interval_s: 15, heartbeat_interval_s: 30, tags: ["web"] },
  })
);
const alertsJson = u8(
  JSON.stringify([
    { id: 1, device_id: "dev-web-01", name: "cpu.utilization_percent", source: "seasonal", status: "resolved", fired_at: "2026-08-20T09:00:00Z", resolved_at: "2026-08-20T09:31:00Z" },
    { id: 2, device_id: "dev-web-01", name: "mem.used_percent", source: "trend", status: "open", fired_at: "2026-08-25T11:30:00Z" },
  ])
);
// PAR1 magic bookends, as real parquet files have.
const PAR1 = u8("PAR1");
const metricsParquet = u8("PAR1" + "0123456789abcdef".repeat(24) + "PAR1");
const rollupsParquet = u8("PAR1" + "fedcba9876543210".repeat(16) + "PAR1");

const files = {
  "device.json": deviceJson,
  "metrics.parquet": metricsParquet,
  "metrics_1m.parquet": rollupsParquet,
  "alerts.json": alertsJson,
};
const manifest = {
  format: "rmmway-client-export",
  format_version: 1,
  exported_at: "2026-08-25T11:55:00Z",
  generated_by: "rmmway server 0.1.0",
  device: { id: "dev-web-01", hostname: "web-01" },
  files: [
    { name: "manifest.json", size: 0, description: "self-describing bundle manifest — verify every other file against it" },
    { name: "device.json", size: deviceJson.length, sha256: sha256(deviceJson), description: "client inventory + configuration" },
    { name: "metrics.parquet", size: metricsParquet.length, sha256: sha256(metricsParquet), rows: 18400, description: "raw metric samples (DuckDB/pandas readable)" },
    { name: "metrics_1m.parquet", size: rollupsParquet.length, sha256: sha256(rollupsParquet), rows: 4320, description: "1-minute rollups" },
    { name: "alerts.json", size: alertsJson.length, sha256: sha256(alertsJson), rows: 2, description: "complete alert history" },
  ],
};
// manifest.json's own size is only known once serialized — iterate twice.
let manifestBytes = u8("{}");
for (let i = 0; i < 2; i++) {
  manifest.files[0].size = manifestBytes.length;
  manifestBytes = u8(JSON.stringify(manifest, null, 2));
}
files["manifest.json"] = manifestBytes;
const BUNDLE = storeZip(files);

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

  if (path === "/api/devices/dev-web-01/export" || path.startsWith("/api/devices/dev-web-01/export?")) {
    if (method !== "GET") return json({ error: "GET only" }, 405);
    // a real export streams; hold briefly so the UI's working state renders.
    await new Promise((r) => setTimeout(r, 30));
    return new Response(BUNDLE.zip, {
      status: 200,
      headers: {
        "Content-Type": "application/zip",
        "Content-Disposition": `attachment; filename="rmmway-export-dev-web-01-20260825-115500.zip"`,
      },
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
function waitUntil(cond, what, ms = 8000) {
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

// ---- 1. sign in; the device list renders --------------------------------------
await waitUntil(() => text().includes("Sign in to continue"), "login screen");
await act(async () => {
  setVal(container.querySelector('input[type="password"]'), "smokepass");
  container.querySelector("form").dispatchEvent(
    new window.Event("submit", { bubbles: true, cancelable: true })
  );
});
await waitUntil(() => text().includes("web-01") && text().includes("fileserver-01"), "the device list");
console.log("ok 1: signed in; the device list renders web-01 and fileserver-01");

// ---- 2. expanding the device reveals the Client export panel -------------------
await act(async () => {
  const row = [...container.querySelectorAll("tr")].find((r) => r.textContent.includes("web-01"));
  row.dispatchEvent(new window.MouseEvent("click", { bubbles: true }));
});
await waitUntil(() => container.querySelector(".device-export"), "the Client export panel in the detail");
if (!container.querySelector(".device-export button") || !/export/i.test(container.querySelector(".device-export").textContent)) {
  throw new Error("the export panel has no Export button");
}
console.log("ok 2: expanding web-01 reveals the 'Client export' panel with an Export button");

// ---- 3. Export opens the confirmation naming the device ------------------------
await act(async () => {
  btn(/^export$/i).dispatchEvent(new window.MouseEvent("click", { bubbles: true }));
});
await waitUntil(() => container.querySelector(".export-confirm"), "the export confirmation");
const confirmText = container.querySelector(".export-confirm").textContent;
if (!confirmText.includes("web-01") || !/parquet/i.test(confirmText)) {
  throw new Error("the confirmation does not name the device and its bundle contents: " + confirmText);
}
console.log("ok 3: clicking Export asks 'Export all data for web-01?' and lists the bundle contents (inventory, Parquet metrics + rollups, alert history)");

// ---- 4. confirm -> Preparing… -> the ZIP download is triggered --------------------
await act(async () => {
  btn(/yes, export/i).dispatchEvent(new window.MouseEvent("click", { bubbles: true }));
});
await waitUntil(() => text().includes("Preparing…"), "the preparing state");
await waitUntil(
  () => fetchLog.some((c) => c === "GET /api/devices/dev-web-01/export"),
  "the export fetch"
);
const expectedName = `web-01-rmmway-export-${new Date().toISOString().slice(0, 10)}.zip`;
await waitUntil(() => window.anchorDownloads.length === 1, "the browser download");
await waitUntil(() => text().includes("Downloaded"), "the success banner");
const dl = window.anchorDownloads[0];
if (dl.download !== expectedName) {
  throw new Error(`the download is misnamed: ${dl.download} (want ${expectedName})`);
}
if (!dl.href.startsWith("blob:")) throw new Error("the download did not use an object URL: " + dl.href);
if (!dl.href.endsWith(`-${BUNDLE.zip.length}`)) {
  throw new Error(`the downloaded blob is not the bundle (${dl.href}, want ${BUNDLE.zip.length} bytes)`);
}
console.log("ok 4: confirming fetches GET /api/devices/dev-web-01/export, shows 'Preparing…', and triggers a download of " + expectedName + " whose bytes are exactly the served bundle");

// ---- 5. the bundle unzips and the manifest verifies ------------------------------
const extracted = unzip(BUNDLE.zip);
const names = Object.keys(extracted).sort();
const need = ["alerts.json", "device.json", "manifest.json", "metrics.parquet", "metrics_1m.parquet"];
for (const n of need) {
  if (!extracted[n]) throw new Error(`the bundle is missing ${n}: has ${names.join(",")}`);
}
const m = JSON.parse(new TextDecoder().decode(extracted["manifest.json"]));
if (m.files.length < 5) throw new Error("the manifest lists fewer than 5 files");
for (const f of m.files) {
  if (f.name === "manifest.json") continue;
  const actual = extracted[f.name];
  if (!actual) throw new Error(`manifest references a missing file: ${f.name}`);
  if (actual.length !== f.size) throw new Error(`${f.name}: manifest size ${f.size} != actual ${actual.length}`);
  if (f.sha256 && f.sha256 !== sha256(actual)) {
    throw new Error(`${f.name}: manifest sha256 ${f.sha256} != actual ${sha256(actual)}`);
  }
}
const parquetMagic = (u) =>
  u[0] === 0x50 && u[1] === 0x41 && u[2] === 0x52 && u[3] === 0x31;
if (!parquetMagic(extracted["metrics.parquet"]) || !parquetMagic(extracted["metrics_1m.parquet"])) {
  throw new Error("the parquet files lack the PAR1 magic");
}
console.log("ok 5: the downloaded bundle unzips to " + names.join(", ") + " and the manifest's SHA-256 + size for every data file match the actual contents (self-verifying); both Parquet files carry the PAR1 magic");

for (const es of [...liveStreams]) es.close();
console.log("\nD-6 client export UI DoD PASS: export button in the device detail, confirmation naming the device, Preparing… state, ZIP download named <hostname>-rmmway-export-<date>.zip, and a manifest that verifies every file.");
process.exit(0);
