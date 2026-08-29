import { useEffect, useState, useCallback } from "react";
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

function fmtNum(n) {
  if (n === null || n === undefined) return "—";
  return (Math.round(n * 100) / 100).toString();
}

// ScoreBadge shows how far the metric is from its baseline.
function ScoreBadge({ score }) {
  const cls = score >= 10 ? "score hot" : score >= 5 ? "score warm" : "score cool";
  return <span className={cls} title="z-score vs baseline">{fmtNum(score)}σ</span>;
}

// AlertRow renders one inbox entry with its triage actions.
function AlertRow({ a, busy, onAction, onUnauthorized }) {
  const [err, setErr] = useState(null);
  const act = async (status) => {
    setErr(null);
    try {
      await onAction(a.id, status);
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setErr(e.message);
    }
  };
  return (
    <tr className={"alert-" + a.status}>
      <td>
        <div className="host">{a.hostname || a.device_id}</div>
        <div className="id">{a.device_id}</div>
      </td>
      <td>
        <div className="host">{a.name}</div>
        <div className="id">
          {a.channel}
          {a.source ? ` · ${a.source}` : ""}
        </div>
      </td>
      <td className="mono">
        <ScoreBadge score={a.score} />{" "}
        <span className="muted" title="observed vs baseline expected">
          {fmtNum(a.value)} ≈ {fmtNum(a.expected)}
        </span>
      </td>
      <td className="mono">
        <span className="events" title="anomaly passes folded into this alert (deduped)">
          ×{a.events}
        </span>
      </td>
      <td>
        <span className={"pill pill-" + a.status}>{a.status}</span>
        {err && <div className="row-err">{err}</div>}
      </td>
      <td className="muted" title="first seen">{relTime(a.first_at)}</td>
      <td className="muted" title="last anomaly pass">{relTime(a.last_at)}</td>
      <td>
        {busy ? (
          <span className="muted">…</span>
        ) : a.status === "open" ? (
          <div className="row-actions">
            <button className="btn ghost" title="Mark acknowledged (stays in inbox)" onClick={() => act("acked")}>
              ack
            </button>
            <button className="btn" title="Mark resolved (leaves inbox)" onClick={() => act("resolved")}>
              resolve
            </button>
          </div>
        ) : a.status === "acked" ? (
          <div className="row-actions">
            <button className="btn" title="Mark resolved (leaves inbox)" onClick={() => act("resolved")}>
              resolve
            </button>
          </div>
        ) : (
          <span className="muted">
            {a.resolved_at ? `resolved ${relTime(a.resolved_at)}` : "—"}
          </span>
        )}
      </td>
    </tr>
  );
}

const TABS = [
  { key: "open", label: "Open" },
  { key: "acked", label: "Acknowledged" },
  { key: "resolved", label: "Resolved" },
  { key: "", label: "All" },
];

// Alerts is the W2-4 inbox: baseline-driven anomalies folded into one
// deduped alert per (device, metric, source). Open alerts auto-resolve when
// the series returns to baseline (or are acked/resolved manually).
export default function Alerts({ token, onUnauthorized }) {
  const [alerts, setAlerts] = useState(null);
  const [counts, setCounts] = useState(null);
  const [error, setError] = useState(null);
  const [tab, setTab] = useState("open");
  const [q, setQ] = useState("");
  const [busy, setBusy] = useState(false);
  const [tick, setTick] = useState(0);

  const statusParam = tab === "All" ? "" : tab;

  const load = useCallback(async () => {
    try {
      const [list, c] = await Promise.all([
        api.alerts(token, { status: statusParam }),
        api.alertCounts(token),
      ]);
      setAlerts(list);
      setCounts(c);
      setError(null);
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setError(e.message);
    }
  }, [token, statusParam, onUnauthorized]);

  useEffect(() => {
    load();
    const id = setInterval(load, 10000);
    return () => clearInterval(id);
  }, [load]);

  // Keep "Ns ago" labels honest.
  useEffect(() => {
    const id = setInterval(() => setTick((t) => t + 1), 30000);
    return () => clearInterval(id);
  }, []);

  const act = useCallback(
    async (id, status) => {
      setBusy(true);
      try {
        await api.setAlertStatus(token, id, status);
        await load();
      } finally {
        setBusy(false);
      }
    },
    [token, load]
  );

  void tick;
  const needle = q.trim().toLowerCase();
  const filtered = (alerts || []).filter(
    (a) =>
      !needle ||
      (a.hostname || "").toLowerCase().includes(needle) ||
      a.device_id.toLowerCase().includes(needle) ||
      a.name.toLowerCase().includes(needle)
  );

  return (
    <section className="view">
      <div className="view-head">
        <div>
          <h2>Alerts</h2>
          <p className="muted">
            {alerts === null
              ? "loading…"
              : counts
              ? `${counts.open} open · ${counts.acked} acknowledged · ${counts.resolved} resolved`
              : "baseline-driven inbox — one alert per anomalous series"}
          </p>
        </div>
        <div className="view-actions">
          <input
            className="search"
            type="search"
            placeholder="filter: host, device, metric"
            value={q}
            onChange={(e) => setQ(e.target.value)}
          />
          <button className="btn" onClick={load} title="Refresh now">
            ↻ refresh
          </button>
        </div>
      </div>

      <div className="tabs" role="tablist">
        {TABS.map((t) => (
          <button
            key={t.key || "all"}
            role="tab"
            aria-selected={tab === t.key}
            className={"tab" + (tab === t.key ? " active" : "")}
            onClick={() => setTab(t.key)}
          >
            {t.label}
            {t.key === "open" && counts && counts.open > 0 && (
              <span className="badge">{counts.open}</span>
            )}
          </button>
        ))}
      </div>

      {error && <div className="banner err">{error}</div>}

      {alerts === null && !error ? (
        <div className="empty">Loading alerts…</div>
      ) : filtered.length === 0 ? (
        <div className="empty">
          {statusParam === "open" ? (
            <>
              <p>No open alerts.</p>
              <p className="muted">
                The baseline engine scores every metric hourly; when one drifts
                off its (day-of-week, hour-of-day) expectation it lands here as
                a single deduped alert — and auto-resolves once the metric
                returns to baseline.
              </p>
            </>
          ) : (
            <p>No {tab === "All" ? "" : tab + " "}alerts match.</p>
          )}
        </div>
      ) : (
        <div className="table-wrap">
          <table className="devices alerts">
            <thead>
              <tr>
                <th>Host</th>
                <th>Metric</th>
                <th>Deviation</th>
                <th>Passes</th>
                <th>Status</th>
                <th>First</th>
                <th>Last</th>
                <th>Actions</th>
              </tr>
            </thead>
            <tbody>
              {filtered.map((a) => (
                <AlertRow key={a.id} a={a} busy={busy} onAction={act} onUnauthorized={onUnauthorized} />
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
