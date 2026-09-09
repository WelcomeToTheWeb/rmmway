import { Fragment, useEffect, useMemo, useState, useCallback } from "react";
import { api } from "./api.js";
import DeviceDetail from "./views/devices/DeviceDetail.jsx";

// ---- shareable table state (gap #10a, wave 2, lane C) -----------------------
// The device table's view state lives in the URL hash, AFTER the route:
//
//   #/devices?sort=host:asc&hidden=tags,agent&client=clt-4f2a
//
//   sort    — "<key>:<asc|desc>"; absent = the server's natural order
//   hidden  — comma-separated hidden column keys (host is never hideable)
//   client  — client filter id; absent = all clients. The server scopes the
//             list (?client=), so the rows AND the counts come from the
//             backend, not a client-side slice.
//
// The hash still starts with "#/devices", so App's parseRoute keeps matching
// the same route; the state is written back on every change (normalizing the
// URL) and re-read on hashchange, so browser back/forward walks the table
// states and a copied URL restores the exact view.

// Column model: single source of truth for headers, cell renderers and the
// hideable set. `key` doubles as the URL state key (sort + hidden).
const COLUMNS = [
  { key: "host", label: "Host", sortable: true, hideable: false },
  { key: "client", label: "Client", sortable: true, hideable: true },
  { key: "os", label: "OS/Arch", sortable: true, hideable: true },
  { key: "agent", label: "Agent", sortable: true, hideable: true },
  { key: "ips", label: "IPs", sortable: true, hideable: true },
  { key: "tags", label: "Tags", sortable: false, hideable: true },
  { key: "status", label: "Status", sortable: true, hideable: true },
];

function parseTableState(hash) {
  const qIndex = hash.indexOf("?");
  const params =
    qIndex === -1
      ? new URLSearchParams()
      : new URLSearchParams(hash.slice(qIndex + 1));
  const state = { sort: null, hidden: new Set(), client: "" };
  const [key, dir] = (params.get("sort") || "").split(":");
  if (
    COLUMNS.some((c) => c.key === key && c.sortable) &&
    (dir === "asc" || dir === "desc")
  ) {
    state.sort = { key, dir };
  }
  (params.get("hidden") || "").split(",").forEach((k) => {
    if (k && COLUMNS.some((c) => c.key === k && c.hideable))
      state.hidden.add(k);
  });
  state.client = params.get("client") || "";
  return state;
}

function tableStateToHash(state) {
  const params = new URLSearchParams();
  if (state.sort) params.set("sort", `${state.sort.key}:${state.sort.dir}`);
  if (state.hidden.size)
    params.set(
      "hidden",
      COLUMNS.filter((c) => state.hidden.has(c.key))
        .map((c) => c.key)
        .join(","),
    );
  if (state.client) params.set("client", state.client);
  const qs = params.toString();
  return "#/devices" + (qs ? `?${qs}` : "");
}

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

// JS base64 of a UTF-8 string (script payloads are short).
function b64(s) {
  return btoa(unescape(encodeURIComponent(s)));
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
          One command, every device carrying the tag — each push is authorized
          for that device. The dispatch is capped at 500 devices.
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
          <select
            className="search bulk-action"
            value={action}
            disabled={busy}
            onChange={(e) => setAction(e.target.value)}
          >
            <option value="run_script">run script</option>
            <option value="reboot">reboot</option>
          </select>
        </label>
        {action === "run_script" && (
          <>
            <label className="field">
              <span>Language</span>
              <select
                className="search bulk-lang"
                value={lang}
                disabled={busy}
                onChange={(e) => setLang(e.target.value)}
              >
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
          <button
            className="btn primary"
            disabled={busy || !tag.trim()}
            onClick={send}
          >
            {busy ? "dispatching…" : "Dispatch to whole group"}
          </button>
        </div>
        {result && (
          <div className="bulk-result">
            <p>
              <strong>{result.requested}</strong> matched · {pushed.length}{" "}
              pushed
              {offline.length > 0 && <> · {offline.length} offline</>}
              {Object.keys(failed).length > 0 && (
                <> · {Object.keys(failed).length} failed</>
              )}
            </p>
            {pushed.length > 0 && (
              <ul className="bulk-result-list mono">
                {pushed.map((p) => (
                  <li key={p.device_id}>
                    <span className="muted">{p.device_id}</span> →{" "}
                    {p.command_id}
                  </li>
                ))}
              </ul>
            )}
            {offline.length > 0 && (
              <p className="muted mono">
                offline (no live stream): {offline.join(", ")}
              </p>
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
  "https://raw.githubusercontent.com/welcometotheweb/rmmway/main/scripts";
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
// copy-paste command per OS, with the server URL pre-filled from the configured
// public URL (GET /api/public-url, falling back to the current origin). The
// device appears in the list the moment the agent connects — no raw curl to
// /admin/bootstrap, no copying a token by hand.
function AddDeviceModal({ token, onUnauthorized, onClose }) {
  const [mint, setMint] = useState(null); // {bootstrap_token, device_id}
  const [error, setError] = useState(null);
  const [server, setServer] = useState(window.location.origin);
  const [publicUrl, setPublicUrl] = useState(null); // from /api/public-url
  const [copied, setCopied] = useState(null); // which block was last copied

  // Fetch the configured public URL and prefill the server field.
  useEffect(() => {
    let alive = true;
    fetch("/api/public-url")
      .then((r) => r.json())
      .then((d) => {
        if (!alive) return;
        if (d.url && d.url !== "") {
          setPublicUrl(d.url);
          setServer(d.url);
        }
      })
      .catch(() => {
        // /api/public-url not available — stick with window.location.origin.
      });
    return () => {
      alive = false;
    };
  }, []);

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
      <div
        className="modal adddev"
        onClick={(e) => e.stopPropagation()}
        role="dialog"
        aria-label="Add a device"
      >
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
        {publicUrl && server !== window.location.origin && (
          <p className="muted adddev-origin-note">
            Prefilled with your configured public URL ({publicUrl}). Agents will
            dial this host on port 50052.
          </p>
        )}
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
                <span className="muted">Token</span> {mint.bootstrap_token}{" "}
                <span className="muted">(one-time, ~30 min)</span>
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

// One fleet row: identity, then one cell per visible column (driven by
// COLUMNS so the header, the URL state and the hideable set stay in one
// place). The whole row toggles the detail panel; column-internal controls
// stop propagation where they act.
function DeviceRow({ d, open, onToggle, visibleColumns, clientName }) {
  const cells = {
    host: (
      <td>
        <div className="host">
          <span className="chev">{open ? "▾" : "▸"}</span> {d.hostname}
        </div>
        <div className="id">{d.id}</div>
      </td>
    ),
    client: (
      <td className="client-cell" title={d.client_id || "unassigned"}>
        {clientName(d)}
      </td>
    ),
    os: (
      <td className="mono">
        {d.os}/{d.arch}
      </td>
    ),
    agent: <td className="mono">{d.agent_version || "—"}</td>,
    ips: (
      <td className="mono ips">
        {d.interfaces && d.interfaces.length ? d.interfaces.join(", ") : "—"}
      </td>
    ),
    tags: (
      <td>
        {d.tags && d.tags.length ? (
          <span className="tags">
            {d.tags.map((t) => (
              <span key={t} className="tag">
                {t}
              </span>
            ))}
          </span>
        ) : (
          <span className="muted">—</span>
        )}
      </td>
    ),
    status: (
      <td>
        <StatusPill online={d.online} lastSeen={d.last_seen} />
      </td>
    ),
  };
  return (
    <tr
      className={(d.online ? "row-on" : "row-off") + (open ? " row-open" : "")}
      onClick={() => onToggle(d.id)}
      title="Show/hide recent agent log entries"
      style={{ cursor: "pointer" }}
    >
      {visibleColumns.map((c) => (
        <Fragment key={c.key}>{cells[c.key]}</Fragment>
      ))}
    </tr>
  );
}

export default function Devices({
  token,
  onUnauthorized,
  focusFilter,
  focusKey,
  liveTick,
}) {
  const [devices, setDevices] = useState(null);
  const [error, setError] = useState(null);
  const [q, setQ] = useState("");
  const [tick, setTick] = useState(0);
  // W6-1: the expanded device (recent indexed events panel below its row).
  const [open, setOpen] = useState(null);
  // Shareable view state (sort / hidden columns / client filter) — lives in
  // the URL hash so a view is linkable and survives reload (gap #10a).
  const [tableState, setTableState] = useState(() =>
    parseTableState(window.location.hash),
  );
  const { hidden, client } = tableState;
  // MSP clients for the Client column + the toolbar filter (gap #2 surface —
  // B owns the store, this view only reads /api/clients). null = loading.
  const [clients, setClients] = useState(null);

  // hashchange (back/forward, or App resetting the hash via the palette)
  // re-reads the shareable state. The effect below writes the state back to
  // the hash, normalizing it — the two never loop because the write is a
  // no-op when the strings are already equal.
  useEffect(() => {
    const onHash = () => setTableState(parseTableState(window.location.hash));
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);
  useEffect(() => {
    const hash = tableStateToHash(tableState);
    if (window.location.hash !== hash) window.location.hash = hash;
  }, [tableState]);

  // Client list for the column mapping + filter. A failure keeps the table
  // usable: the column shows raw ids and the filter stays absent.
  useEffect(() => {
    let alive = true;
    api
      .clients(token)
      .then((list) => alive && setClients(list || []))
      .catch((e) => {
        if (!alive) return;
        if (e.unauthorized) onUnauthorized();
        // non-fatal for the table — the column falls back to raw client ids
      });
    return () => {
      alive = false;
    };
  }, [token, onUnauthorized]);

  const clientById = useMemo(() => {
    const m = new Map();
    (clients || []).forEach((c) => m.set(c.id, c));
    return m;
  }, [clients]);
  const clientName = useCallback(
    (d) => {
      if (!d.client_id) return "Unassigned";
      const c = clientById.get(d.client_id);
      return c ? c.name : d.client_id;
    },
    [clientById],
  );
  // The "Add a device" modal (mint a one-time token -> copy-paste installer).
  const [addOpen, setAddOpen] = useState(false);
  // B-2: the "Dispatch to a group" modal (tag group -> bulk fan-out).
  const [bulkOpen, setBulkOpen] = useState(false);
  // B-2: a tag editor save replaces the device row in local state.
  const saveDevice = useCallback((d) => {
    setDevices((list) =>
      list ? list.map((x) => (x.id === d.id ? d : x)) : list,
    );
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
      // The client filter is server-side (?client=), so the rows AND the
      // counts below come from the backend for the active scope.
      const list = await api.clientScopedDevices(token, client);
      setDevices(list);
      setError(null);
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setError(e.message);
    }
  }, [token, onUnauthorized, client]);

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
        (d.tags || []).some((t) => t.toLowerCase().includes(needle)),
  );
  // Visible columns = the COLUMNS model minus the URL-hidden ones (the
  // toolbar show/hide menu in the same lane writes that state). `host` is
  // never hideable, so the table always keeps at least the identity column.
  const visibleColumns = COLUMNS.filter(
    (c) => !c.hideable || !hidden.has(c.key),
  );
  const onlineCount = (devices || []).filter((d) => d.online).length;
  const total = (devices || []).length;
  const clientLabel = client
    ? (clientById.get(client) || {}).name || client
    : "";
  void tick;

  return (
    <section className="view">
      <div className="view-head">
        <div>
          <h2>Devices</h2>
          <p className="muted">
            {devices === null
              ? "loading…"
              : client
                ? `${total} in ${clientLabel} · ${onlineCount} online · ${
                    total - onlineCount
                  } offline`
                : `${total} total · ${onlineCount} online · ${
                    total - onlineCount
                  } offline`}
          </p>
        </div>
        <div className="view-actions">
          {clients && clients.length > 0 && (
            <select
              className="search client-filter"
              value={client}
              onChange={(e) =>
                setTableState((prev) => ({ ...prev, client: e.target.value }))
              }
              title="Scope the table to one client (server-side ?client=)"
            >
              <option value="">All clients</option>
              {clients.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
            </select>
          )}
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
            title="Send one command to every device carrying a tag"
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
            <p>
              {needle ? (
                <>
                  No devices match <em>{q}</em>.
                </>
              ) : client ? (
                <>
                  No devices in <em>{clientLabel}</em>.
                </>
              ) : (
                "No devices found."
              )}
            </p>
          )}
        </div>
      ) : (
        <div className="table-wrap">
          <table className="devices">
            <thead>
              <tr>
                {visibleColumns.map((c) => (
                  <th key={c.key}>{c.label}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {filtered.map((d) => (
                <Fragment key={d.id}>
                  <DeviceRow
                    d={d}
                    open={open === d.id}
                    onToggle={(id) => setOpen(open === id ? null : id)}
                    visibleColumns={visibleColumns}
                    clientName={clientName}
                  />
                  {open === d.id && (
                    <tr className="detail-row">
                      <td
                        colSpan={visibleColumns.length}
                        className="detail-cell"
                      >
                        <DeviceDetail
                          token={token}
                          device={d}
                          onUnauthorized={onUnauthorized}
                          liveTick={liveTick}
                          onSaved={saveDevice}
                        />
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
