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

function DeviceRow({ d }) {
  return (
    <tr className={d.online ? "row-on" : "row-off"}>
      <td>
        <div className="host">{d.hostname}</div>
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

export default function Devices({ token, onUnauthorized, focusFilter, focusKey }) {
  const [devices, setDevices] = useState(null);
  const [error, setError] = useState(null);
  const [q, setQ] = useState("");
  const [tick, setTick] = useState(0);

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

  // Force a re-render every 30s so the "Ns ago" labels stay honest.
  useEffect(() => {
    const id = setInterval(() => setTick((t) => t + 1), 30000);
    return () => clearInterval(id);
  }, []);

  const needle = q.trim().toLowerCase();
  const filtered = (devices || []).filter(
    (d) =>
      !needle ||
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
          <input
            className="search"
            type="search"
            placeholder="filter: hostname, id, ip, tag"
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
            <>
              <p>No devices yet.</p>
              <p className="muted">
                Mint a bootstrap token and enroll an agent, or run the e2e:
                <pre className="mono">
                  curl -fsS -X POST http://localhost:8080/admin/bootstrap
                </pre>
              </p>
            </>
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
              {filtered.map((d) => <DeviceRow key={d.id} d={d} />)}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
