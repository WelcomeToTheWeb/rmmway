// Kit time-series chart: a pure-SVG line chart for the per-device metrics
// viewer (zero deps). Renders "nice" y ticks with gridlines, human x-axis
// time ticks, a <polyline> series with a soft area fill, a hover crosshair
// + tooltip that snaps to the nearest sample (value + unit, absolute and
// relative time), a stats row (now/min/max/avg/p95 + samples/range/bucket
// meta) and a "live · updated Ns ago" affordance.
//
// Input: the bucketed payload of GET /api/devices/{id}/metrics/series —
//   { name, source, range, bucket_s, count, min, max, last,
//     points: [[t_ms, v], ...] ascending }
//
// Hover: the smoke harness (jsdom) dispatches a bare mousemove on the <svg>.
// jsdom's getBoundingClientRect is degenerate (zero width), so when the rect
// has no width the clientX is used directly as an SVG user unit — the
// nearest-sample math then stays deterministic for the fixture.
import { useEffect, useMemo, useRef, useState } from "react";
import RelTime from "./RelTime.jsx";

// Fixed logical coordinate system; CSS scales the svg (width 100%, height
// auto) so the viewBox ratio is preserved.
const W = 760;
const H = 280;
const PL = 56; // left padding (y labels)
const PR = 16; // right padding
const PT = 14; // top padding
const PB = 28; // bottom padding (x labels)

// ---- units -----------------------------------------------------------------

// Infer the display unit from the metric name suffix (the server keeps the
// dotted names; operators never see them raw on the chart surface).
function metricUnit(name) {
  const n = name || "";
  if (n.endsWith("_percent")) return "percent";
  if (
    n.endsWith("_bytes_total") ||
    n.endsWith("_bytes") ||
    n === "net.bytes_total"
  )
    return "bytes";
  if (n.endsWith("_seconds")) return "seconds";
  if (n.endsWith("_ratio")) return "ratio";
  if (n.endsWith("_total") || n.endsWith("_count")) return "count";
  return "number";
}

// Full value formatting (stats row, tooltip): unit-aware.
export function fmtMetricValue(name, v) {
  if (v === null || v === undefined || !Number.isFinite(v)) return "—";
  switch (metricUnit(name)) {
    case "percent":
      return `${v.toFixed(1)}%`;
    case "bytes": {
      const units = ["B", "KiB", "MiB", "GiB", "TiB"];
      let x = Math.abs(v);
      let u = 0;
      while (x >= 1024 && u < units.length - 1) {
        x /= 1024;
        u++;
      }
      return `${x >= 100 ? Math.round(x) : x.toFixed(1)} ${units[u]}`;
    }
    case "seconds":
      return v < 86400
        ? `${Math.round(v / 60)}m`
        : `${(v / 86400).toFixed(1)}d`;
    case "ratio":
      return `${v.toFixed(2)}×`;
    default:
      return Math.abs(v) >= 100 ? String(Math.round(v)) : v.toFixed(1);
  }
}

// Compact tick-label formatting (short, no units — the axis context carries
// them): 1234 -> 1.2k, 3400000 -> 3.4M.
function fmtCompact(v) {
  if (!Number.isFinite(v)) return "—";
  if (v === 0) return "0";
  const a = Math.abs(v);
  const f = (x) => {
    const s = x.toFixed(1);
    return s.endsWith(".0") ? s.slice(0, -2) : s;
  };
  if (a >= 1e9) return f(v / 1e9) + "B";
  if (a >= 1e6) return f(v / 1e6) + "M";
  if (a >= 1e3) return f(v / 1e3) + "k";
  if (a >= 10) return String(Math.round(v));
  return String(Math.round(v * 100) / 100);
}

// Y tick labels get the unit suffix only where it is not self-evident
// (percent); byte magnitudes read through k/M/G, seconds through m/d.
function yTickLabel(name, v) {
  const unit = metricUnit(name);
  if (unit === "percent") return `${fmtCompact(v)}%`;
  if (unit === "seconds") {
    if (Math.abs(v) >= 86400)
      return `${(v / 86400).toFixed(v % 86400 === 0 ? 0 : 1)}d`;
    if (Math.abs(v) >= 3600) return `${(v / 3600).toFixed(0)}h`;
    return fmtCompact(v);
  }
  return fmtCompact(v);
}

// ---- human labels ------------------------------------------------------------

// Category used to group the series picker (CPU / Memory / Disk / Network /
// Process / System / Other).
export function metricCategory(name) {
  const n = (name || "").toLowerCase();
  if (n.startsWith("cpu")) return "CPU";
  if (n.startsWith("mem") || n.includes("memory") || n.startsWith("swap"))
    return "Memory";
  if (n.startsWith("disk") || n.startsWith("fs.") || n.startsWith("volume"))
    return "Disk";
  if (
    n.startsWith("net") ||
    n.startsWith("network") ||
    n.startsWith("interface")
  )
    return "Network";
  if (n.startsWith("process") || n.startsWith("proc.")) return "Process";
  if (n.startsWith("system") || n.startsWith("host")) return "System";
  return "Other";
}

export const METRIC_CATEGORY_ORDER = [
  "CPU",
  "Memory",
  "Disk",
  "Network",
  "Process",
  "System",
  "Other",
];

// Hand-written labels for the metrics the agents actually ship; everything
// else falls back to a derived "Category <leaf>" label.
const METRIC_LABELS = {
  "cpu.utilization_percent": "CPU utilization",
  "cpu.idle_percent": "CPU idle",
  "cpu.load1": "CPU load (1m)",
  "mem.used_percent": "Memory used",
  "mem.available_percent": "Memory available",
  "mem.used_bytes_total": "Memory used",
  "disk.used_percent": "Disk used",
  "disk.free_percent": "Disk free",
  "disk.used_bytes_total": "Disk used",
  "net.rx_bytes_total": "Network in",
  "net.tx_bytes_total": "Network out",
  "system.uptime_seconds": "System uptime",
  "system.load1": "System load (1m)",
};

export function humanizeMetric(name, source) {
  let label = METRIC_LABELS[name];
  if (!label) {
    const parts = (name || "").split(".");
    const leaf = parts[parts.length - 1] || name;
    const cleaned = leaf
      .replace(/_percent$/, "")
      .replace(/_bytes_total$/, "")
      .replace(/_bytes$/, "")
      .replace(/_seconds$/, "")
      .replace(/_total$/, "")
      .replace(/_/g, " ")
      .trim();
    const title = cleaned.replace(/\b\w/g, (c) => c.toUpperCase());
    const cat = metricCategory(name);
    label = title ? `${cat} ${title}` : cat;
  }
  return source ? `${label} (${source})` : label;
}

// ---- scales ------------------------------------------------------------------

// "Nice" round-number y ticks (4-7 of them) covering [min, max]. A constant
// series is padded by 10% of its magnitude (or 1) so the plot has height
// without the old flat-line hack.
function niceTicks(min, max, forceZeroFloor) {
  let lo = min;
  let hi = max;
  if (forceZeroFloor && lo >= 0) lo = 0;
  if (!Number.isFinite(lo) || !Number.isFinite(hi))
    return { ticks: [0, 1], lo: 0, hi: 1 };
  if (hi - lo < 1e-9) {
    const pad = Math.max(Math.abs(lo) * 0.1, 1);
    lo -= pad;
    hi += pad;
  }
  for (const target of [5, 4, 6]) {
    const step0 = (hi - lo) / target;
    const mag = 10 ** Math.floor(Math.log10(step0));
    const norm = step0 / mag;
    const step = (norm >= 5 ? 10 : norm >= 2 ? 5 : norm >= 1 ? 2 : 1) * mag;
    const from = Math.floor(lo / step) * step;
    const to = Math.ceil(hi / step) * step;
    const ticks = [];
    for (let v = from; v <= to + step * 1e-6; v += step) {
      ticks.push(Math.round(v * 1e6) / 1e6);
    }
    if (ticks.length >= 3 && ticks.length <= 7) {
      return { ticks, lo: from, hi: to };
    }
  }
  const ticks = [lo, hi];
  return { ticks, lo, hi };
}

// Human time steps (ms): pick the largest step whose tick count stays under
// `target` for the visible span.
const TIME_STEPS = [
  60e3, // 1m
  5 * 60e3,
  15 * 60e3,
  30 * 60e3,
  3600e3, // 1h
  3 * 3600e3,
  6 * 3600e3,
  12 * 3600e3,
  24 * 3600e3, // 1d
  2 * 24 * 3600e3,
  7 * 24 * 3600e3,
  30 * 24 * 3600e3,
];

function timeTicks(t0, t1, target = 5) {
  const span = Math.max(1, t1 - t0);
  let step = TIME_STEPS[TIME_STEPS.length - 1];
  for (const s of TIME_STEPS) {
    if (span / s <= target) {
      step = s;
      break;
    }
  }
  const first = Math.ceil(t0 / step) * step;
  const ticks = [];
  for (let t = first; t <= t1; t += step) ticks.push(t);
  return { ticks, step };
}

// HH:MM within one day; date-prefixed once the window spans days (midnight
// ticks show just the date).
function xTickLabel(t, step, t0, t1) {
  const d = new Date(t);
  const hm = d.toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
  const spanDays = (t1 - t0) / 86400e3;
  if (spanDays <= 1) return hm;
  const md = d.toLocaleDateString([], { month: "short", day: "numeric" });
  if (d.getHours() === 0 && d.getMinutes() === 0) return md;
  return `${md} ${hm}`;
}

function absTime(t) {
  return new Date(t).toLocaleString([], {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
}

function relTime(t) {
  const s = Math.max(0, (Date.now() - t) / 1000);
  if (s < 60) return `${Math.floor(s)}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

function percentile(values, p) {
  if (!values.length) return NaN;
  const s = [...values].sort((a, b) => a - b);
  const idx = Math.min(s.length - 1, Math.max(0, Math.ceil(p * s.length) - 1));
  return s[idx];
}

// ---- component -----------------------------------------------------------------

export default function TimeSeriesChart({ data, humanName }) {
  const svgRef = useRef(null);
  const [hover, setHover] = useState(null); // nearest point index
  const [updatedAt, setUpdatedAt] = useState(() => Date.now());
  const [, forceTick] = useState(0);

  const pts = data.points || [];
  const name = data.name || "";

  useEffect(() => {
    setUpdatedAt(Date.now());
  }, [data]);

  // Keep the "updated Ns ago" label honest between polls.
  useEffect(() => {
    const id = setInterval(() => forceTick((n) => n + 1), 10000);
    return () => clearInterval(id);
  }, []);

  const geom = useMemo(() => {
    if (!pts.length) return null;
    const t0raw = pts[0][0];
    const t1raw = pts[pts.length - 1][0];
    // A single sample gets a rendered domain around it so the point sits
    // mid-plot instead of pinning a zero-width window.
    const t0 = t1raw === t0raw ? t0raw - 3600e3 : t0raw;
    const t1 = t1raw === t0raw ? t0raw + 3600e3 : t1raw;
    const span = Math.max(1, t1 - t0);

    const values = pts.map((p) => p[1]);
    const vmin = Math.min(...values);
    const vmax = Math.max(...values);
    const { ticks, lo, hi } = niceTicks(
      vmin,
      vmax,
      metricUnit(name) === "percent",
    );

    const x = (t) => PL + ((t - t0) / span) * (W - PL - PR);
    const y = (v) => PT + (1 - (v - lo) / (hi - lo)) * (H - PT - PB);

    const line = pts
      .map(([t, v]) => `${x(t).toFixed(1)},${y(v).toFixed(1)}`)
      .join(" ");
    const area =
      `${x(pts[0][0]).toFixed(1)},${(H - PB).toFixed(1)} ` +
      pts.map(([t, v]) => `${x(t).toFixed(1)},${y(v).toFixed(1)}`).join(" ") +
      ` ${x(pts[pts.length - 1][0]).toFixed(1)},${(H - PB).toFixed(1)}`;

    const { ticks: xticks, step } = timeTicks(t0, t1);

    return {
      t0,
      t1,
      span,
      ticks,
      lo,
      hi,
      x,
      y,
      line,
      area,
      xticks,
      step,
      avg: values.reduce((a, b) => a + b, 0) / values.length,
      p95: percentile(values, 0.95),
    };
  }, [pts, name]);

  const onMove = (e) => {
    if (!geom || !pts.length) return;
    const svg = svgRef.current;
    let fx;
    if (svg) {
      const rect = svg.getBoundingClientRect();
      if (rect && rect.width > 0) {
        // Uniform scale (viewBox ratio preserved): ratio across the rect is
        // the ratio across the viewBox.
        fx = ((e.clientX - rect.left) / rect.width) * W;
      } else {
        fx = e.clientX; // jsdom: zero-width rect — clientX as user units
      }
    } else {
      fx = e.clientX;
    }
    const plotX = Math.max(PL, Math.min(W - PR, fx));
    const tAt = geom.t0 + ((plotX - PL) / (W - PL - PR)) * geom.span;
    // Points are bucket-aligned (even spacing); nearest by time.
    let best = 0;
    let bestD = Infinity;
    for (let i = 0; i < pts.length; i++) {
      const d = Math.abs(pts[i][0] - tAt);
      if (d < bestD) {
        bestD = d;
        best = i;
      }
    }
    setHover(best);
  };

  const human = humanName || humanizeMetric(name, data.source);
  const fmt = (v) => fmtMetricValue(name, v);
  const hoverPt = hover !== null && pts[hover] ? pts[hover] : null;
  const tooltipLeft = hoverPt
    ? Math.max(10, Math.min(90, (geom.x(hoverPt[0]) / W) * 100))
    : 0;

  return (
    <div className="metrics-wrap">
      <div className="metrics-title">
        <span className="metrics-human">{human}</span>
        <code className="mono metrics-rawname">{name}</code>
        <span className="metrics-live">
          <span className="metrics-live-dot" />
          live
          <span className="muted">
            · updated <RelTime iso={new Date(updatedAt).toISOString()} />
          </span>
        </span>
      </div>
      <div className="metrics-stats">
        <span>
          now <strong className="mono">{fmt(data.last)}</strong>
        </span>
        <span>
          min <span className="mono">{fmt(data.min)}</span>
        </span>
        <span>
          max <span className="mono">{fmt(data.max)}</span>
        </span>
        <span>
          avg <span className="mono">{fmt(geom ? geom.avg : NaN)}</span>
        </span>
        <span>
          p95 <span className="mono">{fmt(geom ? geom.p95 : NaN)}</span>
        </span>
        <span className="muted">
          {data.count} samples · {data.range} · {data.bucket_s}s buckets
        </span>
      </div>
      {geom ? (
        <div className="metrics-plotwrap">
          <svg
            ref={svgRef}
            className="metrics-chart"
            viewBox={`0 0 ${W} ${H}`}
            role="img"
            aria-label={`${human} over ${data.range}`}
            onMouseMove={onMove}
            onMouseLeave={() => setHover(null)}
          >
            <rect
              x={PL}
              y={PT}
              width={W - PL - PR}
              height={H - PT - PB}
              className="metrics-plot"
            />
            {geom.ticks.map((t) => (
              <line
                key={`g${t}`}
                className="metrics-grid"
                x1={PL}
                x2={W - PR}
                y1={geom.y(t)}
                y2={geom.y(t)}
              />
            ))}
            {geom.ticks.map((t) => (
              <text
                key={`yt${t}`}
                className="metrics-ytick"
                x={PL - 8}
                y={geom.y(t) + 3}
                textAnchor="end"
              >
                {yTickLabel(name, t)}
              </text>
            ))}
            {geom.xticks.map((t) => (
              <text
                key={`xt${t}`}
                className="metrics-xtick"
                x={Math.max(PL, Math.min(W - PR, geom.x(t)))}
                y={H - PB + 18}
                textAnchor="middle"
              >
                {xTickLabel(t, geom.step, geom.t0, geom.t1)}
              </text>
            ))}
            <polygon className="metrics-area" points={geom.area} />
            <polyline className="metrics-line" points={geom.line} />
            <circle
              className="metrics-last"
              cx={geom.x(pts[pts.length - 1][0])}
              cy={geom.y(pts[pts.length - 1][1])}
              r={3}
            />
            {hoverPt && (
              <>
                <line
                  className="metrics-crosshair"
                  x1={geom.x(hoverPt[0])}
                  x2={geom.x(hoverPt[0])}
                  y1={PT}
                  y2={H - PB}
                />
                <circle
                  className="metrics-hoverdot"
                  cx={geom.x(hoverPt[0])}
                  cy={geom.y(hoverPt[1])}
                  r={4}
                />
              </>
            )}
          </svg>
          {hoverPt && (
            <div
              className="metrics-tooltip"
              style={{ left: `${tooltipLeft}%` }}
            >
              <div className="metrics-tooltip-title">{human}</div>
              <div className="metrics-tooltip-value mono">
                {fmt(hoverPt[1])}
                <span className="muted"> · {relTime(hoverPt[0])}</span>
              </div>
              <div className="muted metrics-tooltip-time">
                {absTime(hoverPt[0])}
              </div>
            </div>
          )}
        </div>
      ) : (
        <div className="empty muted">No samples in this window yet.</div>
      )}
    </div>
  );
}
