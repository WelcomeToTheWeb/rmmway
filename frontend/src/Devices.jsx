import { Fragment, useEffect, useState, useCallback } from "react";
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

export default function Devices({ token, onUnauthorized, focusFilter, focusKey }) {
  const [devices, setDevices] = useState(null);
  const [error, setError] = useState(null);
  const [q, setQ] = useState("");
  const [tick, setTick] = useState(0);
  // W6-1: the expanded device (recent indexed events panel below its row).
  const [open, setOpen] = useState(null);

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
                        <DeviceEvents token={token} deviceId={d.id} onUnauthorized={onUnauthorized} />
                      </td>
                    </tr>
                  )}
                </Fragment>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
