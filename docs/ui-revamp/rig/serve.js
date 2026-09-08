// Static host + fake RMMWay API for UI evidence screenshots.
// Usage: DIST=/path/to/dist PORT=8123 node serve.js
import http from "node:http";
import { readFile } from "node:fs/promises";
import { join, extname } from "node:path";

const DIST = process.env.DIST;
const PORT = Number(process.env.PORT || 8123);

// ---- fixtures ---------------------------------------------------------------
const DEVICES = [
  {
    id: "dev-web01",
    hostname: "web-01",
    os: "linux",
    arch: "amd64",
    agent_version: "0.4.2",
    interfaces: ["10.0.4.11"],
    tags: ["web", "prod"],
    online: true,
    first_seen: "2026-05-02T09:14:00Z",
    last_seen: new Date(Date.now() - 4000).toISOString(),
  },
  {
    id: "dev-db01",
    hostname: "db-01",
    os: "linux",
    arch: "amd64",
    agent_version: "0.4.2",
    interfaces: ["10.0.4.21"],
    tags: ["db", "prod"],
    online: true,
    first_seen: "2026-05-02T09:20:00Z",
    last_seen: new Date(Date.now() - 6000).toISOString(),
  },
  {
    id: "dev-fs01",
    hostname: "win-fs-01",
    os: "windows",
    arch: "amd64",
    agent_version: "0.4.2",
    interfaces: ["10.0.4.31"],
    tags: ["files"],
    online: true,
    first_seen: "2026-05-15T11:02:00Z",
    last_seen: new Date(Date.now() - 9000).toISOString(),
  },
  {
    id: "dev-gw03",
    hostname: "iot-gw-03",
    os: "linux",
    arch: "arm64",
    agent_version: "0.4.1",
    interfaces: ["10.0.9.3"],
    tags: ["iot"],
    online: false,
    first_seen: "2026-06-01T08:00:00Z",
    last_seen: new Date(Date.now() - 42 * 60 * 60 * 1000).toISOString(),
  },
];
const SERIES = [
  { name: "cpu.utilization_percent", source: "" },
  { name: "cpu.load1", source: "" },
  { name: "mem.used_percent", source: "" },
  { name: "mem.used_bytes_total", source: "" },
  { name: "disk.used_percent", source: "sda1" },
  { name: "net.rx_bytes_total", source: "" },
  { name: "net.tx_bytes_total", source: "" },
  { name: "system.uptime_seconds", source: "" },
];

// Deterministic-ish wave per metric so screenshots look alive but stable.
function value(name, t, i) {
  const day = (t % 86400e3) / 86400e3;
  const jitter = (Math.sin(i * 12.9898) * 43758.5453) % 1;
  switch (name) {
    case "cpu.utilization_percent":
      return clamp(
        34 + 22 * Math.sin(day * 2 * Math.PI - 1.2) + jitter * 9,
        2,
        99,
      );
    case "cpu.load1":
      return Math.max(0.1, (34 + 22 * Math.sin(day * 2 * Math.PI - 1.2)) / 32);
    case "mem.used_percent":
      return clamp(
        61 + 7 * Math.sin(day * 2 * Math.PI * 2) + jitter * 3,
        20,
        97,
      );
    case "mem.used_bytes_total":
      return (
        16 *
        1024 ** 3 *
        (0.55 + 0.1 * Math.sin(day * 2 * Math.PI) + jitter * 0.02)
      );
    case "disk.used_percent":
      return clamp(71.2 + i * 0.004 + jitter * 0.15, 0, 100);
    case "net.rx_bytes_total":
      return (
        220000 +
        180000 * Math.abs(Math.sin(i * 0.35)) +
        Math.abs(jitter) * 90000
      );
    case "net.tx_bytes_total":
      return (
        90000 +
        60000 * Math.abs(Math.sin(i * 0.21 + 1)) +
        Math.abs(jitter) * 40000
      );
    case "system.uptime_seconds":
      return 14 * 86400 + (t - (Date.now() - 86400e3)) / 1000;
    default:
      return 50;
  }
}
const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));

const RANGES = {
  "1h": [3600e3, 60],
  "6h": [6 * 3600e3, 300],
  "24h": [24 * 3600e3, 600],
  "7d": [7 * 86400e3, 3600],
  "30d": [30 * 86400e3, 14400],
};
function seriesPayload(deviceId, name, source, range) {
  const [span, bucket] = RANGES[range] || RANGES["24h"];
  const n = Math.max(12, Math.min(160, Math.round(span / (bucket * 1000))));
  const now = Date.now();
  const pts = [];
  for (let i = 0; i < n; i++) {
    const t = now - (n - 1 - i) * bucket * 1000;
    pts.push([t, Number(value(name, t, i).toFixed(2))]);
  }
  const vals = pts.map((p) => p[1]);
  return {
    device_id: deviceId,
    name,
    source,
    range,
    bucket_s: bucket,
    count: n,
    min: Math.min(...vals),
    max: Math.max(...vals),
    last: vals[vals.length - 1],
    points: pts,
  };
}

const EVENTS = [
  ["info", "agent started", { pid: 4211, version: "0.4.2" }],
  ["info", "metrics pipeline online", { interval_ms: 5000 }],
  ["warn", "disk sda1 above 70%", { device: "sda1", percent: 71.2 }],
  ["info", "command run_script dispatched", { command_id: "c-9f21" }],
  [
    "info",
    "command run_script succeeded",
    { command_id: "c-9f21", exit_code: 0 },
  ],
  ["error", "log rotate failed", { unit: "nginx", error: "EBUSY" }],
  ["info", "heartbeat", { latency_ms: 3 }],
  ["warn", "cpu load elevated", { load1: 4.2, cores: 4 }],
].map(([level, msg, attrs], i) => ({
  id: `ev-${i}`,
  level,
  msg,
  attrs,
  timestamp_ms: Date.now() - (8 - i) * 47000,
}));

const COMMANDS = {
  pending: [
    {
      Id: "c-a7f3",
      IssuedAtMs: Date.now() - 42000,
      Action: { RunScript: { Script: "systemctl restart nginx", Lang: "sh" } },
    },
  ],
  results: [
    {
      command_id: "c-9f21",
      status: 3,
      exit_code: 0,
      stdout_tail: "nginx restarted\nactive (running)",
      stderr_tail: "",
      error: "",
      completed_at_ms: Date.now() - 35 * 60 * 1000,
    },
    {
      command_id: "c-41c8",
      status: 4,
      exit_code: 2,
      stdout_tail: "",
      stderr_tail: "journalctl: unrecognized option '--since=42d'",
      error: "",
      completed_at_ms: Date.now() - 5 * 3600e3,
    },
  ],
};

// ---- server -------------------------------------------------------------------
const json = (res, obj, status = 200) => {
  res.writeHead(status, { "Content-Type": "application/json" });
  res.end(JSON.stringify(obj));
};
const MIME = {
  ".html": "text/html",
  ".js": "text/javascript",
  ".css": "text/css",
  ".svg": "image/svg+xml",
  ".woff2": "font/woff2",
  ".png": "image/png",
  ".ico": "image/x-icon",
};

http
  .createServer(async (req, res) => {
    const url = new URL(req.url, "http://x");
    const p = url.pathname;
    try {
      if (p === "/api/setup/status")
        return json(res, { available: true, setup: true });
      if (p === "/api/login") {
        let body = "";
        for await (const c of req) body += c;
        const u = JSON.parse(body || "{}");
        if (u.username === "admin")
          return json(res, {
            token: "evidence-token",
            expiry: "2030-01-01T00:00:00Z",
            capabilities: [],
          });
        return json(res, { error: "invalid username or password" }, 401);
      }
      if (p === "/api/devices") return json(res, DEVICES);
      if (p === "/api/alerts/counts")
        return json(res, { open: 2, acked: 1, resolved: 0 });
      if (p.startsWith("/api/alerts")) return json(res, []);
      if (p === "/healthz")
        return json(res, { ok: true, version: "0.4.2", probes: { db: "ok" } });
      let m;
      if ((m = /^\/api\/devices\/([^/]+)\/metrics\/series$/.exec(p))) {
        const dev = DEVICES.find((d) => d.id === m[1]);
        if (!dev) return json(res, { error: "unknown device" }, 404);
        const name = url.searchParams.get("name") || "";
        const src = url.searchParams.get("source") || "";
        const hit = SERIES.find((s) => s.name === name && s.source === src);
        if (!hit) return json(res, { error: "no such series" }, 404);
        return json(
          res,
          seriesPayload(
            dev.id,
            name,
            src,
            url.searchParams.get("range") || "24h",
          ),
        );
      }
      if ((m = /^\/api\/devices\/([^/]+)\/metrics$/.exec(p))) {
        const dev = DEVICES.find((d) => d.id === m[1]);
        if (!dev) return json(res, { error: "unknown device" }, 404);
        const range = url.searchParams.get("range") || "7d";
        const list = dev.id === "dev-gw03" ? [] : SERIES;
        return json(res, {
          device_id: dev.id,
          range,
          series: list.map((s) => {
            const pay = seriesPayload(dev.id, s.name, s.source, "24h");
            return {
              name: s.name,
              source: s.source,
              last: pay.last,
              count: pay.count,
            };
          }),
        });
      }
      if ((m = /^\/api\/devices\/([^/]+)\/commands$/.exec(p)))
        return json(
          res,
          m[1] === "dev-web01" ? COMMANDS : { pending: [], results: [] },
        );
      if (p === "/api/events" || p.startsWith("/api/events?")) {
        const dev = url.searchParams.get("device");
        return json(res, { events: dev === "dev-gw03" ? [] : EVENTS });
      }
      if ((m = /^\/api\/devices\/([^/]+)\/events$/.exec(p)))
        return json(res, {
          device_id: m[1],
          events: m[1] === "dev-gw03" ? [] : EVENTS,
        });
      // static
      let file = join(DIST, p === "/" ? "index.html" : p);
      if (!file.startsWith(DIST)) file = join(DIST, "index.html");
      const body = await readFile(file).catch(async () =>
        readFile(join(DIST, "index.html")),
      );
      res.writeHead(200, {
        "Content-Type": MIME[extname(file)] || "application/octet-stream",
      });
      res.end(body);
    } catch (e) {
      res.writeHead(500, { "Content-Type": "text/plain" });
      res.end(String(e));
    }
  })
  .listen(PORT, () =>
    console.log(`evidence server on :${PORT} serving ${DIST}`),
  );
