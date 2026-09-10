import { useCallback, useEffect, useMemo, useState } from "react";
import { api } from "./api.js";

// ---- fleet dashboard home (gap #8a, wave 2 round 2, lane C) -------------------
// The new default route: one screen, the whole fleet's health. Every tile
// composes EXISTING endpoints client-side (one /api/devices call feeds the
// donut + OS bars + the hostname map; one /api/alerts pair; one
// /api/baseline/anomalies; one /api/events journal page; one /api/tickets
// call for ticket counts; per-device inventory checks for compliance) — no
// new server surface. Every tile degrades: a fetch failure renders a muted
// "unavailable" note inside the tile, never a broken grid.

const REFRESH_MS = 30000; // devices + alert counts re-poll (live-ish home)

function relTime(iso) {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const s = Math.max(0, (Date.now() - d.getTime()) / 1000);
  if (s < 60) return `${Math.floor(s)}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

// Shorten rmmway.events.<group>.<name> -> <group>.<name> for the compact
// activity rows.
function shortType(type) {
  return (type || "").replace(/^rmmway\.events\./, "");
}

// SVG donut, two segments (online / offline) — stroke-dasharray on a full
// circle; no chart library (the app has none).
function Donut({ online, offline }) {
  const total = online + offline;
  const r = 42;
  const c = 2 * Math.PI * r;
  const onFrac = total > 0 ? online / total : 0;
  return (
    <div
      className="donut"
      role="img"
      aria-label={`${online} online, ${offline} offline`}
    >
      <svg viewBox="0 0 120 120" aria-hidden="true">
        <circle
          className="donut-off"
          cx="60"
          cy="60"
          r={r}
          fill="none"
          strokeWidth="14"
          strokeDasharray={`${c} ${c}`}
          transform="rotate(-90 60 60)"
        />
        <circle
          className="donut-on"
          cx="60"
          cy="60"
          r={r}
          fill="none"
          strokeWidth="14"
          strokeDasharray={`${c * onFrac} ${c}`}
          strokeLinecap={total > 0 && online < total ? "butt" : "butt"}
          transform="rotate(-90 60 60)"
        />
      </svg>
      <div className="donut-center">
        <span className="donut-total">{total}</span>
        <span className="muted">devices</span>
      </div>
    </div>
  );
}

export default function Dashboard({ token, onUnauthorized }) {
  const [devices, setDevices] = useState(null);
  const [devicesErr, setDevicesErr] = useState(null);
  const [counts, setCounts] = useState(null);
  const [countsErr, setCountsErr] = useState(null);
  const [alerts, setAlerts] = useState(null);
  const [alertsErr, setAlertsErr] = useState(null);
  const [anomalies, setAnomalies] = useState(null);
  const [anomaliesErr, setAnomaliesErr] = useState(null);
  const [journal, setJournal] = useState(null);
  const [journalErr, setJournalErr] = useState(null);
  // Wave 3 live data (tickets #7, inventory #4)
  const [tickets, setTickets] = useState(null);
  const [ticketsErr, setTicketsErr] = useState(null);
  const [ticketCounts, setTicketCounts] = useState(null);
  const [ticketCountsErr, setTicketCountsErr] = useState(null);
  const [patchInfo, setPatchInfo] = useState(null);
  const [patchErr, setPatchErr] = useState(null);

  const unauthorized = useCallback(
    (e) => e && e.unauthorized && onUnauthorized(),
    [onUnauthorized],
  );

  // The "live" pair: devices feed the donut + OS bars + the hostname maps in
  // the anomaly/activity tiles; the alert counts feed the alerts tile header.
  const loadLive = useCallback(async () => {
    api
      .devices(token)
      .then((list) => {
        setDevices(list || []);
        setDevicesErr(null);
      })
      .catch((e) => {
        if (!unauthorized(e)) setDevicesErr(e.message);
      });
    api
      .alertCounts(token)
      .then((c) => {
        setCounts(c || null);
        setCountsErr(null);
      })
      .catch((e) => {
        if (!unauthorized(e)) setCountsErr(e.message);
      });
  }, [token, unauthorized]);

  // The slower tiles: open alerts, top anomalies, recent activity.
  const loadStatic = useCallback(async () => {
    api
      .alerts(token, { status: "open", limit: 5 })
      .then((list) => {
        setAlerts(list || []);
        setAlertsErr(null);
      })
      .catch((e) => {
        if (!unauthorized(e)) setAlertsErr(e.message);
      });
    api
      .baselineAnomalies(token, { limit: 5 })
      .then((list) => {
        setAnomalies(list || []);
        setAnomaliesErr(null);
      })
      .catch((e) => {
        if (!unauthorized(e)) setAnomaliesErr(e.message);
      });
    api
      .eventJournal(token, { limit: 12 })
      .then((list) => {
        setJournal((list || []).slice().reverse()); // server is oldest-first
        setJournalErr(null);
      })
      .catch((e) => {
        if (!unauthorized(e)) setJournalErr(e.message);
      });
  }, [token, unauthorized]);

  // Wave 3: tickets (gap #7) — live open/in_progress tickets + status counts.
  const loadTickets = useCallback(async () => {
    try {
      const res = await api.get(
        "/api/tickets?status=open,in_progress&limit=5",
      );
      setTickets(res.data || []);
      setTicketsErr(null);
    } catch (e) {
      if (!unauthorized(e)) setTicketsErr(e.message);
    }
    // Status breakdown for the header pills
    const statuses = ["open", "in_progress", "resolved", "closed"];
    const counts = {};
    let anyErr = false;
    for (const s of statuses) {
      try {
        const res = await api.get(
          `/api/tickets?status=${s}&limit=1000`,
        );
        counts[s] = res.data ? res.data.length : 0;
      } catch (e) {
        if (!unauthorized(e)) {
          counts[s] = null;
          anyErr = true;
        }
      }
    }
    if (anyErr) {
      setTicketCounts(null);
      setTicketCountsErr("partial count failure");
    } else {
      setTicketCounts(counts);
      setTicketCountsErr(null);
    }
  }, [token, unauthorized]);

  // Wave 3: inventory compliance (gap #4) — sample a few devices' last
  // inventory collection to derive a compliance view.
  const loadPatchInfo = useCallback(async () => {
    if (!devices || devices.length === 0) return;
    const results = [];
    let err = null;
    for (const d of devices.slice(0, 5)) {
      try {
        const res = await api.deviceInventory(token, d.id);
        results.push({
          id: d.id,
          hostname: d.hostname,
          online: d.online,
          collected_at: res.collected_at,
          software_count: res.software ? res.software.length : 0,
        });
      } catch (e) {
        if (!unauthorized(e)) err = err || e.message;
        results.push({ id: d.id, hostname: d.hostname, online: d.online });
      }
    }
    const now = Date.now();
    const weekMs = 7 * 86400000;
    const withRecent = results.filter(
      (r) => r.collected_at && now - new Date(r.collected_at).getTime() < weekMs,
    ).length;
    setPatchInfo({
      devices_checked: results.length,
      devices_with_inventory: results.filter((r) => r.collected_at).length,
      devices_recent: withRecent,
      details: results,
    });
    setPatchErr(err);
  }, [token, devices, unauthorized]);

  useEffect(() => {
    loadLive();
    loadStatic();
    loadTickets();
    const id = setInterval(loadLive, REFRESH_MS);
    return () => clearInterval(id);
  }, [loadLive, loadStatic, loadTickets]);

  // Re-check patch info when devices change.
  useEffect(() => {
    if (devices && devices.length > 0) loadPatchInfo();
  }, [devices, loadPatchInfo]);

  useEffect(() => {
    loadLive();
    loadStatic();
    const id = setInterval(loadLive, REFRESH_MS);
    return () => clearInterval(id);
  }, [loadLive, loadStatic]);

  const hostById = useMemo(() => {
    const m = new Map();
    (devices || []).forEach((d) => m.set(d.id, d.hostname));
    return m;
  }, [devices]);
  const hostOf = useCallback(
    (id) => (id ? hostById.get(id) || id : "—"),
    [hostById],
  );

  // Derived fleet stats (one devices call, several tiles).
  const online = (devices || []).filter((d) => d.online).length;
  const offline = (devices || []).length - online;
  const byOs = useMemo(() => {
    const m = new Map();
    (devices || []).forEach((d) =>
      m.set(d.os || "unknown", (m.get(d.os || "unknown") || 0) + 1),
    );
    return [...m.entries()].sort((a, b) => b[1] - a[1]);
  }, [devices]);

  const devState = devicesErr
    ? { error: devicesErr }
    : devices === null
      ? null
      : {};
  const alertState = alertsErr
    ? { error: alertsErr }
    : countsErr
      ? { error: countsErr }
      : alerts === null
        ? null
        : {};
  const anomState = anomaliesErr
    ? { error: anomaliesErr }
    : anomalies === null
      ? null
      : {};
  const journalState = journalErr
    ? { error: journalErr }
    : journal === null
      ? null
      : {};
  const ticketState = ticketsErr
    ? { error: ticketsErr }
    : ticketCountsErr
      ? { error: ticketCountsErr }
      : tickets === null
        ? null
        : {};
  const patchState = patchErr
    ? { error: patchErr }
    : patchInfo === null
      ? null
      : {};

  return (
    <section className="view dash-view">
      <div className="view-head">
        <div>
          <h2>Dashboard</h2>
          <p className="muted">
            {devices === null
              ? "loading fleet…"
              : `${(devices || []).length} devices · ${online} online · ${offline} offline`}
          </p>
        </div>
        <div className="view-actions">
          <button
            className="btn"
            onClick={() => {
              loadLive();
              loadStatic();
            }}
            title="Refresh all tiles"
          >
            ↻ refresh
          </button>
        </div>
      </div>

      <div className="dash">
        {/* Row 1: fleet status ------------------------------------------- */}
        <div className="tile tile-4">
          <header className="tile-head">
            <h3>Fleet status</h3>
            <a
              className="tile-link"
              href="#/devices"
              title="Open the device table"
            >
              devices →
            </a>
          </header>
          {devState === null ? (
            <p className="muted tile-loading">loading…</p>
          ) : devState.error ? (
            <p className="muted tile-err" title={devState.error}>
              unavailable
            </p>
          ) : (
            <div className="tile-body fleet-status">
              <Donut online={online} offline={offline} />
              <ul className="fleet-legend">
                <li>
                  <span className="dot on" /> online <strong>{online}</strong>
                </li>
                <li>
                  <span className="dot off" /> offline{" "}
                  <strong>{offline}</strong>
                </li>
              </ul>
            </div>
          )}
        </div>

        <div className="tile tile-4">
          <header className="tile-head">
            <h3>Devices per OS</h3>
          </header>
          {devState === null ? (
            <p className="muted tile-loading">loading…</p>
          ) : devState.error ? (
            <p className="muted tile-err" title={devState.error}>
              unavailable
            </p>
          ) : byOs.length === 0 ? (
            <p className="muted">No devices enrolled yet.</p>
          ) : (
            <div className="tile-body">
              <ul className="os-bars">
                {byOs.map(([os, n]) => (
                  <li key={os}>
                    <span className="os-name mono">{os}</span>
                    <span className="os-bar">
                      <span
                        className="os-bar-fill"
                        style={{
                          width: `${((n / (devices || []).length) * 100).toFixed(1)}%`,
                        }}
                      />
                    </span>
                    <span className="os-count mono">{n}</span>
                  </li>
                ))}
              </ul>
            </div>
          )}
        </div>

        <div className="tile tile-4">
          <header className="tile-head">
            <h3>Alerts</h3>
            <a
              className="tile-link"
              href="#/alerts"
              title="Open the alert inbox"
            >
              inbox →
            </a>
          </header>
          {alertState === null ? (
            <p className="muted tile-loading">loading…</p>
          ) : alertState.error ? (
            <p className="muted tile-err" title={alertState.error}>
              unavailable
            </p>
          ) : (
            <div className="tile-body">
              <p className="alert-counts">
                <span className="pill pill-open">
                  {counts ? counts.open : (alerts || []).length} open
                </span>
                {counts && counts.acked > 0 && (
                  <span className="pill pill-acked">{counts.acked} acked</span>
                )}
              </p>
              {(alerts || []).length === 0 ? (
                <p className="muted">No open alerts.</p>
              ) : (
                <ul className="alert-list">
                  {(alerts || []).map((a) => (
                    <li key={a.id}>
                      <span className="host">{a.hostname}</span>
                      <span className="muted a-name" title={a.name}>
                        {a.name}
                      </span>
                      <span className="muted a-time">{relTime(a.last_at)}</span>
                    </li>
                  ))}
                </ul>
              )}
            </div>
          )}
        </div>

        {/* Row 2: signal --------------------------------------------------- */}
        <div className="tile tile-6">
          <header className="tile-head">
            <h3>Top anomalies</h3>
            <a
              className="tile-link"
              href="#/baseline"
              title="Open the baseline explorer"
            >
              baseline →
            </a>
          </header>
          {anomState === null ? (
            <p className="muted tile-loading">loading…</p>
          ) : anomState.error ? (
            <p className="muted tile-err" title={anomState.error}>
              unavailable
            </p>
          ) : (anomalies || []).length === 0 ? (
            <p className="muted">
              No anomalies on record — the fleet is scoring clean.
            </p>
          ) : (
            <div className="tile-body">
              <ul className="anom-list">
                {(anomalies || []).map((a) => (
                  <li key={a.id}>
                    <span className="host">{hostOf(a.device_id)}</span>
                    <span className="muted a-name mono" title={a.name}>
                      {a.name}
                      {a.source ? ` (${a.source})` : ""}
                    </span>
                    <span className="z-pill" title={`${a.channel} z-score`}>
                      z {Number(a.score).toFixed(1)}
                    </span>
                    <span className="muted a-time">{relTime(a.at)}</span>
                  </li>
                ))}
              </ul>
            </div>
          )}
        </div>

        <div className="tile tile-6">
          <header className="tile-head">
            <h3>Recent activity</h3>
            <a
              className="tile-link"
              href="#/events"
              title="Open the event journal"
            >
              journal →
            </a>
          </header>
          {journalState === null ? (
            <p className="muted tile-loading">loading…</p>
          ) : journalState.error ? (
            <p className="muted tile-err" title={journalState.error}>
              unavailable
            </p>
          ) : (journal || []).length === 0 ? (
            <p className="muted">No events journaled yet.</p>
          ) : (
            <div className="tile-body">
              <ul className="activity-list">
                {(journal || []).map((env) => (
                  <li key={env.id} className={"cat-" + env.category}>
                    <span className="act-dot" aria-hidden="true" />
                    <span className="act-type" title={env.type}>
                      {shortType(env.type)}
                    </span>
                    {env.device_id && (
                      <span className="host">{hostOf(env.device_id)}</span>
                    )}
                    <span className="muted a-time">{relTime(env.at)}</span>
                  </li>
                ))}
              </ul>
            </div>
          )}
        </div>

        {/* Row 3: wave 3 live (inventory #4, tickets #7) -------------------- */}
        <div className="tile tile-6">
          <header className="tile-head">
            <h3>Patch compliance</h3>
            <a
              className="tile-link"
              href="#/devices"
              title="Open the device table for inventory details"
            >
              devices →
            </a>
          </header>
          {patchState === null ? (
            <p className="muted tile-loading">loading inventory…</p>
          ) : patchState.error ? (
            <p className="muted tile-err" title={patchState.error}>
              unavailable
            </p>
          ) : patchInfo.devices_checked === 0 ? (
            <p className="muted">No devices to check.</p>
          ) : (
            <div className="tile-body">
              <p className="alert-counts">
                <span className="pill pill-ok">
                  {patchInfo.devices_recent}/{patchInfo.devices_checked} recent
                </span>
                <span className="pill pill-mut">
                  {patchInfo.devices_with_inventory} have inventory
                </span>
              </p>
              <ul className="activity-list">
                {patchInfo.details.slice(0, 5).map((d) => (
                  <li key={d.id}>
                    <span className="host">{d.hostname}</span>
                    {d.collected_at ? (
                      <span className="muted a-time">
                        {relTime(d.collected_at)}
                      </span>
                    ) : (
                      <span className="muted">no inventory</span>
                    )}
                  </li>
                ))}
              </ul>
            </div>
          )}
        </div>

        <div className="tile tile-6">
          <header className="tile-head">
            <h3>Tickets</h3>
            <a
              className="tile-link"
              href="#/tickets"
              title="Open the ticket queue"
            >
              queue →
            </a>
          </header>
          {ticketState === null ? (
            <p className="muted tile-loading">loading tickets…</p>
          ) : ticketState.error ? (
            <p className="muted tile-err" title={ticketState.error}>
              unavailable
            </p>
          ) : (
            <div className="tile-body">
              <p className="alert-counts">
                <span className="pill pill-open">
                  {ticketCounts ? ticketCounts.open : "—"} open
                </span>
                <span className="pill pill-run">
                  {ticketCounts ? ticketCounts.in_progress : "—"} in progress
                </span>
              </p>
              {(tickets || []).length === 0 ? (
                <p className="muted">No open tickets.</p>
              ) : (
                <ul className="alert-list">
                  {(tickets || []).map((t) => (
                    <li key={t.id}>
                      <span className="host">{t.id}</span>
                      <span className="muted a-name" title={t.title}>
                        {t.title || "(untitled)"}
                      </span>
                      <span className="muted a-time">{relTime(t.created_at)}</span>
                    </li>
                  ))}
                </ul>
              )}
            </div>
          )}
        </div>
      </div>
    </section>
  );
}
