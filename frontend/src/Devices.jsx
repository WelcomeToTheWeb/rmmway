import { Fragment, useEffect, useState, useCallback, useRef } from "react";
import { api } from "./api.js";

function relTime(iso) {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const s = Math.max(0, (Date.now() - d.getTime()) / 1000);
  if (s < 60) return `${Math.floor(s)}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return d.toLocaleString();
}

function evtTime(ms) {
  const d = new Date(ms);
  if (Number.isNaN(d.getTime())) return "—";
  return (
    d.toLocaleTimeString([], { hour12: false }) +
    "." + String(d.getMilliseconds()).padStart(3, "0")
  );
}

const LEVEL_CLASS = { debug: "lvl-debug", info: "lvl-info", warn: "lvl-warn", error: "lvl-error" };

// W6-1: the expandable per-device "recent indexed events" panel. Polls
// every 3s while open so a live agent's log lines appear as they index.
function DeviceEvents({ token, deviceId, onUnauthorized }) {
  const [events, setEvents] = useState(null);
  const [error, setError] = useState(null);
  const [level, setLevel] = useState("");

  const load = useCallback(async () => {
    try {
      const res = await api.events(token, deviceId, { limit: 100, level });
      setEvents(res.events || []);
      setError(null);
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setError(e.message);
    }
  }, [token, deviceId, level, onUnauthorized]);

  useEffect(() => {
    load();
    const id = setInterval(load, 3000);
    return () => clearInterval(id);
  }, [load]);

  return (
    <div className="device-events">
      <div className="device-events-head">
        <span className="muted">Recent indexed events (W6-1 · also shipped to Loki)</span>
        <select
          className="search"
          value={level}
          onChange={(e) => setLevel(e.target.value)}
          title="Filter by severity"
        >
          <option value="">all levels</option>
          <option value="debug">debug</option>
          <option value="info">info</option>
          <option value="warn">warn</option>
          <option value="error">error</option>
        </select>
      </div>
      {error && <div className="banner err">{error}</div>}
      {events === null && !error ? (
        <div className="empty">Loading events…</div>
      ) : events.length === 0 ? (
        <div className="empty muted">
          No indexed events yet (the agent ships its structured log lines here).
        </div>
      ) : (
        <table className="events">
          <thead>
            <tr>
              <th>Time</th>
              <th>Level</th>
              <th>Message</th>
            </tr>
          </thead>
          <tbody>
            {events.map((ev) => {
              const attrs = ev.attrs
                ? Object.entries(ev.attrs).map(([k, v]) => `${k}=${v}`).join(" ")
                : "";
              return (
                <tr key={ev.id}>
                  <td className="mono evt-time">{evtTime(ev.timestamp_ms)}</td>
                  <td className="mono">
                    <span className={"lvl " + (LEVEL_CLASS[ev.level] || "")}>
                      {(ev.level || "?").toUpperCase()}
                    </span>
                  </td>
                  <td className="evt-msg">
                    <span className="mono">{ev.msg}</span>
                    {attrs && <span className="muted mono evt-attrs"> {attrs}</span>}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </div>
  );
}

// D-1: the per-device "Commands" panel — every command dispatched to this
// device, newest first, with the agent's reported outcome. pending[] rows
// show PENDING (no report yet) or the agent's non-final ack (RECEIVED /
// RUNNING); rows with a final report show its status, and expanding a row
// reveals the reported output (stdout/stderr tail, exit code, error).
// Live updates ride the SSE stream (a command-category envelope bumps
// liveTick -> immediate re-fetch); the manual refresh is the fallback for
// when the stream is down.
const CMD_STATUS = {
  0: ["UNSPECIFIED", "pill-mut"],
  1: ["RECEIVED", "pill-run"],
  2: ["RUNNING", "pill-run"],
  3: ["SUCCEEDED", "pill-ok"],
  4: ["FAILED", "pill-bad"],
  5: ["TIMED_OUT", "pill-bad"],
  6: ["UNSUPPORTED", "pill-bad"],
  7: ["REFUSED", "pill-bad"],
};
const CMD_PENDING = ["PENDING", "pill-mut"];

// The command's action type (run_script/reboot) lives only in the pending
// proto (Action oneof, serialized as { RunScript: {...} } / { Reboot: {} });
// results carry only the outcome, so the action is resolved from there.
function cmdAction(cmd) {
  if (!cmd || !cmd.Action) return null;
  if (cmd.Action.RunScript) {
    const lang = cmd.Action.RunScript.Lang || "sh";
    return `run_script (${lang})`;
  }
  if (cmd.Action.Reboot) return "reboot";
  return null;
}

function cmdStamp(ms) {
  if (!ms) return "—";
  const d = new Date(ms);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toLocaleString();
}

// Merge pending commands with their (possibly non-final) results so each
// command renders as exactly one row: pending wins for identity (it carries
// the action + issued time), the result supplies the freshest status/output.
function mergedCmdRows(pending, results) {
  const byId = new Map();
  for (const r of results || []) byId.set(r.command_id, r);
  const rows = (pending || []).map((c) => ({
    id: c.Id,
    issued: c.IssuedAtMs || 0,
    action: cmdAction(c),
    result: byId.get(c.Id) || null,
  }));
  // Results whose command already left pending[] (history) get a row too —
  // the server keeps the device's full command record.
  const pendingIds = new Set((pending || []).map((c) => c.Id));
  for (const r of results || []) {
    if (!pendingIds.has(r.command_id)) {
      rows.push({
        id: r.command_id,
        issued: r.completed_at_ms || 0,
        action: null,
        result: r,
      });
    }
  }
  return rows.sort((a, b) => (b.issued || 0) - (a.issued || 0));
}

function DeviceCommands({ token, deviceId, onUnauthorized, liveTick }) {
  const [data, setData] = useState(null);
  const [error, setError] = useState(null);
  const [openCmd, setOpenCmd] = useState(null);

  const load = useCallback(async () => {
    try {
      const res = await api.commands(token, deviceId);
      setData(res);
      setError(null);
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setError(e.message);
    }
  }, [token, deviceId, onUnauthorized]);

  useEffect(() => {
    load();
  }, [load]);

  // D-1: a final command result lands on the live stream (App bumps
  // liveTick for command-category events); re-fetch at once instead of
  // waiting for the operator to click refresh. liveTick=0 = initial mount
  // (load() already ran).
  useEffect(() => {
    if (liveTick && liveTick > 0) load();
  }, [liveTick, load]);

  const rows = data === null ? null : mergedCmdRows(data.pending, data.results);

  return (
    <div className="device-commands">
      <div className="device-commands-head">
        <span className="muted">Commands (D-1 · newest first, live over the event stream)</span>
        <button className="btn" onClick={load} title="Refresh now">
          ↻ refresh
        </button>
      </div>
      {error && <div className="banner err">{error}</div>}
      {rows === null && !error ? (
        <div className="empty">Loading commands…</div>
      ) : rows.length === 0 ? (
        <div className="empty muted">No commands dispatched yet.</div>
      ) : (
        <table className="events cmds">
          <thead>
            <tr>
              <th>Dispatched</th>
              <th>Command</th>
              <th>Action</th>
              <th>Status</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((row) => {
              const [label, cls] = row.result ? CMD_STATUS[row.result.status] || [String(row.result.status), "pill-mut"] : CMD_PENDING;
              const open = openCmd === row.id;
              const out = row.result;
              return (
                <Fragment key={row.id}>
                  <tr
                    className={open ? "cmd-row row-open" : "cmd-row"}
                    onClick={() => setOpenCmd(open ? null : row.id)}
                    title="Show/hide the agent's reported output"
                  >
                    <td className="mono evt-time">{cmdStamp(row.issued)}</td>
                    <td className="mono">
                      <span className="chev">{open ? "▾" : "▸"}</span> {row.id}
                    </td>
                    <td>{row.action || <span className="muted">—</span>}</td>
                    <td>
                      <span className={"pill " + cls}>{label}</span>
                    </td>
                  </tr>
                  {open && out && (
                    <tr className="detail-row">
                      <td colSpan={4} className="detail-cell">
                        <div className="cmd-detail mono">
                          {out.exit_code !== undefined && out.exit_code !== null && (
                            <div>exit code: {out.exit_code}</div>
                          )}
                          {out.stdout_tail && <pre className="cmd-out">{out.stdout_tail}</pre>}
                          {out.stderr_tail && (
                            <pre className="cmd-out err">{out.stderr_tail}</pre>
                          )}
                          {out.error && <div className="banner err">error: {out.error}</div>}
                          {!out.stdout_tail && !out.stderr_tail && !out.error && (
                            <div className="muted">The agent reported {label} with no output.</div>
                          )}
                        </div>
                      </td>
                    </tr>
                  )}
                </Fragment>
              );
            })}
          </tbody>
        </table>
      )}
    </div>
  );
}

// Per-device metrics viewer: a series picker (which (name, source) series
// the device has reported), a range selector, and a line chart of the
// server-bucketed samples of the chosen series. The server averages raw
// samples into fixed buckets, so even 30d stays a few hundred points.
const METRIC_RANGES = ["1h", "6h", "24h", "7d", "30d"];

// D-6: one-click client export. One request, one self-verifying ZIP bundle
// (manifest.json + device.json + metrics/rollups Parquet + full alert
// history) downloaded under a hostname-stamped name.
function DeviceExport({ token, device, onUnauthorized }) {
  // phase machine: idle → confirm → preparing → done | error
  const [phase, setPhase] = useState("idle");
  const [result, setResult] = useState(null); // { name, kb }
  const [err, setErr] = useState("");
  const host = device.hostname || device.id;

  const run = async () => {
    setPhase("preparing");
    setErr("");
    try {
      const blob = await api.exportDevice(token, device.id);
      const name = `${host}-rmmway-export-${new Date().toISOString().slice(0, 10)}.zip`;
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = name;
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 10000);
      setResult({ name, kb: Math.max(1, Math.round(blob.size / 1024)) });
      setPhase("done");
    } catch (e) {
      if (e.unauthorized) return onUnauthorized();
      setErr(
        e.status === 503
          ? "Export is not wired on this server (in-memory mode) — start with Postgres to enable client exports."
          : e.message
      );
      setPhase("error");
    }
  };

  return (
    <div className="device-export">
      <div className="device-export-row row-actions">
        <span className="muted">
          Client export (D-6) — one self-verifying ZIP: inventory, raw
          metrics + 1-min rollups (Parquet), full alert history, manifest.
        </span>
        {phase === "idle" && (
          <button className="btn" onClick={() => setPhase("confirm")}>Export</button>
        )}
        {phase === "preparing" && <button className="btn" disabled>Preparing…</button>}
        {phase === "done" && (
          <button className="btn ghost" onClick={() => { setResult(null); setPhase("confirm"); }}>
            Export again
          </button>
        )}
        {phase === "error" && (
          <button className="btn ghost" onClick={() => { setErr(""); setPhase("confirm"); }}>Retry</button>
        )}
      </div>
      {phase === "confirm" && (
        <div className="export-confirm">
          <span>
            Export all data for <strong>{host}</strong>? Includes inventory,
            raw metrics (Parquet), 1-min rollups (Parquet), and full alert
            history.
          </span>
          <span className="row-actions">
            <button className="btn" onClick={run}>Yes, export</button>
            <button className="btn ghost" onClick={() => setPhase("idle")}>Cancel</button>
          </span>
        </div>
      )}
      {phase === "done" && result && (
        <div className="banner ok">
          Downloaded <span className="mono">{result.name}</span> ({result.kb}
          KB) — the manifest inside the ZIP verifies every file.
        </div>
      )}
      {phase === "error" && <div className="banner err">{err}</div>}
    </div>
  );
}

function fmtMetricValue(name, v) {
  if (!Number.isFinite(v)) return "—";
  if (name === "net.bytes_total" || name.endsWith("_bytes_total")) {
    const units = ["B", "KiB", "MiB", "GiB", "TiB"];
    let u = 0;
    let x = v;
    while (x >= 1024 && u < units.length - 1) {
      x /= 1024;
      u++;
    }
    return `${x.toFixed(x >= 100 ? 0 : 1)} ${units[u]}`;
  }
  if (name === "system.uptime_seconds" || name.endsWith("_seconds")) {
    if (v < 86400) return `${Math.round(v / 60)}m`;
    return `${(v / 86400).toFixed(1)}d`;
  }
  if (name.endsWith("_percent")) return `${v.toFixed(1)}%`;
  return Math.abs(v) >= 100 ? String(Math.round(v)) : v.toFixed(1);
}

function MetricChart({ data }) {
  const W = 680,
    H = 150,
    PL = 58,
    PR = 12,
    PT = 10,
    PB = 20;
  const pts = data.points;
  const t0 = pts[0][0];
  const t1 = pts[pts.length - 1][0];
  const span = Math.max(1, t1 - t0);
  let vmin = data.min,
    vmax = data.max;
  if (vmax - vmin < 1e-9) {
    vmin -= 1;
    vmax += 1;
  } // flat line: avoid a zero-height plot
  const x = (t) => PL + ((t - t0) / span) * (W - PL - PR);
  const y = (v) => PT + (1 - (v - vmin) / (vmax - vmin)) * (H - PT - PB);
  const line = pts
    .map(([t, v]) => `${x(t).toFixed(1)},${y(v).toFixed(1)}`)
    .join(" ");
  const fmt = (v) => fmtMetricValue(data.name, v);
  const tLabel = (t) =>
    new Date(t).toLocaleString([], {
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
      hour12: false,
    });
  return (
    <div>
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
        <span className="muted">
          {data.count} samples · {data.range} · {data.bucket_s}s buckets
        </span>
      </div>
      <svg className="metrics-chart" viewBox={`0 0 ${W} ${H}`} role="img" aria-label={`${data.name} over ${data.range}`}>
        <rect
          x={PL}
          y={PT}
          width={W - PL - PR}
          height={H - PT - PB}
          className="metrics-plot"
        />
        <polyline points={line} className="metrics-line" />
        <circle cx={x(t1)} cy={y(data.last)} r={3} className="metrics-last" />
        <text x={PL - 6} y={y(vmax) + 4} textAnchor="end">
          {fmt(vmax)}
        </text>
        <text x={PL - 6} y={y(vmin) + 4} textAnchor="end">
          {fmt(vmin)}
        </text>
        <text x={PL} y={H - 5}>
          {tLabel(t0)}
        </text>
        <text x={W - PR} y={H - 5} textAnchor="end">
          {tLabel(t1)}
        </text>
      </svg>
    </div>
  );
}

function DeviceMetrics({ token, device, onUnauthorized }) {
  const [series, setSeries] = useState(null);
  const [selIdx, setSelIdx] = useState(0);
  const [range, setRange] = useState("24h");
  const [data, setData] = useState(null);
  const [error, setError] = useState(null);
  const selRef = useRef(null); // the selected {name, source}, survives re-lists

  const loadSeries = useCallback(async () => {
    try {
      const res = await api.metricsNames(token, device.id, "7d");
      const list = res.series || [];
      setSeries(list);
      setError(null);
      if (!list.length) return;
      // Keep the current selection if it is still offered; otherwise
      // default to the host-wide CPU series when present.
      const cur = list.findIndex(
        (m) => m.name === selRef.current?.name && m.source === selRef.current?.source
      );
      const cpu = list.findIndex((m) => m.name === "cpu.utilization_percent" && !m.source);
      const pick = cur >= 0 ? cur : cpu >= 0 ? cpu : 0;
      selRef.current = list[pick];
      setSelIdx(pick);
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setError(e.message);
    }
  }, [token, device.id, onUnauthorized]);

  const sel = series && series[selIdx];

  const loadData = useCallback(async () => {
    if (!sel) {
      setData(null);
      return;
    }
    try {
      const res = await api.metricsSeries(token, device.id, sel.name, sel.source, range);
      setData(res);
      setError(null);
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setError(e.message);
    }
  }, [token, device.id, sel, range, onUnauthorized]);

  useEffect(() => {
    loadSeries();
    const id = setInterval(loadSeries, 60000);
    return () => clearInterval(id);
  }, [loadSeries]);

  useEffect(() => {
    loadData();
    const id = setInterval(loadData, 30000);
    return () => clearInterval(id);
  }, [loadData]);

  const onMetricChange = (e) => {
    const i = Number(e.target.value);
    setSelIdx(i);
    if (series) selRef.current = series[i];
  };

  return (
    <div className="device-metrics">
      <div className="device-metrics-head">
        <span className="muted">Metrics</span>
        <select
          className="search"
          value={String(selIdx)}
          onChange={onMetricChange}
          disabled={!series || !series.length}
          title="Which metric to chart"
        >
          {(series || []).map((m, i) => (
            <option key={`${m.name}|${m.source}`} value={i}>
              {m.name}
              {m.source ? ` (${m.source})` : ""}
            </option>
          ))}
          {series && !series.length && <option value={0}>no series</option>}
        </select>
        <select
          className="search"
          value={range}
          onChange={(e) => setRange(e.target.value)}
          title="Time window"
        >
          {METRIC_RANGES.map((r) => (
            <option key={r} value={r}>
              {r}
            </option>
          ))}
        </select>
      </div>
      {error && <div className="banner err">{error}</div>}
      {data && data.points && data.points.length ? (
        <MetricChart data={data} />
      ) : !error ? (
        <div className="empty muted">
          No samples in this window yet (the agent ships metrics every few
          seconds once online).
        </div>
      ) : null}
    </div>
  );
}

// JS base64 of a UTF-8 string (script payloads are short).
function b64(s) {
  return btoa(unescape(encodeURIComponent(s)));
}

// B-2: the per-device tag editor (operator tagging). Chips remove a tag;
// the input adds one. Each change PATCHes the device's WHOLE tag list, so
// the list always matches what the server (and the search index) sees.
function TagEditor({ token, device, onUnauthorized, onSaved }) {
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);

  const apply = async (tags) => {
    setBusy(true);
    setError(null);
    try {
      const res = await api.setTags(token, device.id, tags);
      onSaved(res.device || { ...device, tags });
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setError(e.message);
    } finally {
      setBusy(false);
    }
  };

  function add() {
    const t = draft.trim();
    if (!t || busy) return;
    if ((device.tags || []).includes(t.toLowerCase())) {
      setDraft("");
      return;
    }
    setDraft("");
    apply([...(device.tags || []), t]);
  }

  function remove(tag) {
    if (busy) return;
    apply((device.tags || []).filter((t) => t !== tag));
  }

  return (
    <div className="tag-editor">
      <span className="tag-editor-label muted">Tags (B-2)</span>
      <div className="tags">
        {(device.tags || []).map((t) => (
          <span key={t} className="tag editable" title={t}>
            {t}
            <button
              className="tag-x"
              disabled={busy}
              onClick={() => remove(t)}
              title={`Remove tag ${t}`}
            >
              ×
            </button>
          </span>
        ))}
      </div>
      <div className="tag-editor-add">
        <input
          className="search tag-editor-input"
          type="text"
          placeholder="add a tag (e.g. web, windows-servers)"
          value={draft}
          disabled={busy}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              add();
            }
          }}
        />
        <button className="btn" disabled={busy || !draft.trim()} onClick={add}>
          + add
        </button>
      </div>
      {error && <div className="banner err">{error}</div>}
      {busy && <div className="muted tag-editor-busy">saving…</div>}
    </div>
  );
}

// B-2: "dispatch to a group" — ONE capability-gated command fanned out to
// every device carrying a tag. The server re-checks the session's capability
// (403) and mints a per-device token per pushed command (500-device cap).
const DEFAULT_SCRIPT = "#!/bin/sh\necho RMMWay group script\nuptime";

function GroupDispatchModal({ token, initialTag, onUnauthorized, onClose }) {
  const [tag, setTag] = useState(initialTag || "");
  const [action, setAction] = useState("run_script");
  const [lang, setLang] = useState("sh");
  const [script, setScript] = useState(DEFAULT_SCRIPT);
  const [timeout, setTimeoutS] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);
  const [result, setResult] = useState(null);

  async function send() {
    const body = { tag: tag.trim() };
    if (action === "reboot") {
      body.action = "reboot";
    } else {
      body.action = "run_script";
      body.lang = lang;
      body.script = b64(script);
      if (timeout !== "") body.timeout_s = Number(timeout);
    }
    setBusy(true);
    setError(null);
    try {
      setResult(await api.bulkDispatch(token, body));
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setError(e.message);
    } finally {
      setBusy(false);
    }
  }

  const pushed = (result && result.pushed) || [];
  const offline = (result && result.offline) || [];
  const failed = (result && result.failed) || {};

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div
        className="modal bulk"
        onClick={(e) => e.stopPropagation()}
        role="dialog"
        aria-label="Dispatch to a device group"
      >
        <div className="modal-head">
          <h3>Dispatch to a group</h3>
          <button className="btn ghost" onClick={onClose} title="Close">
            ✕
          </button>
        </div>
        <p className="muted">
          One command, every device carrying the tag — each push carries its
          own per-device capability token (the server gates the session's
          capability first; the fan-out is capped at 500 devices).
        </p>
        <label className="field">
          <span>Tag (group)</span>
          <input
            className="search bulk-tag"
            type="text"
            placeholder="e.g. web, windows-servers"
            value={tag}
            disabled={busy}
            onChange={(e) => setTag(e.target.value)}
          />
        </label>
        <label className="field">
          <span>Action</span>
          <select className="search bulk-action" value={action} disabled={busy} onChange={(e) => setAction(e.target.value)}>
            <option value="run_script">run script</option>
            <option value="reboot">reboot</option>
          </select>
        </label>
        {action === "run_script" && (
          <>
            <label className="field">
              <span>Language</span>
              <select className="search bulk-lang" value={lang} disabled={busy} onChange={(e) => setLang(e.target.value)}>
                <option value="sh">sh</option>
                <option value="powershell">powershell</option>
                <option value="python">python</option>
              </select>
            </label>
            <label className="field">
              <span>Script</span>
              <textarea
                className="bulk-script"
                rows={5}
                value={script}
                disabled={busy}
                onChange={(e) => setScript(e.target.value)}
              />
            </label>
            <label className="field">
              <span>Timeout (s, optional)</span>
              <input
                className="search bulk-timeout"
                type="number"
                min="1"
                placeholder="default"
                value={timeout}
                disabled={busy}
                onChange={(e) => setTimeoutS(e.target.value)}
              />
            </label>
          </>
        )}
        {error && <div className="banner err">{error}</div>}
        <div className="bulk-actions">
          <button className="btn primary" disabled={busy || !tag.trim()} onClick={send}>
            {busy ? "dispatching…" : "Dispatch to whole group"}
          </button>
        </div>
        {result && (
          <div className="bulk-result">
            <p>
              <strong>{result.requested}</strong> matched · {pushed.length} pushed
              {offline.length > 0 && <> · {offline.length} offline</>}
              {Object.keys(failed).length > 0 && (
                <> · {Object.keys(failed).length} failed</>
              )}
            </p>
            {pushed.length > 0 && (
              <ul className="bulk-result-list mono">
                {pushed.map((p) => (
                  <li key={p.device_id}>
                    <span className="muted">{p.device_id}</span> → {p.command_id}
                  </li>
                ))}
              </ul>
            )}
            {offline.length > 0 && (
              <p className="muted mono">offline (no live stream): {offline.join(", ")}</p>
            )}
            {Object.keys(failed).length > 0 && (
              <ul className="bulk-result-list mono">
                {Object.entries(failed).map(([id, err]) => (
                  <li key={id}>
                    <span className="muted">{id}</span> → {err}
                  </li>
                ))}
              </ul>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

function StatusPill({ online, lastSeen }) {
  return (
    <div className="status">
      <span className={"dot " + (online ? "on" : "off")} />
      <span className={"status-text " + (online ? "on" : "off")}>
        {online ? "online" : "offline"}
      </span>
      <span className="status-sub">{relTime(lastSeen)}</span>
    </div>
  );
}

// The install scripts are fetched from the public repo. Override
// INSTALL_BASE (e.g. a self-hosted mirror) to point the one-liner elsewhere.
const INSTALL_BASE =
  (typeof window !== "undefined" && window.__RMMWAY_INSTALL_BASE__) ||
  "https://raw.githubusercontent.com/welcometotheweb/rmmway/Phase-1/scripts";
const INSTALL_SH = `${INSTALL_BASE}/install.sh`;
const INSTALL_PS1 = `${INSTALL_BASE}/install.ps1`;

// Build the copy-paste install one-liners for a minted token + server URL.
// (server + token carry no shell metacharacters in the standard case.)
function linuxInstallCmd(server, token) {
  return `curl -fsSL ${INSTALL_SH} | bash -s -- --server ${server} --bootstrap ${token}`;
}
function windowsInstallCmd(server, token) {
  return `iwr -useb ${INSTALL_PS1} -OutFile install.ps1; powershell -ExecutionPolicy Bypass -File install.ps1 -Server ${server} -Bootstrap ${token}`;
}

// AddDeviceModal is the operator's "Add a device" action. It mints a one-time
// enrollment token (POST /api/bootstrap) and hands the operator a single
// copy-paste command per OS, with the server URL pre-filled from the current
// origin (editable). The device appears in the list the moment the agent
// connects — no raw curl to /admin/bootstrap, no copying a token by hand.
function AddDeviceModal({ token, onUnauthorized, onClose }) {
  const [mint, setMint] = useState(null); // {bootstrap_token, device_id}
  const [error, setError] = useState(null);
  const [server, setServer] = useState(window.location.origin);
  const [copied, setCopied] = useState(null); // which block was last copied

  useEffect(() => {
    let alive = true;
    api
      .bootstrap(token)
      .then((m) => alive && setMint(m))
      .catch((e) => {
        if (!alive) return;
        if (e.unauthorized) onUnauthorized();
        else setError(e.message);
      });
    return () => {
      alive = false;
    };
  }, [token, onUnauthorized]);

  async function copy(text, which) {
    try {
      if (navigator.clipboard && navigator.clipboard.writeText) {
        await navigator.clipboard.writeText(text);
      } else {
        const ta = document.createElement("textarea");
        ta.value = text;
        document.body.appendChild(ta);
        ta.select();
        document.execCommand("copy");
        document.body.removeChild(ta);
      }
      setCopied(which);
      setTimeout(() => setCopied(null), 1500);
    } catch {
      /* clipboard unavailable — the text is still selectable */
    }
  }

  const linux = mint ? linuxInstallCmd(server, mint.bootstrap_token) : "";
  const win = mint ? windowsInstallCmd(server, mint.bootstrap_token) : "";

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal adddev" onClick={(e) => e.stopPropagation()} role="dialog" aria-label="Add a device">
        <div className="modal-head">
          <h3>Add a device</h3>
          <button className="btn ghost" onClick={onClose} title="Close">
            ✕
          </button>
        </div>
        <p className="muted">
          Run this on the machine you want to monitor. It installs the RMMWay
          agent and enrolls it — the device appears in the list the moment it
          connects.
        </p>
        <label className="field">
          <span>Server URL</span>
          <input
            className="search adddev-server"
            value={server}
            onChange={(e) => setServer(e.target.value)}
            title="The operator's public URL. The host + mTLS gRPC port (default 50052) must be reachable from the device."
          />
        </label>
        {error && <div className="banner err">{error}</div>}
        {mint === null && !error && (
          <div className="empty">Minting a one-time enrollment token…</div>
        )}
        {mint && (
          <>
            <div className="adddev-facts mono">
              <div>
                <span className="muted">Device ID</span> {mint.device_id}
              </div>
              <div>
                <span className="muted">Token</span> {mint.bootstrap_token} <span className="muted">(one-time, ~30 min)</span>
              </div>
            </div>
            <div className="adddev-block">
              <div className="adddev-block-head">
                <span>Linux / macOS</span>
                <button className="btn" onClick={() => copy(linux, "linux")}>
                  {copied === "linux" ? "copied ✓" : "copy"}
                </button>
              </div>
              <pre className="mono adddev-cmd">{linux}</pre>
            </div>
            <div className="adddev-block">
              <div className="adddev-block-head">
                <span>Windows (PowerShell)</span>
                <button className="btn" onClick={() => copy(win, "win")}>
                  {copied === "win" ? "copied ✓" : "copy"}
                </button>
              </div>
              <pre className="mono adddev-cmd">{win}</pre>
            </div>
            <p className="muted adddev-note">
              Only the server host + the mTLS gRPC port (default 50052) need to
              be reachable from the device — the plain gRPC bootstrap port stays
              internal. Enrollment runs over the server's HTTPS origin.
            </p>
          </>
        )}
      </div>
    </div>
  );
}

function DeviceRow({ d, open, onToggle }) {
  return (
    <tr
      className={(d.online ? "row-on" : "row-off") + (open ? " row-open" : "")}
      onClick={() => onToggle(d.id)}
      title="Show/hide recent indexed events (W6-1)"
      style={{ cursor: "pointer" }}
    >
      <td>
        <div className="host">
          <span className="chev">{open ? "▾" : "▸"}</span> {d.hostname}
        </div>
        <div className="id">{d.id}</div>
      </td>
      <td className="mono">{d.os}/{d.arch}</td>
      <td className="mono">{d.agent_version || "—"}</td>
      <td className="mono ips">
        {d.interfaces && d.interfaces.length ? d.interfaces.join(", ") : "—"}
      </td>
      <td>
        {d.tags && d.tags.length ? (
          <span className="tags">{d.tags.map((t) => <span key={t} className="tag">{t}</span>)}</span>
        ) : (
          <span className="muted">—</span>
        )}
      </td>
      <td><StatusPill online={d.online} lastSeen={d.last_seen} /></td>
    </tr>
  );
}

export default function Devices({ token, onUnauthorized, focusFilter, focusKey, liveTick }) {
  const [devices, setDevices] = useState(null);
  const [error, setError] = useState(null);
  const [q, setQ] = useState("");
  const [tick, setTick] = useState(0);
  // W6-1: the expanded device (recent indexed events panel below its row).
  const [open, setOpen] = useState(null);
  // The "Add a device" modal (mint a one-time token -> copy-paste installer).
  const [addOpen, setAddOpen] = useState(false);
  // B-2: the "Dispatch to a group" modal (tag group -> bulk fan-out).
  const [bulkOpen, setBulkOpen] = useState(false);
  // B-2: a tag editor save replaces the device row in local state.
  const saveDevice = useCallback((d) => {
    setDevices((list) => (list ? list.map((x) => (x.id === d.id ? d : x)) : list));
  }, []);

  // When the palette triggers "go to device", the parent bumps focusKey and
  // sets focusFilter to the hostname; we sync the local filter here.
  useEffect(() => {
    if (focusKey !== null && focusFilter !== null) {
      setQ(focusFilter);
    }
  }, [focusKey, focusFilter]);

  const load = useCallback(async () => {
    try {
      const list = await api.devices(token);
      setDevices(list);
      setError(null);
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setError(e.message);
    }
  }, [token, onUnauthorized]);

  useEffect(() => {
    load();
    const id = setInterval(load, 5000);
    return () => clearInterval(id);
  }, [load]);

  // B-1: a device online/offline flip arrives on the live stream (App bumps
  // liveTick); re-pull immediately so the status badge flips without waiting
  // for the 5s poll. liveTick=0 is the initial mount (load() already ran).
  useEffect(() => {
    if (liveTick && liveTick > 0) load();
  }, [liveTick, load]);

  // Force a re-render every 30s so the "Ns ago" labels stay honest.
  useEffect(() => {
    const id = setInterval(() => setTick((t) => t + 1), 30000);
    return () => clearInterval(id);
  }, []);

  const needle = q.trim().toLowerCase();
  // B-2: `tag:<name>` in the filter box is an EXACT tag-group filter (the
  // same syntax the palette + /api/search use) — everything else is the
  // substring match over hostname / id / ip / tags.
  const tagOnly = needle.startsWith("tag:");
  const tagNeedle = tagOnly ? needle.slice(4).trim() : "";
  const filtered = (devices || []).filter((d) =>
    tagOnly
      ? (d.tags || []).includes(tagNeedle)
      : !needle ||
        d.hostname.toLowerCase().includes(needle) ||
        d.id.toLowerCase().includes(needle) ||
        (d.interfaces || []).some((ip) => ip.includes(needle)) ||
        (d.tags || []).some((t) => t.toLowerCase().includes(needle))
  );
  const onlineCount = (devices || []).filter((d) => d.online).length;
  const total = (devices || []).length;
  void tick;

  return (
    <section className="view">
      <div className="view-head">
        <div>
          <h2>Devices</h2>
          <p className="muted">
            {devices === null
              ? "loading…"
              : `${total} total · ${onlineCount} online · ${total - onlineCount} offline`}
          </p>
        </div>
        <div className="view-actions">
          <button
            className="btn primary"
            onClick={() => setAddOpen(true)}
            title="Mint a one-time token and get a copy-paste install command"
          >
            + Add device
          </button>
          <button
            className="btn"
            onClick={() => setBulkOpen(true)}
            title="Fan one capability-gated command out to every device carrying a tag (B-2)"
          >
            ⚡ Dispatch to group
          </button>
          <input
            className="search"
            type="search"
            placeholder="filter: hostname, id, ip, tag (or tag:web)"
            value={q}
            onChange={(e) => setQ(e.target.value)}
          />
          <button className="btn" onClick={load} title="Refresh now">
            ↻ refresh
          </button>
        </div>
      </div>

      {error && <div className="banner err">{error}</div>}

      {devices === null && !error ? (
        <div className="empty">Loading devices…</div>
      ) : filtered.length === 0 ? (
        <div className="empty">
          {total === 0 ? (
            <div className="empty empty-adddev">
              <p>No devices yet.</p>
              <p className="muted">
                Add your first device — the modal mints a one-time token and
                gives you a single copy-paste command to run on the machine.
              </p>
              <button className="btn primary" onClick={() => setAddOpen(true)}>
                + Add a device
              </button>
            </div>
          ) : (
            <p>No devices match <em>{q}</em>.</p>
          )}
        </div>
      ) : (
        <div className="table-wrap">
          <table className="devices">
            <thead>
              <tr>
                <th>Host</th>
                <th>OS/Arch</th>
                <th>Agent</th>
                <th>IPs</th>
                <th>Tags</th>
                <th>Status</th>
              </tr>
            </thead>
            <tbody>
              {filtered.map((d) => (
                <Fragment key={d.id}>
                  <DeviceRow
                    d={d}
                    open={open === d.id}
                    onToggle={(id) => setOpen(open === id ? null : id)}
                  />
                  {open === d.id && (
                    <tr className="detail-row">
                      <td colSpan={6} className="detail-cell">
                        <div className="device-detail">
                          <DeviceExport
                            token={token}
                            device={d}
                            onUnauthorized={onUnauthorized}
                          />
                          <TagEditor
                            token={token}
                            device={d}
                            onUnauthorized={onUnauthorized}
                            onSaved={saveDevice}
                          />
                          <DeviceMetrics
                            token={token}
                            device={d}
                            onUnauthorized={onUnauthorized}
                          />
                          <DeviceCommands
                            token={token}
                            deviceId={d.id}
                            onUnauthorized={onUnauthorized}
                            liveTick={liveTick}
                          />
                          <DeviceEvents token={token} deviceId={d.id} onUnauthorized={onUnauthorized} />
                        </div>
                      </td>
                    </tr>
                  )}
                </Fragment>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {addOpen && (
        <AddDeviceModal
          token={token}
          onUnauthorized={onUnauthorized}
          onClose={() => setAddOpen(false)}
        />
      )}

      {bulkOpen && (
        <GroupDispatchModal
          token={token}
          initialTag={tagNeedle}
          onUnauthorized={onUnauthorized}
          onClose={() => setBulkOpen(false)}
        />
      )}
    </section>
  );
}
