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
                          <TagEditor
                            token={token}
                            device={d}
                            onUnauthorized={onUnauthorized}
                            onSaved={saveDevice}
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
