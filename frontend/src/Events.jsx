import { useCallback, useEffect, useMemo, useState } from "react";
import { api } from "./api.js";

// D-2: the global event journal browser. The live SSE stream (wired in
// App.jsx) already pushes new envelopes; this page adds the layer the stream
// alone doesn't provide — historical paging, server-side filtering, and the
// full envelope on demand.
//
// Paging model: on first visit (and on every filter change) the client pages
// FORWARD — after=0, then after=<last id of the batch>, … — until a response
// comes back shorter than PAGE_SIZE, i.e. the end of the journal, and displays
// that final batch (the most recent ≤PAGE_SIZE entries). "Load earlier"
// fetches the preceding batch; "Jump to latest" re-runs the forward walk.
// Rows display newest-first (the server returns a batch oldest-first, so the
// view renders the reversed batch) — a live SSE envelope then appends to the
// end of the batch and appears at the TOP without any re-fetch.
const PAGE_SIZE = 200;
// The server's journal taxonomy (webhook.AllCategories): an unknown category
// is a 400. Command results are journaled as "automation" (the bus subject
// rmmway.events.command.result maps there), not "command".
const CATEGORIES = ["alert", "inventory", "automation", "other"];

function fmtAt(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? String(iso) : d.toLocaleString();
}

// The journal stores device_id only; the hostname comes from the device list
// (best-effort), then from the bus event payload, then the raw id.
function hostnameOf(env, hostnames) {
  if (env.device_id) {
    return hostnames[env.device_id] ||
      (env.event && env.event.data && env.event.data.hostname) ||
      env.device_id;
  }
  return (env.event && env.event.data && env.event.data.hostname) || "—";
}

// One line per journal row; the full envelope JSON lives in the detail pane.
function summarize(env, hostnames) {
  const ev = env.event || {};
  const d = ev.data || {};
  const action = d.action || ev.status || "";
  if (env.category === "alert") {
    const what = d.name || ev.source || "metric";
    if (action) {
      const score = d.score != null ? ` (score ${d.score})` : "";
      return `${what} ${action}${score}`;
    }
    return `${what}${action ? ` — ${action}` : ""}`;
  }
  if (env.category === "inventory") {
    const host = (d.hostname && d.hostname) || hostnameOf(env, hostnames);
    return action ? `${host} ${action}` : `${host} — ${env.type || "device event"}`;
  }
  if (env.category === "automation") {
    if (ev.command_id) return `${ev.command_id}${ev.status ? " " + ev.status : ""}`;
    if (ev.message) return ev.message;
    if (ev.node_id) return `flow ${ev.node_id}${ev.run_id ? ` (run ${ev.run_id})` : ""}`;
    return action || env.type || "automation event";
  }
  return ev.message || action || env.type || "event";
}

function EventRow({ env, hostnames, open, onToggle, onGoToDevice }) {
  const host = hostnameOf(env, hostnames);
  return (
    <>
      <tr
        className={"journal-row" + (open ? " open" : "")}
        data-seq={env.id}
        onClick={onToggle}
      >
        <td className="journal-time">{fmtAt(env.at)}</td>
        <td><span className={"pill cat cat-" + (env.category || "other")}>{env.category || "other"}</span></td>
        <td>{host}</td>
        <td className="journal-type">{env.type}</td>
        <td className="journal-summary">{summarize(env, hostnames)}</td>
      </tr>
      {open && (
        <tr className="journal-detail-row">
          <td colSpan={5}>
            <div className="journal-detail">
              {env.device_id && onGoToDevice && (
                <button
                  className="btn ghost"
                  onClick={(e) => {
                    e.stopPropagation();
                    onGoToDevice(env.device_id, host);
                  }}
                >
                  Go to device →
                </button>
              )}
              <pre className="journal-json">{JSON.stringify(env, null, 2)}</pre>
            </div>
          </td>
        </tr>
      )}
    </>
  );
}

export default function Events({ token, onUnauthorized, onGoToDevice, lastEvent }) {
  const [devices, setDevices] = useState([]);
  const [category, setCategory] = useState("");
  const [device, setDevice] = useState("");
  const [type, setType] = useState("");
  // The filters actually sent to the server (applied on demand).
  const [applied, setApplied] = useState({ category: "", device: "", type: "" });
  const [page, setPage] = useState(null); // null = loading; server order (oldest first)
  const [atLatest, setAtLatest] = useState(true);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState(null);
  const [expanded, setExpanded] = useState(null);

  const hostnames = useMemo(
    () => Object.fromEntries((devices || []).map((d) => [d.id, d.hostname])),
    [devices]
  );

  useEffect(() => {
    let alive = true;
    api
      .devices(token)
      .then((list) => alive && setDevices(list || []))
      .catch(() => {}); // the hostname map is best-effort; ids still render
    return () => {
      alive = false;
    };
  }, [token]);

  const fetchPage = useCallback(
    (after, f) =>
      api.eventJournal(token, {
        after,
        limit: PAGE_SIZE,
        category: f.category,
        device: f.device,
        type: f.type,
      }),
    [token]
  );

  // Walk forward to the end of the journal, then show the final batch.
  // The server answers seq > after, so continuing with after=<last id>
  // pages on without skipping or repeating a row.
  const loadLatest = useCallback(
    async (f) => {
      setLoading(true);
      setError(null);
      try {
        let after = 0;
        let batch = [];
        for (let i = 0; i < 50; i++) {
          batch = await fetchPage(after, f);
          if (batch.length < PAGE_SIZE) break;
          after = batch[batch.length - 1].id;
        }
        setPage(batch);
        setAtLatest(true);
        setExpanded(null);
      } catch (e) {
        if (e.unauthorized) onUnauthorized();
        else setError(e.message);
      } finally {
        setLoading(false);
      }
    },
    [fetchPage, onUnauthorized]
  );

  useEffect(() => {
    loadLatest({ category: "", device: "", type: "" });
  }, [loadLatest]);

  const applyFilters = () => {
    const f = { category, device, type };
    setApplied(f);
    loadLatest(f);
  };

  // The preceding batch: the server answers seq > after, so after =
  // first_id − PAGE_SIZE − 1 yields exactly the PAGE_SIZE entries before the
  // current window (the spec's `first_id − limit` assumed an inclusive
  // `>=`; the journal query is strict `>`, so subtract one more to avoid
  // skipping a row at the boundary). Clamped at 0 for the journal's start.
  const loadEarlier = useCallback(async () => {
    if (!page || page.length === 0 || loading) return;
    const after = Math.max(0, page[0].id - PAGE_SIZE - 1);
    setLoading(true);
    setError(null);
    try {
      const batch = await fetchPage(after, applied);
      setPage(batch);
      setAtLatest(false);
      setExpanded(null);
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setError(e.message);
    } finally {
      setLoading(false);
    }
  }, [page, loading, applied, fetchPage, onUnauthorized]);

  // Live reactivity: a fresh envelope off the SSE stream lands at the top of
  // the view without a re-fetch — but only while viewing the latest window
  // and when it matches the active filters (the server applies the same
  // category/device/type rules, so this mirrors them client-side).
  useEffect(() => {
    if (!lastEvent || !page || !atLatest) return;
    if (page.some((e) => e.id === lastEvent.id)) return;
    if (lastEvent.id <= page[page.length - 1].id) return;
    if (applied.category && lastEvent.category !== applied.category) return;
    if (applied.device && lastEvent.device_id !== applied.device) return;
    if (applied.type && lastEvent.type !== applied.type) return;
    setPage([...page, lastEvent]);
  }, [lastEvent, page, atLatest, applied]);

  const rows = page ? [...page].reverse() : null;
  const hasEarlier = !!page && page.length > 0 && page[0].id > 1;

  return (
    <div className="view">
      <div className="view-head">
        <div>
          <h2>Events</h2>
          <p className="muted">
            The global event journal — every alert, inventory change, and
            automation event, newest first. Filter, page back through history,
            and open a row for the full envelope.
          </p>
        </div>
        <div className="view-actions">
          <label className="events-filter">
            <span>Category</span>
            <select
              value={category}
              onChange={(e) => setCategory(e.target.value)}
            >
              <option value="">all</option>
              {CATEGORIES.map((c) => (
                <option key={c} value={c}>
                  {c}
                </option>
              ))}
            </select>
          </label>
          <label className="events-filter">
            <span>Device</span>
            <select
              value={device}
              onChange={(e) => setDevice(e.target.value)}
            >
              <option value="">all</option>
              {(devices || []).map((d) => (
                <option key={d.id} value={d.id}>
                  {d.hostname || d.id}
                </option>
              ))}
            </select>
          </label>
          <label className="events-filter">
            <span>Type</span>
            <input
              className="search events-type"
              value={type}
              onChange={(e) => setType(e.target.value)}
              placeholder="e.g. rmmway.events.alert"
            />
          </label>
          <button className="btn" onClick={applyFilters} disabled={loading}>
            Apply
          </button>
        </div>
      </div>

      <div className="view-head events-paging">
        <div className="muted">
          {rows
            ? `${rows.length} entries · seq ${page[0].id}–${page[page.length - 1].id}${atLatest ? " (latest)" : ""}`
            : loading
            ? "loading journal…"
            : ""}
        </div>
        <div className="row-actions">
          <button
            className="btn ghost"
            onClick={loadEarlier}
            disabled={!hasEarlier || loading}
            title="Fetch the batch of entries before this window"
          >
            ← Load earlier
          </button>
          {!atLatest && (
            <button
              className="btn"
              onClick={() => loadLatest(applied)}
              disabled={loading}
            >
              Jump to latest
            </button>
          )}
        </div>
      </div>

      {error && <div className="banner err">{error}</div>}
      {!rows && !error && <div className="empty">Loading the event journal…</div>}
      {rows && rows.length === 0 && (
        <div className="empty">No journal entries match the current filter.</div>
      )}
      {rows && rows.length > 0 && (
        <div className="table-wrap">
          <table className="journal">
            <thead>
              <tr>
                <th>Time</th>
                <th>Category</th>
                <th>Device</th>
                <th>Type</th>
                <th>Summary</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((env) => (
                <EventRow
                  key={env.id}
                  env={env}
                  hostnames={hostnames}
                  open={expanded === env.id}
                  onToggle={() =>
                    setExpanded(expanded === env.id ? null : env.id)
                  }
                  onGoToDevice={onGoToDevice}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
