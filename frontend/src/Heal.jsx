import { useCallback, useEffect, useMemo, useState } from "react";
import { api } from "./api.js";

// D-3: the self-healing playbook dashboard. The PLAYBOOKS panel shows the
// engine's declarative detect→verify-safe→remediate→confirm rules (with
// enable/disable toggles and a create form); the RUNS panel is the
// remediation audit trail — newest first, filterable by status and device
// server-side, each row opening the full stage trace. "Run Pass Now" drives
// one synchronous detect + advance pass (the same pass the background loop
// runs), reports its summary, and refreshes both panels — a freshly
// detected series appears as a new run here and walks the state machine
// detected → remediating → confirming → resolved across passes.
//
// Status vocabulary (heal.Run): detected, verifying, remediating,
// confirming are in flight; resolved (healed) / escalated (needs a human)
// / failed end it; skipped = a verify-safe refusal (cooldown, offline
// device, or another run already owns the series).
const RUN_STATUSES = [
  "detected",
  "verifying",
  "remediating",
  "confirming",
  "resolved",
  "escalated",
  "failed",
  "skipped",
];
const ACTIVE_STATUSES = new Set(["detected", "verifying", "remediating", "confirming"]);

function fmtAt(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? String(iso) : d.toLocaleString();
}

// Wall time from creation to the terminal transition (confirm/escalate, or
// the run's last update when it failed/was skipped); null while in flight.
function fmtDuration(run) {
  const end =
    run.confirmed_at ||
    run.escalated_at ||
    (run.status === "failed" || run.status === "skipped" ? run.updated_at : null);
  if (!run.created_at || !end) return null;
  const ms = new Date(end) - new Date(run.created_at);
  if (Number.isNaN(ms) || ms < 0) return null;
  const s = Math.round(ms / 1000);
  if (s < 60) return s + "s";
  return Math.floor(s / 60) + "m" + (s % 60) + "s";
}

// The trigger condition, human readable: "cpu.utilization_percent > 90".
function triggerOf(pb) {
  if (!pb.metric || !pb.detect_op) return "—";
  return `${pb.metric} ${pb.detect_op} ${pb.detect_threshold}`;
}

// The remediation action, human readable: the OS-appropriate script's first
// line (the engine picks sh vs powershell by the device's OS at runtime).
function actionOf(pb) {
  const first = (s) => (s || "").split("\n")[0].trim();
  const sh = first(pb.remediate_sh);
  const ps = first(pb.remediate_powershell);
  if (sh && ps) return `script: ${sh} / ${ps}`;
  if (sh) return `script: ${sh}`;
  if (ps) return `powershell: ${ps}`;
  return "no script yet";
}

function StatusPill({ status }) {
  return (
    <span className={"pill hs hs-" + status}>{status}</span>
  );
}

// One panel: the declarative playbooks + the create form.
function PlaybooksPanel({ playbooks, lastRuns, busy, onToggle, onCreate, creating, error, onOpenForm, form }) {
  return (
    <section className="heal-panel">
      <div className="heal-panel-head">
        <h3>Playbooks</h3>
        {!creating && (
          <button className="btn ghost" onClick={onOpenForm}>
            + New playbook
          </button>
        )}
      </div>
      {error && <div className="banner err">{error}</div>}
      {form && (
        <CreatePlaybookForm
          onCancel={() => form.close()}
          onSubmit={async (body) => {
            await onCreate(body);
            form.close();
          }}
        />
      )}
      <div className="table-wrap">
        <table className="heal-playbooks">
          <thead>
            <tr>
              <th>Name</th>
              <th>Trigger</th>
              <th>Scope</th>
              <th>Action</th>
              <th>Last run</th>
              <th>Enabled</th>
            </tr>
          </thead>
          <tbody>
            {(playbooks || []).map((pb) => {
              const last = lastRuns[pb.key];
              return (
                <tr key={pb.key} className={"heal-pb-row" + (pb.enabled ? "" : " off")}>
                  <td>
                    <strong>{pb.name || pb.key}</strong>
                    <div className="muted heal-key">{pb.key}</div>
                    {pb.description && <div className="muted">{pb.description}</div>}
                  </td>
                  <td className="heal-trigger">{triggerOf(pb)}</td>
                  <td>{pb.os_filter || "all OS"}{pb.source ? ` · ${pb.source}` : ""}</td>
                  <td className="heal-action">{actionOf(pb)}</td>
                  <td className="muted">
                    {last
                      ? `${fmtAt(last.created_at)} (${last.status})`
                      : "never"}
                  </td>
                  <td>
                    <label className="switch" title={pb.enabled ? "Disable" : "Enable"}>
                      <input
                        type="checkbox"
                        className="pb-toggle"
                        data-pb={pb.key}
                        checked={!!pb.enabled}
                        disabled={busy}
                        onChange={() => onToggle(pb)}
                      />
                      <span className="switch-track" />
                    </label>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
        {playbooks && playbooks.length === 0 && (
          <div className="empty">No playbooks yet — create the first one.</div>
        )}
      </div>
    </section>
  );
}

// The "new playbook" form: the detect condition, the confirm re-measure
// condition, the OS-scoped remediation scripts, and the cooldown.
function CreatePlaybookForm({ onCancel, onSubmit }) {
  const [f, setF] = useState({
    name: "",
    metric: "",
    detect_op: ">",
    detect_threshold: "",
    confirm_op: "<",
    confirm_threshold: "",
    os_filter: "",
    cooldown_seconds: "3600",
    remediate_sh: "",
    remediate_powershell: "",
  });
  const [err, setErr] = useState(null);
  const [busy, setBusy] = useState(false);
  const set = (k) => (e) => setF((s) => ({ ...s, [k]: e.target.value }));

  const submit = async (e) => {
    e.preventDefault();
    if (busy) return;
    setErr(null);
    setBusy(true);
    try {
      const body = {
        name: f.name,
        metric: f.metric,
        detect_op: f.detect_op,
        detect_threshold: Number(f.detect_threshold),
        confirm_op: f.confirm_op,
        confirm_threshold: f.confirm_threshold === "" ? undefined : Number(f.confirm_threshold),
        os_filter: f.os_filter || undefined,
        cooldown_seconds: Number(f.cooldown_seconds) || undefined,
        remediate_sh: f.remediate_sh || undefined,
        remediate_powershell: f.remediate_powershell || undefined,
      };
      await onSubmit(body);
    } catch (ex) {
      setErr(ex.message || String(ex));
      setBusy(false);
    }
  };

  return (
    <form className="heal-form" onSubmit={submit}>
      {err && <div className="banner err">{err}</div>}
      <div className="heal-form-grid">
        <label>
          Name
          <input value={f.name} onChange={set("name")} placeholder="CPU saturation" required />
        </label>
        <label>
          Metric
          <input value={f.metric} onChange={set("metric")} placeholder="cpu.utilization_percent" required />
        </label>
        <label>
          Detect when
          <span className="heal-form-row">
            <select value={f.detect_op} onChange={set("detect_op")}>
              {[">", ">=", "==", "<", "<="].map((op) => (
                <option key={op} value={op}>{op}</option>
              ))}
            </select>
            <input
              type="number"
              step="any"
              value={f.detect_threshold}
              onChange={set("detect_threshold")}
              placeholder="threshold"
              required
            />
          </span>
        </label>
        <label>
          Confirm healed when
          <span className="heal-form-row">
            <select value={f.confirm_op} onChange={set("confirm_op")}>
              {["<", "<=", "==", ">", ">="].map((op) => (
                <option key={op} value={op}>{op}</option>
              ))}
            </select>
            <input
              type="number"
              step="any"
              value={f.confirm_threshold}
              onChange={set("confirm_threshold")}
              placeholder="threshold"
            />
          </span>
        </label>
        <label>
          OS filter (comma list; empty = all)
          <input value={f.os_filter} onChange={set("os_filter")} placeholder="linux, windows" />
        </label>
        <label>
          Cooldown (seconds)
          <input type="number" value={f.cooldown_seconds} onChange={set("cooldown_seconds")} />
        </label>
        <label className="heal-form-wide">
          Remediation script (sh — Linux/macOS)
          <textarea
            className="heal-script"
            rows={3}
            value={f.remediate_sh}
            onChange={set("remediate_sh")}
            placeholder="systemctl restart your-service"
          />
        </label>
        <label className="heal-form-wide">
          Remediation script (PowerShell — Windows)
          <textarea
            className="heal-script"
            rows={3}
            value={f.remediate_powershell}
            onChange={set("remediate_powershell")}
            placeholder="Restart-Service YourService"
          />
        </label>
      </div>
      <div className="row-actions">
        <button type="submit" className="btn" disabled={busy || !f.name || !f.metric || f.detect_threshold === ""}>
          {busy ? "Saving…" : "Create playbook"}
        </button>
        <button type="button" className="btn ghost" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </form>
  );
}

// The second panel: the runs audit trail + the per-run stage trace.
function RunsPanel({ runs, devices, hostnames, selectedRun, onSelect, onFilter, error }) {
  const [status, setStatus] = useState("");
  const [deviceId, setDeviceId] = useState("");
  return (
    <section className="heal-panel">
      <div className="heal-panel-head">
        <h3>Runs</h3>
        <div className="heal-runs-filters">
          <select
            value={status}
            onChange={(e) => {
              const v = e.target.value;
              setStatus(v);
              onFilter({ status: v, device_id: deviceId });
            }}
          >
            <option value="">all statuses</option>
            {RUN_STATUSES.map((s) => (
              <option key={s} value={s}>{s}</option>
            ))}
          </select>
          <select
            value={deviceId}
            onChange={(e) => {
              const v = e.target.value;
              setDeviceId(v);
              onFilter({ status, device_id: v });
            }}
          >
            <option value="">all devices</option>
            {(devices || []).map((d) => (
              <option key={d.id} value={d.id}>{d.hostname || d.id}</option>
            ))}
          </select>
        </div>
      </div>
      {error && <div className="banner err">{error}</div>}
      {selectedRun ? (
        <RunDetail run={selectedRun.run} events={selectedRun.events} hostnames={hostnames} onBack={() => onSelect(null)} />
      ) : (
        <div className="table-wrap">
          <table className="heal-runs">
            <thead>
              <tr>
                <th>When</th>
                <th>Playbook</th>
                <th>Device</th>
                <th>Status</th>
                <th>Duration</th>
              </tr>
            </thead>
            <tbody>
              {(runs || []).map((r) => (
                <tr
                  key={r.id}
                  className="heal-run-row"
                  data-run-id={r.id}
                  onClick={() => onSelect(r.id)}
                >
                  <td className="muted mono">{fmtAt(r.created_at)}</td>
                  <td>{r.playbook_key}</td>
                  <td>{hostnames[r.device_id] || r.device_id}</td>
                  <td><StatusPill status={r.status} /></td>
                  <td className="muted">
                    {ACTIVE_STATUSES.has(r.status) ? "in flight…" : (fmtDuration(r) || "—")}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {runs && runs.length === 0 && (
            <div className="empty">No runs for this filter.</div>
          )}
        </div>
      )}
    </section>
  );
}

// The full trace of one run: the stage-by-stage audit trail (which trigger
// fired, what was dispatched, what the agent reported back, the outcome)
// plus the stage timeline.
function RunDetail({ run, events, hostnames, onBack }) {
  return (
    <div className="heal-run-detail">
      <div className="heal-run-detail-head">
        <button className="btn ghost" onClick={onBack}>← All runs</button>
        <div>
          <strong>{run.playbook_key}</strong>
          <StatusPill status={run.status} />
        </div>
        <div className="muted">
          {hostnames[run.device_id] || run.device_id}
          {run.source ? ` · source ${run.source}` : ""}
        </div>
      </div>
      <ol className="heal-trace">
        {(events || []).map((ev) => (
          <li key={ev.id} className="heal-trace-step">
            <div className="heal-trace-head">
              <StatusPill status={ev.status} />
              <span className="muted mono">{fmtAt(ev.at)}</span>
            </div>
            {ev.reason && <pre className="heal-trace-reason">{ev.reason}</pre>}
          </li>
        ))}
      </ol>
    </div>
  );
}

export default function Heal({ token, onUnauthorized, onGoToDevice }) {
  const [devices, setDevices] = useState([]);
  const [playbooks, setPlaybooks] = useState(null);
  const [runs, setRuns] = useState(null);
  const [statusFilter, setStatusFilter] = useState("");
  const [deviceFilter, setDeviceFilter] = useState("");
  const [selectedId, setSelectedId] = useState(null);
  const [detail, setDetail] = useState(null); // { run, events }
  const [error, setError] = useState(null);
  const [notWired, setNotWired] = useState(false);
  const [busy, setBusy] = useState(false);
  const [creating, setCreating] = useState(false);
  const [passBusy, setPassBusy] = useState(false);
  const [passSummary, setPassSummary] = useState(null);

  const hostnames = useMemo(
    () => Object.fromEntries((devices || []).map((d) => [d.id, d.hostname])),
    [devices]
  );

  // Best-effort hostname map for the run rows.
  useEffect(() => {
    let alive = true;
    api.devices(token).then((list) => alive && setDevices(list || [])).catch(() => {});
    return () => { alive = false; };
  }, [token]);

  const loadPlaybooks = useCallback(async () => {
    try {
      const list = await api.healPlaybooks(token);
      setPlaybooks(list || []);
      setNotWired(false);
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else if (e.status === 503) setNotWired(true);
      else setError(e.message);
    }
  }, [token, onUnauthorized]);

  const loadRuns = useCallback(
    async (status, deviceId) => {
      try {
        const list = await api.healRuns(token, { status, device_id: deviceId });
        setRuns(list || []);
        setNotWired(false);
      } catch (e) {
        if (e.unauthorized) onUnauthorized();
        else if (e.status === 503) setNotWired(true);
        else setError(e.message);
      }
    },
    [token, onUnauthorized]
  );

  useEffect(() => {
    loadPlaybooks();
    loadRuns("", "");
  }, [loadPlaybooks, loadRuns]);

  const lastRuns = useMemo(() => {
    const m = {};
    for (const r of runs || []) {
      if (!m[r.playbook_key] || new Date(r.created_at) > new Date(m[r.playbook_key].created_at)) {
        m[r.playbook_key] = r;
      }
    }
    return m;
  }, [runs]);

  const selectRun = useCallback(
    async (id) => {
      setSelectedId(id);
      if (id == null) {
        setDetail(null);
        return;
      }
      setError(null);
      try {
        setDetail(await api.healRun(token, id));
      } catch (e) {
        if (e.unauthorized) onUnauthorized();
        else setError(e.message);
      }
    },
    [token, onUnauthorized]
  );

  const togglePlaybook = useCallback(
    async (pb) => {
      setBusy(true);
      setError(null);
      try {
        const updated = await api.healUpdatePlaybook(token, pb.key, { enabled: !pb.enabled });
        setPlaybooks((list) =>
          (list || []).map((p) => (p.key === pb.key ? { ...p, ...updated } : p))
        );
      } catch (e) {
        if (e.unauthorized) onUnauthorized();
        else setError(e.message);
      } finally {
        setBusy(false);
      }
    },
    [token, onUnauthorized]
  );

  const createPlaybook = useCallback(
    async (body) => {
      setBusy(true);
      setError(null);
      try {
        await api.healCreatePlaybook(token, body);
        await loadPlaybooks();
        await loadRuns(statusFilter, deviceFilter);
      } catch (e) {
        // rethrow so the form can show the error inline
        if (e.unauthorized) onUnauthorized();
        throw e;
      } finally {
        setBusy(false);
      }
    },
    [token, onUnauthorized, loadPlaybooks, loadRuns, statusFilter, deviceFilter]
  );

  const runPass = useCallback(async () => {
    setPassBusy(true);
    setError(null);
    try {
      const p = await api.healPass(token);
      setPassSummary(p);
      await Promise.all([loadPlaybooks(), loadRuns(statusFilter, deviceFilter)]);
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setError(e.message);
    } finally {
      setPassBusy(false);
    }
  }, [token, onUnauthorized, loadPlaybooks, loadRuns, statusFilter, deviceFilter]);

  if (notWired) {
    return (
      <div className="view">
        <h2>Heal</h2>
        <div className="empty">
          The heal engine is not wired on this server (in-memory mode) — start
          with Postgres to enable playbooks.
        </div>
      </div>
    );
  }

  return (
    <div className="view">
      <div className="view-head">
        <div>
          <h2>Heal</h2>
          <p className="muted">
            Self-healing playbooks: detect a failing metric, verify it's safe
            to act, remediate on the device, confirm the fix — or escalate to
            a human.
          </p>
        </div>
        <div className="row-actions">
          {passSummary && (
            <span className="heal-pass-summary muted">
              last pass: {passSummary.detections} detected · {passSummary.started} started ·{" "}
              {passSummary.confirmed} confirmed · {passSummary.escalated} escalated ·{" "}
              {passSummary.failed} failed · {passSummary.active_runs} in flight
            </span>
          )}
          <button className="btn" onClick={runPass} disabled={passBusy}>
            {passBusy ? "Running pass…" : "Run Pass Now"}
          </button>
        </div>
      </div>

      {error && <div className="banner err">{error}</div>}

      <div className="heal-panels">
        <PlaybooksPanel
          playbooks={playbooks}
          lastRuns={lastRuns}
          busy={busy}
          onToggle={togglePlaybook}
          onCreate={createPlaybook}
          creating={creating}
          error={null}
          onOpenForm={() => setCreating(true)}
          form={creating ? { close: () => setCreating(false) } : null}
        />
        <RunsPanel
          runs={runs}
          devices={devices}
          hostnames={hostnames}
          selectedRun={detail}
          onSelect={(id) => (id ? selectRun(id) : selectRun(null))}
          onFilter={(f) => {
            setStatusFilter(f.status || "");
            setDeviceFilter(f.device_id || "");
            loadRuns(f.status || "", f.device_id || "");
          }}
          error={null}
        />
      </div>
    </div>
  );
}
