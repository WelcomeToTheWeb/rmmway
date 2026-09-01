import { useCallback, useEffect, useMemo, useState } from "react";
import { api } from "./api.js";

// D-4: webhook endpoint management. Endpoints subscribe to the journaled
// event categories (alert / inventory / automation / other — an empty
// subscription means ALL) and receive them as HMAC-SHA256-signed deliveries,
// retried with backoff and dead-lettered when the receiver keeps failing.
// Delivery state is cursor-based: `last_seq` is the highest journal sequence
// delivered with a 2xx, so a row's journal view colors every seq ≤ last_seq
// as delivered and the rest as pending. "Replay" moves the cursor back
// (0 = the whole journal) and the sweeper re-drives the gap.
//
// NOTE (deviation, W3-2): the spec listed the category set as
// alert/inventory/automation/command; the server's journal categories are
// alert/inventory/automation/other (command results are journaled as
// `automation`). The UI follows the server.
const CATEGORIES = ["alert", "inventory", "automation", "other"];
const PAGE = 200;
const CAT_CLASS = {
  alert: "cat cat-alert",
  inventory: "cat cat-inventory",
  automation: "cat cat-automation",
  other: "cat cat-other",
};
const catClass = (c) => CAT_CLASS[c] || "cat cat-other";
const fmtAt = (s) => {
  if (!s) return "";
  const d = new Date(s);
  return Number.isNaN(d.getTime()) ? String(s) : d.toLocaleString();
};

// The add/edit form. The HMAC secret is only settable at creation (the API
// never returns it and PATCH doesn't take it), so on edit it's a disabled
// field with a hint.
function WebhookForm({ initial, onSubmit, onCancel, busy }) {
  const isEdit = !!initial;
  const [f, setF] = useState({
    name: initial?.name || "",
    url: initial?.url || "",
    secret: "",
    categories: initial ? [...(initial.categories || [])] : ["alert"],
    enabled: initial ? initial.enabled !== false : true,
  });
  const [err, setErr] = useState(null);

  const toggleCat = (c) =>
    setF((s) => ({
      ...s,
      categories: s.categories.includes(c)
        ? s.categories.filter((x) => x !== c)
        : [...s.categories, c],
    }));

  const submit = async (e) => {
    e.preventDefault();
    if (busy) return;
    setErr(null);
    try {
      const body = isEdit
        ? {
            name: f.name,
            url: f.url,
            categories: f.categories,
            enabled: f.enabled,
          }
        : {
            name: f.name,
            url: f.url,
            secret: f.secret,
            categories: f.categories,
            enabled: f.enabled,
          };
      await onSubmit(body);
    } catch (ex) {
      setErr(ex.message || String(ex));
    }
  };

  return (
    <form className="webhook-form" onSubmit={submit}>
      {err && <div className="banner err">{err}</div>}
      <div className="webhook-form-grid">
        <label>
          Name
          <input value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} placeholder="slack-ops" required />
        </label>
        <label>
          Request URL
          <input
            value={f.url}
            onChange={(e) => setF({ ...f, url: e.target.value })}
            placeholder="https://hooks.slack.com/T000/B000/000"
            required
          />
        </label>
        <label className={isEdit ? "webhook-secret-disabled" : ""}>
          HMAC secret (signs every delivery)
          <input
            type="password"
            value={f.secret}
            onChange={(e) => setF({ ...f, secret: e.target.value })}
            placeholder={isEdit ? "set at creation — not changeable here" : "whsec_…"}
            disabled={isEdit}
            required={!isEdit}
          />
        </label>
        <label className="webhook-form-wide">
          Subscribed categories (none = all)
          <span className="webhook-cats">
            {CATEGORIES.map((c) => (
              <label key={c} className="check">
                <input
                  type="checkbox"
                  checked={f.categories.includes(c)}
                  onChange={() => toggleCat(c)}
                />
                <span className={catClass(c)}>{c}</span>
              </label>
            ))}
          </span>
        </label>
        <label className="check">
          <input
            type="checkbox"
            checked={f.enabled}
            onChange={(e) => setF({ ...f, enabled: e.target.checked })}
          />
          Enabled
        </label>
      </div>
      <div className="row-actions">
        <button type="submit" className="btn" disabled={busy || !f.name || !f.url || (!isEdit && !f.secret)}>
          {busy ? "Saving…" : isEdit ? "Save changes" : "Add webhook"}
        </button>
        <button type="button" className="btn ghost" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </form>
  );
}

// The per-endpoint delivery journal: everything this endpoint is subscribed
// to, with each seq colored against the endpoint's delivery cursor.
function DeliveriesPanel({ ep, events, hostnames, onBack, onReplay, replayMsg, replayBusy }) {
  const rows = useMemo(() => [...(events || [])].reverse(), [events]);
  const pending = (events || []).filter((e) => e.id > ep.last_seq).length;
  return (
    <section className="webhook-panel">
      <div className="webhook-panel-head">
        <button className="btn ghost" onClick={onBack}>← All webhooks</button>
        <div>
          <strong>{ep.name}</strong>
          <span className="muted mono webhook-url"> {ep.url}</span>
        </div>
        <div className="row-actions">
          <ReplayControls ep={ep} onReplay={onReplay} busy={replayBusy} />
        </div>
      </div>
      {replayMsg && (
        <div className="banner ok">
          Cursor reset confirmed — from_seq={replayMsg.from_seq}, last_seq=
          {replayMsg.last_seq}, status {replayMsg.status}. The sweeper now
          re-delivers from sequence {replayMsg.from_seq} forward.
        </div>
      )}
      <div className="webhook-journal-meta muted">
        {rows.length} journaled event{rows.length === 1 ? "" : "s"} in this
        endpoint's categories · cursor at seq {ep.last_seq} · {pending}{" "}
        pending
      </div>
      <div className="table-wrap">
        <table className="journal">
          <thead>
            <tr>
              <th>Seq</th>
              <th>Category</th>
              <th>Type</th>
              <th>Device</th>
              <th>When</th>
              <th>Delivery</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((ev) => (
              <tr key={ev.id} className="journal-row">
                <td className="mono">{ev.id}</td>
                <td><span className={catClass(ev.category)}>{ev.category}</span></td>
                <td>{ev.type}</td>
                <td>{ev.device_id ? hostnames[ev.device_id] || ev.device_id : "—"}</td>
                <td className="muted mono">{fmtAt(ev.at)}</td>
                <td>
                  {ev.id <= ep.last_seq ? (
                    <span className="pill cat cat-ok">delivered</span>
                  ) : (
                    <span className="pill cat cat-pending">pending</span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {events && rows.length === 0 && (
          <div className="empty">No journaled events in this endpoint's categories.</div>
        )}
      </div>
    </section>
  );
}

// "Replay since last success" is a cursor reset to where it already is (the
// cursor never moves past a 2xx), i.e. re-drive only the undelivered tail —
// cheap and safe. "Replay all" resets to the START of the journal, so
// already-delivered events are resent: that gets an inline confirm.
function ReplayControls({ ep, onReplay, busy }) {
  const [confirming, setConfirming] = useState(false);
  return (
    <div className="webhook-replay">
      <button
        className="btn ghost"
        disabled={busy}
        onClick={() => onReplay(ep.last_seq)}
      >
        Replay since last success
      </button>
      {confirming ? (
        <span className="webhook-replay-confirm">
          Resend the ENTIRE journal from seq 0?
          <button className="btn" disabled={busy} onClick={() => onReplay(0, null)}>
            Yes, replay all
          </button>
          <button className="btn ghost" disabled={busy} onClick={() => setConfirming(false)}>
            Cancel
          </button>
        </span>
      ) : (
        <button className="btn ghost" disabled={busy} onClick={() => setConfirming(true)}>
          Replay all
        </button>
      )}
    </div>
  );
}

export default function Webhooks({ token, onUnauthorized, onGoToDevice }) {
  const [devices, setDevices] = useState([]);
  const [hooks, setHooks] = useState(null);
  const [form, setForm] = useState(null); // null | {} new | endpoint edit
  const [viewing, setViewing] = useState(null); // endpoint whose journal is shown
  const [events, setEvents] = useState(null);
  const [error, setError] = useState(null);
  const [notWired, setNotWired] = useState(false);
  const [busy, setBusy] = useState(false);
  const [replayMsg, setReplayMsg] = useState(null);
  const [replayBusy, setReplayBusy] = useState(false);

  const hostnames = useMemo(
    () => Object.fromEntries((devices || []).map((d) => [d.id, d.hostname])),
    [devices]
  );

  useEffect(() => {
    let alive = true;
    api.devices(token).then((list) => alive && setDevices(list || [])).catch(() => {});
    return () => { alive = false; };
  }, [token]);

  const loadHooks = useCallback(async () => {
    try {
      const list = await api.webhooks(token);
      setHooks(list || []);
      setNotWired(false);
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else if (e.status === 503) setNotWired(true);
      else setError(e.message);
    }
  }, [token, onUnauthorized]);

  useEffect(() => {
    loadHooks();
  }, [loadHooks]);

  // Page forward to the journal tail (the journal can outgrow one page);
  // the per-endpoint view shows that newest batch.
  const openDeliveries = useCallback(
    async (ep) => {
      setViewing(ep);
      setEvents(null);
      setReplayMsg(null);
      setError(null);
      try {
        let after = 0;
        let batch = [];
        for (let i = 0; i < 50; i++) {
          batch = await api.webhookEvents(token, ep.id, { after, limit: PAGE });
          if (!batch || batch.length < PAGE) break;
          after = batch[batch.length - 1].id;
        }
        setEvents(batch || []);
      } catch (e) {
        if (e.unauthorized) onUnauthorized();
        else setError(e.message);
      }
    },
    [token, onUnauthorized]
  );

  const toggle = useCallback(
    async (ep) => {
      setBusy(true);
      setError(null);
      try {
        const updated = await api.webhookUpdate(token, ep.id, { enabled: !ep.enabled });
        setHooks((list) => (list || []).map((h) => (h.id === ep.id ? { ...h, ...updated } : h)));
        setViewing((v) => (v && v.id === ep.id ? { ...v, ...updated } : v));
      } catch (e) {
        if (e.unauthorized) onUnauthorized();
        else setError(e.message);
      } finally {
        setBusy(false);
      }
    },
    [token, onUnauthorized]
  );

  const save = useCallback(
    async (body) => {
      setBusy(true);
      setError(null);
      try {
        if (form && form.id != null) {
          const updated = await api.webhookUpdate(token, form.id, body);
          setHooks((list) => (list || []).map((h) => (h.id === form.id ? { ...h, ...updated } : h)));
          setViewing((v) => (v && v.id === form.id ? { ...v, ...updated } : v));
        } else {
          await api.webhookCreate(token, body);
          await loadHooks();
        }
        setForm(null);
      } catch (e) {
        if (e.unauthorized) onUnauthorized();
        throw e;
      } finally {
        setBusy(false);
      }
    },
    [token, onUnauthorized, form, loadHooks]
  );

  const remove = useCallback(
    async (ep) => {
      setBusy(true);
      setError(null);
      try {
        await api.webhookDelete(token, ep.id);
        await loadHooks();
        setViewing((v) => (v && v.id === ep.id ? null : v));
      } catch (e) {
        if (e.unauthorized) onUnauthorized();
        else setError(e.message);
      } finally {
        setBusy(false);
      }
    },
    [token, onUnauthorized, loadHooks]
  );

  const replay = useCallback(
    async (fromSeq) => {
      if (!viewing) return;
      setReplayBusy(true);
      setError(null);
      try {
        const r = await api.webhookReplay(token, viewing.id, { from_seq: fromSeq });
        await openDeliveries({ ...viewing, last_seq: r.last_seq ?? fromSeq });
        setReplayMsg(r); // openDeliveries clears it — restore the confirmation
      } catch (e) {
        if (e.unauthorized) onUnauthorized();
        else setError(e.message);
      } finally {
        setReplayBusy(false);
      }
    },
    [token, onUnauthorized, viewing, openDeliveries]
  );

  if (notWired) {
    return (
      <div className="view">
        <h2>Webhooks</h2>
        <div className="empty">
          The webhook framework is not wired on this server (in-memory
          mode) — start with Postgres to enable signed webhook delivery.
        </div>
      </div>
    );
  }

  return (
    <div className="view">
      <div className="view-head">
        <div>
          <h2>Webhooks</h2>
          <p className="muted">
            HMAC-signed deliveries of journaled events, with retry,
            dead-lettering, and replay from the journal.
          </p>
        </div>
        {!form && !viewing && (
          <button className="btn" onClick={() => setForm({})}>+ Add webhook</button>
        )}
      </div>

      {error && <div className="banner err">{error}</div>}

      {viewing ? (
        <DeliveriesPanel
          ep={viewing}
          events={events}
          hostnames={hostnames}
          onBack={() => setViewing(null)}
          onReplay={(fromSeq) => replay(fromSeq)}
          replayMsg={replayMsg}
          replayBusy={replayBusy || busy}
        />
      ) : (
        <section className="webhook-panel">
          {form && (
            <WebhookForm
              initial={form.id != null ? form : null}
              onSubmit={save}
              onCancel={() => setForm(null)}
              busy={busy}
            />
          )}
          <div className="table-wrap">
            <table className="webhook-list">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>URL</th>
                  <th>Categories</th>
                  <th>Last delivery</th>
                  <th>Failures</th>
                  <th>Enabled</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                {(hooks || []).map((ep) => (
                  <tr key={ep.id} className={"webhook-row" + (ep.enabled ? "" : " off")}>
                    <td>
                      <strong>{ep.name}</strong>
                      <div className="muted mono">seq {ep.last_seq}</div>
                    </td>
                    <td className="mono webhook-url">{ep.url}</td>
                    <td>
                      {(ep.categories && ep.categories.length
                        ? ep.categories
                        : ["all"]
                      ).map((c) => (
                        <span key={c} className={c === "all" ? "cat cat-other" : catClass(c)}>
                          {c}
                        </span>
                      ))}
                    </td>
                    <td className="muted">{fmtAt(ep.updated_at)}</td>
                    <td>
                      {ep.status === "failing" || ep.attempts > 0 ? (
                        <span className="pill cat cat-pending">
                          {ep.attempts} consecutive
                          {ep.status === "failing" ? " · failing" : ""}
                        </span>
                      ) : (
                        <span className="muted">none</span>
                      )}
                    </td>
                    <td>
                      <label className="switch" title={ep.enabled ? "Disable" : "Enable"}>
                        <input
                          type="checkbox"
                          checked={!!ep.enabled}
                          disabled={busy}
                          onChange={() => toggle(ep)}
                        />
                        <span className="switch-track" />
                      </label>
                    </td>
                    <td className="webhook-actions">
                      <button
                        className="btn ghost tiny"
                        onClick={() => setForm({ ...ep })}
                      >
                        Edit
                      </button>
                      <button
                        className="btn ghost tiny"
                        onClick={() => openDeliveries(ep)}
                      >
                        Deliveries
                      </button>
                      <button
                        className="btn ghost tiny danger"
                        onClick={() => remove(ep)}
                        disabled={busy}
                      >
                        Delete
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            {hooks && hooks.length === 0 && (
              <div className="empty">No webhooks yet — add the first one.</div>
            )}
          </div>
        </section>
      )}
    </div>
  );
}
