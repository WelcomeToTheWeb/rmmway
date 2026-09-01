import { useCallback, useEffect, useMemo, useState } from "react";
import { api } from "./api.js";

// D-5: the baseline anomaly explorer. Every metric is scored against its
// own device's seasonal + trend baseline; this page is the current
// landscape of those scores — worst first, filterable by device / metric /
// minimum severity, with a manual recompute.
//
// NOTE (deviation, W2-4): the spec assumed the anomaly `score` is 0-1; the
// engine scores in z-space (how many robust sigmas the reading sits from
// its own baseline: max of the seasonal and trend channel z's). The table
// shows the raw z and bands it: <3σ calm, 3-6σ elevated, ≥6σ severe.
// Filtering (device/metric/min-score) is applied client-side — the current
// server query honors only `limit` — so the results are identical, the
// request just carries the params the server will honor later.
const fmtAt = (s) => {
  if (!s) return "";
  const d = new Date(s);
  return Number.isNaN(d.getTime()) ? String(s) : d.toLocaleString();
};
const fmtZ = (z) => (z == null ? "—" : z.toFixed(1) + "σ");
const band = (score) => (score >= 6 ? "severe" : score >= 3 ? "elevated" : "calm");

export default function Baseline({ token, onUnauthorized, onGoToDevice }) {
  const [devices, setDevices] = useState([]);
  const [anomalies, setAnomalies] = useState(null);
  const [deviceId, setDeviceId] = useState("");
  const [metric, setMetric] = useState("");
  const [minScore, setMinScore] = useState("");
  const [error, setError] = useState(null);
  const [notWired, setNotWired] = useState(false);
  const [running, setRunning] = useState(false);
  const [lastRun, setLastRun] = useState(null); // { anomalies, series, runs }

  const hostnames = useMemo(
    () => Object.fromEntries((devices || []).map((d) => [d.id, d.hostname])),
    [devices]
  );

  useEffect(() => {
    let alive = true;
    api.devices(token).then((list) => alive && setDevices(list || [])).catch(() => {});
    return () => { alive = false; };
  }, [token]);

  const load = useCallback(
    (dev, met, min) => {
      api.baselineAnomalies(token, { device_id: dev, name: met, min_score: min })
        .then((list) => {
          setAnomalies(list || []);
          setNotWired(false);
        })
        .catch((e) => {
          if (e.unauthorized) onUnauthorized();
          else if (e.status === 503) setNotWired(true);
          else setError(e.message);
        });
    },
    [token, onUnauthorized]
  );

  useEffect(() => {
    load(deviceId, metric, minScore);
  }, [load, deviceId, metric, minScore]);

  // The server returns newest first; the operator cares about severity —
  // sort worst first, then by most recent.
  const visible = useMemo(() => {
    const min = minScore === "" ? null : Number(minScore);
    return (anomalies || [])
      .filter(
        (a) =>
          (!deviceId || a.device_id === deviceId) &&
          (!metric || a.name === metric) &&
          (min == null || Number.isNaN(min) || a.score >= min)
      )
      .sort((x, y) => y.score - x.score || new Date(y.at) - new Date(x.at));
  }, [anomalies, deviceId, metric, minScore]);

  const recompute = useCallback(async () => {
    setRunning(true);
    setError(null);
    try {
      const r = await api.baselineRun(token);
      setLastRun(r);
      load(deviceId, metric, minScore);
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setError(e.message);
    } finally {
      setRunning(false);
    }
  }, [token, onUnauthorized, load, deviceId, metric, minScore]);

  if (notWired) {
    return (
      <div className="view">
        <h2>Baseline</h2>
        <div className="empty">
          The baseline engine is not wired on this server (in-memory mode) —
          start with Postgres to enable per-device baselines.
        </div>
      </div>
    );
  }

  return (
    <div className="view">
      <div className="view-head">
        <div>
          <h2>Baseline</h2>
          <p className="muted">
            Every metric scored against its own device's seasonal + trend
            baseline — the readings furthest from normal, worst first.
          </p>
        </div>
        <div className="row-actions">
          {lastRun && (
            <span className="muted baseline-run-summary">
              last recompute: {lastRun.anomalies?.length ?? 0} anomalies across{" "}
              {lastRun.series} series
            </span>
          )}
          <button className="btn" onClick={recompute} disabled={running}>
            {running ? "Scoring fleet…" : "Recompute"}
          </button>
        </div>
      </div>

      {error && <div className="banner err">{error}</div>}

      <div className="baseline-filters row-actions">
        <select value={deviceId} onChange={(e) => setDeviceId(e.target.value)}>
          <option value="">all devices</option>
          {(devices || []).map((d) => (
            <option key={d.id} value={d.id}>{d.hostname || d.id}</option>
          ))}
        </select>
        <input
          className="baseline-metric-input"
          placeholder="filter metric (e.g. cpu.utilization_percent)"
          value={metric}
          onChange={(e) => setMetric(e.target.value)}
        />
        <label className="check muted">
          min score
          <input
            type="number"
            step="any"
            value={minScore}
            onChange={(e) => setMinScore(e.target.value)}
          />
        </label>
      </div>

      <section className="baseline-panel">
        <div className="table-wrap">
          <table className="baseline-table">
            <thead>
              <tr>
                <th>Device</th>
                <th>Metric</th>
                <th>Source</th>
                <th>Value</th>
                <th>Score</th>
                <th>Channel</th>
                <th>Last computed</th>
              </tr>
            </thead>
            <tbody>
              {visible.map((a) => (
                <tr key={a.id} className="baseline-row">
                  <td>
                    <a
                      className="baseline-devlink"
                      href="#/devices"
                      onClick={(e) => {
                        e.preventDefault();
                        if (onGoToDevice) onGoToDevice(a.device_id, hostnames[a.device_id] || a.device_id);
                      }}
                    >
                      {hostnames[a.device_id] || a.device_id}
                    </a>
                  </td>
                  <td className="mono">{a.name}</td>
                  <td className="muted">{a.source}</td>
                  <td className="mono">{a.value}</td>
                  <td>
                    <span className={"pill score score-" + band(a.score)}>
                      {a.score.toFixed(1)}σ
                    </span>
                  </td>
                  <td className="muted">
                    {a.channel}
                    {a.seasonal_z != null && ` · season ${fmtZ(a.seasonal_z)}`}
                    {a.trend_z != null && ` · trend ${fmtZ(a.trend_z)}`}
                  </td>
                  <td className="muted mono">{fmtAt(a.detected_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {anomalies && visible.length === 0 && (
            <div className="empty">
              No anomalies match the current filters
              {anomalies.length === 0 ? " — the fleet is at baseline" : ""}.
            </div>
          )}
        </div>
      </section>
    </div>
  );
}
