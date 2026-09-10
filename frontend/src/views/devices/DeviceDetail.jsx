// Device detail surfaces: what renders inside the expanded device row
// (export, tags, metrics, commands, agent log). Extracted from Devices.jsx
// (UI revamp Phase 3) — DOM classes are the smoke-test contract and stay
// byte-identical; the readability pass lands in styles (devices.css).
import {
  Fragment,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { api } from "../../api.js";
import CodeBlock from "../../ui/CodeBlock.jsx";
import { EmptyState } from "../../ui/index.js";
import TimeSeriesChart, {
  humanizeMetric,
  metricCategory,
  METRIC_CATEGORY_ORDER,
} from "../../ui/TimeSeriesChart.jsx";
import Tabs from "../../ui/Tabs.jsx";
import DeviceInventory from "./DeviceInventory.jsx";
import DeviceIdentity from "./DeviceIdentity.jsx";
import QuickHealth from "./QuickHealth.jsx";
import InventorySnapshot from "./InventorySnapshot.jsx";
import MultiMetricChart from "./MultiMetricChart.jsx";
import CorrelatedMetrics from "./CorrelatedMetrics.jsx";

// ---- agent log (recent indexed events) ------------------------------------

function evtTime(ms) {
  const d = new Date(ms);
  if (Number.isNaN(d.getTime())) return "—";
  return (
    d.toLocaleTimeString([], { hour12: false }) +
    "." +
    String(d.getMilliseconds()).padStart(3, "0")
  );
}

const LEVEL_CLASS = {
  debug: "lvl-debug",
  info: "lvl-info",
  warn: "lvl-warn",
  error: "lvl-error",
};

// W6-1: the expandable per-device "recent indexed events" panel. Polls
// every 3s while open so a live agent's log lines appear as they index.
function DeviceEvents({ token, deviceId, onUnauthorized }) {
  const [events, setEvents] = useState(null);
  const [error, setError] = useState(null);
  const [level, setLevel] = useState("");
  const [text, setText] = useState("");

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

  const needle = text.trim().toLowerCase();
  const visible = (events || []).filter(
    (ev) =>
      !needle ||
      (ev.msg || "").toLowerCase().includes(needle) ||
      Object.entries(ev.attrs || {})
        .map(([k, v]) => `${k}=${v}`)
        .join(" ")
        .toLowerCase()
        .includes(needle),
  );

  return (
    <div className="device-events">
      <div className="device-events-head">
        <span className="muted">
          Agent log — recent entries (also shipped to Loki)
          {events && <span className="muted"> · {events.length} shown</span>}
        </span>
        <div className="device-events-filters">
          <input
            className="search"
            type="search"
            placeholder="filter log lines"
            value={text}
            onChange={(e) => setText(e.target.value)}
          />
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
      </div>
      {error && <div className="banner err">{error}</div>}
      {events === null && !error ? (
        <EmptyState title="Loading events…" icon="📋" />
      ) : events.length === 0 ? (
        <EmptyState
          title="No log entries yet"
          body="The agent ships its log lines here."
          icon="📋"
        />
      ) : visible.length === 0 ? (
        <EmptyState title="No log lines match the filter." icon="🔍" />
      ) : (
        <div className="device-events-scroll">
          <table className="events">
            <thead>
              <tr>
                <th>Time</th>
                <th>Level</th>
                <th>Message</th>
              </tr>
            </thead>
            <tbody>
              {visible.map((ev) => {
                const attrs = ev.attrs
                  ? Object.entries(ev.attrs)
                      .map(([k, v]) => `${k}=${v}`)
                      .join(" ")
                  : "";
                return (
                  <tr key={ev.id}>
                    <td className="mono evt-time">
                      {evtTime(ev.timestamp_ms)}
                    </td>
                    <td className="mono">
                      <span className={"lvl " + (LEVEL_CLASS[ev.level] || "")}>
                        {(ev.level || "?").toUpperCase()}
                      </span>
                    </td>
                    <td className="evt-msg">
                      <span className="mono">{ev.msg}</span>
                      {attrs && (
                        <span className="muted mono evt-attrs"> {attrs}</span>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

// ---- commands ---------------------------------------------------------------

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
        <span className="muted">
          Commands — newest first, updates live as agents report
        </span>
        <button className="btn" onClick={load} title="Refresh now">
          ↻ refresh
        </button>
      </div>
      {error && <div className="banner err">{error}</div>}
      {rows === null && !error ? (
        <EmptyState title="Loading commands…" icon="⚡" />
      ) : rows.length === 0 ? (
        <EmptyState title="No commands dispatched yet." icon="⚡" />
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
              const [label, cls] = row.result
                ? CMD_STATUS[row.result.status] || [
                    String(row.result.status),
                    "pill-mut",
                  ]
                : CMD_PENDING;
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
                          {out.exit_code !== undefined &&
                            out.exit_code !== null && (
                              <div className="cmd-exit">
                                exit code: {out.exit_code}
                              </div>
                            )}
                          {out.stdout_tail && (
                            <div className="cmd-outwrap">
                              <span className="muted">stdout</span>
                              <CodeBlock
                                code={out.stdout_tail}
                                className="cmd-out-block"
                              />
                            </div>
                          )}
                          {out.stderr_tail && (
                            <div className="cmd-outwrap">
                              <span className="muted">stderr</span>
                              <CodeBlock
                                code={out.stderr_tail}
                                className="cmd-out-block err"
                              />
                            </div>
                          )}
                          {out.error && (
                            <div className="banner err">error: {out.error}</div>
                          )}
                          {!out.stdout_tail &&
                            !out.stderr_tail &&
                            !out.error && (
                              <div className="muted">
                                The agent reported {label} with no output.
                              </div>
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

// ---- client export ------------------------------------------------------------

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
          ? "Device export needs the full server stack (database) to run — start the production stack to enable it."
          : e.message,
      );
      setPhase("error");
    }
  };

  return (
    <div className="device-export">
      <div className="device-export-row row-actions">
        <span className="muted">
          Export this device's full data as one self-verifying ZIP — inventory,
          metrics (Parquet), full alert history.
        </span>
        {phase === "idle" && (
          <button className="btn" onClick={() => setPhase("confirm")}>
            Export
          </button>
        )}
        {phase === "preparing" && (
          <button className="btn" disabled>
            Preparing…
          </button>
        )}
        {phase === "done" && (
          <button
            className="btn ghost"
            onClick={() => {
              setResult(null);
              setPhase("confirm");
            }}
          >
            Export again
          </button>
        )}
        {phase === "error" && (
          <button
            className="btn ghost"
            onClick={() => {
              setErr("");
              setPhase("confirm");
            }}
          >
            Retry
          </button>
        )}
      </div>
      {phase === "confirm" && (
        <div className="export-confirm">
          <span>
            Export all data for <strong>{host}</strong>? Includes inventory, raw
            metrics (Parquet), 1-min rollups (Parquet), and full alert history.
          </span>
          <span className="row-actions">
            <button className="btn" onClick={run}>
              Yes, export
            </button>
            <button className="btn ghost" onClick={() => setPhase("idle")}>
              Cancel
            </button>
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

// ---- metrics viewer -------------------------------------------------------------

const METRIC_RANGES = ["1h", "6h", "24h", "7d", "30d"];

// Per-device metrics viewer: a series picker (which (name, source) series
// the device has reported), a range selector, and a line chart of the
// server-bucketed samples of the chosen series. The server averages raw
// samples into fixed buckets, so even 30d stays a few hundred points.
function DeviceMetrics({ token, device, onUnauthorized }) {
  const [series, setSeries] = useState(null);
  const [selIdx, setSelIdx] = useState(0);
  const [data, setData] = useState(null);
  const [error, setError] = useState(null);
  const selRef = useRef(null); // the selected {name, source}, survives re-lists

  // Auto-detect the best default range based on device age (Phase 3.2)
  const defaultRange = useMemo(() => {
    if (!device.first_seen) return "24h";
    const deviceAgeMs = Date.now() - new Date(device.first_seen).getTime();
    const deviceAgeHours = deviceAgeMs / 3600000;
    if (deviceAgeHours < 2) return "1h";
    if (deviceAgeHours < 24) return "6h";
    if (deviceAgeHours < 168) return "24h";
    return "7d";
  }, [device.first_seen]);

  const [range, setRange] = useState(defaultRange);

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
        (m) =>
          m.name === selRef.current?.name &&
          m.source === selRef.current?.source,
      );
      const cpu = list.findIndex(
        (m) => m.name === "cpu.utilization_percent" && !m.source,
      );
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
      const res = await api.metricsSeries(
        token,
        device.id,
        sel.name,
        sel.source,
        range,
      );
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

  // Group the picker by category (CPU / Memory / Disk / …) while keeping
  // every option's value = its ORIGINAL index in the server list, so the
  // selection math (and the smoke contract) is order-independent.
  const groups = useMemo(() => {
    const byCat = new Map();
    (series || []).forEach((m, i) => {
      const c = metricCategory(m.name);
      if (!byCat.has(c)) byCat.set(c, []);
      byCat.get(c).push({ m, i });
    });
    const order = METRIC_CATEGORY_ORDER.filter((c) => byCat.has(c));
    for (const c of byCat.keys()) if (!order.includes(c)) order.push(c);
    return order.map((c) => ({ cat: c, items: byCat.get(c) }));
  }, [series]);

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
          {groups.map((g) => (
            <optgroup key={g.cat} label={g.cat}>
              {g.items.map(({ m, i }) => (
                <option key={`${m.name}|${m.source}`} value={i}>
                  {m.name}
                  {m.source ? ` (${m.source})` : ""}
                </option>
              ))}
            </optgroup>
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
        <TimeSeriesChart
          data={data}
          humanName={sel ? humanizeMetric(sel.name, sel.source) : undefined}
        />
      ) : error ? null : (
        <div className="empty muted">
          No samples in this window yet (the agent ships metrics every few
          seconds once online).
        </div>
      )}
    </div>
  );
}

// ---- tags ---------------------------------------------------------------------------

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
      <span className="tag-editor-label muted">Tags</span>
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

// ---- composition -----------------------------------------------------------------------

// The expanded device row's contents: export, tags, metrics, commands, and
// the recent agent log. Rendered inside the <tr class="detail-row"> of the
// device table (Devices.jsx keeps the table itself).
export default function DeviceDetail({
  token,
  device,
  onUnauthorized,
  liveTick,
  onSaved,
  defaultTab,
}) {
  const [tab, setTab] = useState(defaultTab || "overview");

  const tabs = [
    { key: "overview", label: "Overview" },
    { key: "metrics", label: "Metrics" },
    { key: "multi-metric", label: "Multi-Metric" },
    { key: "correlated", label: "Correlated" },
    { key: "inventory", label: "Inventory" },
    { key: "commands", label: "Commands" },
    { key: "events", label: "Events" },
  ];

  return (
    <div className="device-detail">
      <div className="device-detail-actions">
        <span className="muted">
          Quick actions for {device.hostname || device.id}
        </span>
        <div className="device-detail-btns">
          <a
            href={`#/session/${device.id}`}
            className="btn btn-primary"
            title="Open a remote session to this device"
          >
            🔌 Connect
          </a>
          <button
            className="btn"
            title="Run commands on this device"
            onClick={() => {
              document.dispatchEvent(
                new CustomEvent("device-open-tab", {
                  detail: { id: device.id, tab: "commands" },
                }),
              );
            }}
          >
            ⚡ Command
          </button>
        </div>
      </div>
      <Tabs tabs={tabs} value={tab} onChange={setTab} />
      <div className="device-detail-tab-content">
        {tab === "overview" && (
          <>
            <DeviceIdentity device={device} clients={null} />
            <QuickHealth
              token={token}
              device={device}
              onUnauthorized={onUnauthorized}
            />
            <InventorySnapshot
              token={token}
              device={device}
              onUnauthorized={onUnauthorized}
              onCollect={onSaved}
            />
            <TagEditor
              token={token}
              device={device}
              onUnauthorized={onUnauthorized}
              onSaved={onSaved}
            />
            <DeviceExport
              token={token}
              device={device}
              onUnauthorized={onUnauthorized}
            />
          </>
        )}
        {tab === "metrics" && (
          <DeviceMetrics
            token={token}
            device={device}
            onUnauthorized={onUnauthorized}
          />
        )}
        {tab === "multi-metric" && (
          <MultiMetricChart
            token={token}
            device={device}
            onUnauthorized={onUnauthorized}
          />
        )}
        {tab === "correlated" && (
          <CorrelatedMetrics
            token={token}
            device={device}
            onUnauthorized={onUnauthorized}
          />
        )}
        {tab === "inventory" && (
          <DeviceInventory
            token={token}
            device={device}
            onUnauthorized={onUnauthorized}
          />
        )}
        {tab === "commands" && (
          <DeviceCommands
            token={token}
            deviceId={device.id}
            onUnauthorized={onUnauthorized}
            liveTick={liveTick}
          />
        )}
        {tab === "events" && (
          <DeviceEvents
            token={token}
            deviceId={device.id}
            onUnauthorized={onUnauthorized}
          />
        )}
      </div>
    </div>
  );
}
