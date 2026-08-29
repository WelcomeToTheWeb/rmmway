import { useEffect, useState, useCallback } from "react";
import { api } from "./api.js";

const OPS = [">", ">=", "==", "<", "<="];
const STEP_TYPES = [
  { key: "script", label: "run script" },
  { key: "check", label: "check metric (branch)" },
  { key: "notify", label: "notify" },
];

// ---- graph <-> composer model ----------------------------------------------
//
// Composer model: a trigger plus an ordered list of steps. Each step is
// script | check | notify. Linear steps continue to the next step; a check
// step branches: "if condition holds -> then target" / "otherwise -> else
// target", where a target is "next" (the following step), "end", or a jump
// to a LATER step (that's what makes it a DAG, not a line).

function stepId(i) {
  return "s" + (i + 1);
}

function buildGraph(comp) {
  const trig = comp.trigger;
  const nodes = [
    {
      id: "t",
      kind: "trigger",
      name: trig.name?.trim() || `when ${trig.metric} ${trig.op} ${trig.threshold}`,
      metric: trig.metric,
      source: trig.source || "",
      op: trig.op,
      threshold: Number(trig.threshold),
      next: comp.steps.length ? stepId(0) : "",
    },
  ];
  comp.steps.forEach((st, i) => {
    const id = stepId(i);
    const nextId = i + 1 < comp.steps.length ? stepId(i + 1) : "";
    const target = (choice) =>
      choice === "next" ? nextId : choice === "end" ? "" : stepId(Number(choice));
    if (st.type === "script") {
      nodes.push({
        id,
        kind: "script",
        name: st.name?.trim() || "run script",
        lang: st.lang,
        script: st.script,
        timeout_s: Number(st.timeout) || 0,
        next: nextId,
      });
    } else if (st.type === "check") {
      nodes.push({
        id,
        kind: "check",
        name: st.name?.trim() || `check ${st.metric} ${st.op} ${st.threshold}`,
        metric: st.metric,
        source: st.source || "",
        op: st.op,
        threshold: Number(st.threshold),
        timeout_s: Number(st.timeout) || 0,
        then: target(st.then || "next"),
        else: target(st.else || "end"),
      });
    } else {
      nodes.push({
        id,
        kind: "notify",
        name: st.name?.trim() || "notify",
        message: st.message,
        next: nextId,
      });
    }
  });
  return { nodes };
}

function toComposer(flow) {
  const byId = {};
  (flow.graph.nodes || []).forEach((n) => (byId[n.id] = n));
  const trig = (flow.graph.nodes || []).find((n) => n.kind === "trigger");
  // Walk the backbone (trigger -> next/then) to recover the step order.
  const steps = [];
  let cur = trig && trig.next ? byId[trig.next] : null;
  let guard = 0;
  while (cur && guard++ < 64) {
    if (cur.kind === "script") {
      steps.push({
        type: "script",
        name: cur.name || "",
        lang: cur.lang || "sh",
        script: cur.script || "",
        timeout: cur.timeout_s ?? 300,
      });
    } else if (cur.kind === "check") {
      steps.push({
        type: "check",
        name: cur.name || "",
        metric: cur.metric || "",
        source: cur.source || "",
        op: cur.op || ">",
        threshold: cur.threshold ?? 90,
        timeout: cur.timeout_s ?? 300,
        then: cur.then ? "next" : "end",
        else: cur.else || "end",
      });
    } else if (cur.kind === "notify") {
      steps.push({ type: "notify", name: cur.name || "", message: cur.message || "" });
    }
    const nxt = cur.kind === "check" ? cur.then : cur.next;
    cur = nxt ? byId[nxt] : null;
  }
  return {
    name: flow.name || "",
    description: flow.description || "",
    enabled: flow.enabled !== false,
    cooldown_seconds: flow.cooldown_seconds || 0,
    trigger: {
      name: trig?.name || "",
      metric: trig?.metric || "",
      source: trig?.source || "",
      op: trig?.op || ">",
      threshold: trig?.threshold ?? 90,
    },
    steps,
  };
}

// ---- visual pipeline (the rendered chain) ----------------------------------

function condLabel(n) {
  return `${n.metric}${n.source ? `(${n.source})` : ""} ${n.op} ${n.threshold}`;
}

function nodeLabel(n) {
  switch (n.kind) {
    case "trigger":
      return "when " + condLabel(n);
    case "script":
      return "run " + (n.lang || "sh") + " script";
    case "check":
      return "if " + condLabel(n);
    case "notify":
      return "notify";
    default:
      return n.id;
  }
}

// renderChain follows the backbone (next/then) from the trigger; check nodes
// carry their else-branch as a label. Off-backbone targets (a jump target)
// are rendered where they sit on the backbone with a "via else" hint.
function Pipeline({ graph }) {
  const byId = {};
  (graph.nodes || []).forEach((n) => (byId[n.id] = n));
  const trig = (graph.nodes || []).find((n) => n.kind === "trigger");
  const chain = [];
  let cur = trig;
  let guard = 0;
  while (cur && guard++ < 64) {
    chain.push(cur);
    const nxt = cur.kind === "check" ? cur.then : cur.next;
    cur = nxt ? byId[nxt] : null;
  }
  // which nodes are jump (else) targets?
  const jumpTargets = new Set(
    (graph.nodes || []).filter((n) => n.kind === "check" && n.else).map((n) => n.else)
  );
  return (
    <div className="pipeline">
      {chain.map((n, i) => (
        <span key={n.id + i} className="pipe-step">
          <span
            className={"pipe-card kind-" + n.kind}
            title={
              n.kind === "script"
                ? n.script
                : n.kind === "check"
                ? `if ${condLabel(n)} → then: ${n.then ? nodeLabel(byId[n.then]) : "end"} · else: ${n.else ? nodeLabel(byId[n.else]) : "end"}`
                : n.message || ""
            }
          >
            <span className="pipe-kind">{n.kind}</span>
            <span className="pipe-label">{nodeLabel(n)}</span>
            {n.kind === "check" && (
              <span className="pipe-branch">
                yes → {n.then ? (byId[n.then] ? nodeLabel(byId[n.then]) : "end") : "end"}
                {" · "}no → {n.else ? (byId[n.else] ? nodeLabel(byId[n.else]) : "end") : "end"}
              </span>
            )}
            {jumpTargets.has(n.id) && <span className="pipe-jump" title="a check can jump here on 'no'">⇣ else</span>}
          </span>
          {i < chain.length - 1 && <span className="pipe-arrow">→</span>}
        </span>
      ))}
      {chain.length === 0 && <span className="muted">empty flow</span>}
    </div>
  );
}

// ---- status + time helpers --------------------------------------------------

function pill(status) {
  const cls =
    status === "running" ? "pill-run" : status === "succeeded" ? "pill-ok" : "pill-bad";
  return <span className={"pill " + cls}>{status}</span>;
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

// ---- one flow card ----------------------------------------------------------

function FlowCard({ token, flow, devices, onUnauthorized, onChanged, onEdit }) {
  const [showTrigger, setShowTrigger] = useState(false);
  const [deviceId, setDeviceId] = useState("");
  const [value, setValue] = useState("");
  const [err, setErr] = useState(null);
  const [ok, setOk] = useState(null);

  const act = async (fn) => {
    setErr(null);
    setOk(null);
    try {
      await fn();
      await onChanged();
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setErr(e.message);
    }
  };

  return (
    <div className={"flow-card" + (flow.enabled ? "" : " flow-off")}>
      <div className="flow-head">
        <div>
          <span className="flow-name">{flow.name}</span>{" "}
          <span className={"pill " + (flow.enabled ? "pill-ok" : "pill-mut")}>
            {flow.enabled ? "enabled" : "disabled"}
          </span>
        </div>
        <span className="muted tiny">
          id {flow.id} · updated {relTime(flow.updated_at)}
        </span>
      </div>
      {flow.description && <p className="muted flow-desc">{flow.description}</p>}
      <Pipeline graph={flow.graph} />
      <div className="flow-actions">
        <button
          className="btn ghost"
          title="Edit this flow (opens the composer)"
          onClick={onEdit}
        >
          edit
        </button>
        <button
          className="btn ghost"
          title={flow.enabled ? "Disable this flow" : "Enable this flow"}
          onClick={() => act(() => api.updateFlow(token, flow.id, { enabled: !flow.enabled }))}
        >
          {flow.enabled ? "disable" : "enable"}
        </button>
        <button
          className="btn ghost"
          title="Fire a synthetic trigger (pick a device; value optional)"
          onClick={() => setShowTrigger((v) => !v)}
        >
          test trigger
        </button>
        <span className="muted" style={{ flex: 1 }} />
        <button
          className="btn ghost"
          title="Delete this flow (runs keep their audit rows)"
          onClick={() => act(() => api.deleteFlow(token, flow.id))}
        >
          delete
        </button>
      </div>
      {showTrigger && (
        <div className="trigger-box">
          <label className="field">
            <span>device</span>
            <select value={deviceId} onChange={(e) => setDeviceId(e.target.value)}>
              <option value="">— pick a device —</option>
              {devices.map((d) => (
                <option key={d.id} value={d.id}>
                  {d.hostname} ({d.id})
                </option>
              ))}
            </select>
          </label>
          <label className="field">
            <span>value (optional — empty = measure the live metric)</span>
            <input value={value} onChange={(e) => setValue(e.target.value)} placeholder="e.g. 95" />
          </label>
          <div className="row-actions">
            <button
              className="btn primary"
              disabled={!deviceId}
              onClick={() =>
                act(async () => {
                  await api.triggerFlow(token, flow.id, {
                    device_id: deviceId,
                    value: value === "" ? null : Number(value),
                  });
                  setOk("trigger published to the bus — the chain now runs over NATS; watch the runs below");
                  setShowTrigger(false);
                })
              }
            >
              fire
            </button>
          </div>
          {ok && <div className="banner ok2">{ok}</div>}
        </div>
      )}
      {err && <div className="banner err">{err}</div>}
    </div>
  );
}

// ---- composer ---------------------------------------------------------------

function Composer({ token, initial, onClose, onSaved, onUnauthorized }) {
  const editing = !!initial;
  const [comp, setComp] = useState(initial || {
    name: "",
    description: "",
    enabled: true,
    cooldown_seconds: 0,
    trigger: { name: "", metric: "", source: "", op: ">", threshold: 90 },
    steps: [],
  });
  const [err, setErr] = useState(null);

  const set = (patch) => setComp((c) => ({ ...c, ...patch }));
  const setTrig = (patch) => setComp((c) => ({ ...c, trigger: { ...c.trigger, ...patch } }));
  const setStep = (i, patch) =>
    setComp((c) => ({ ...c, steps: c.steps.map((s, j) => (j === i ? { ...s, ...patch } : s)) }));

  const addStep = (type) => {
    const base = { name: "", then: "next", else: "end" };
    if (type === "script")
      setComp((c) => ({
        ...c,
        steps: [...c.steps, { ...base, type, lang: "sh", script: "", timeout: 300 }],
      }));
    if (type === "check")
      setComp((c) => ({
        ...c,
        steps: [...c.steps, { ...base, type, metric: c.trigger.metric, source: "", op: c.trigger.op, threshold: c.trigger.threshold, timeout: 300 }],
      }));
    if (type === "notify") setComp((c) => ({ ...c, steps: [...c.steps, { ...base, type, message: "" }]}));
  };
  const rmStep = (i) =>
    // removing a step invalidates jump targets — reset any "else/then" that
    // pointed past the removed index back to "end" (simple + safe).
    setComp((c) => {
      const steps = c.steps.filter((_, j) => j !== i);
      steps.forEach((s) => {
        ["then", "else"].forEach((k) => {
          if (s[k] && s[k] !== "next" && s[k] !== "end" && Number(s[k].slice(1)) >= i) s[k] = "end";
        });
      });
      return { ...c, steps };
    });

  // Target choices for a check's then/else at step i: "next" (or "end" when
  // last) plus jumps to LATER steps (DAG forward edges only — no cycles).
  const targetOptions = (i) => {
    const opts = [];
    if (i + 1 < comp.steps.length) opts.push({ v: "next", label: "→ next step" });
    opts.push({ v: "end", label: "→ end chain" });
    for (let j = i + 1; j < comp.steps.length; j++) {
      opts.push({ v: stepId(j), label: `→ step ${j + 1} (${comp.steps[j].type})` });
    }
    return opts;
  };

  const save = async () => {
    setErr(null);
    if (!comp.name.trim()) return setErr("name is required");
    if (!comp.trigger.metric.trim()) return setErr("trigger metric is required");
    const body = {
      name: comp.name.trim(),
      description: comp.description,
      enabled: comp.enabled,
      cooldown_seconds: Number(comp.cooldown_seconds) || 0,
      graph: buildGraph(comp),
    };
    try {
      if (editing) await api.updateFlow(token, initial.id, body);
      else await api.createFlow(token, body);
      await onSaved();
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setErr(e.message);
    }
  };

  return (
    <div className="composer">
      <h3>{editing ? `Edit flow: ${initial.name}` : "New flow"}</h3>
      <p className="muted">
        Trigger → steps. Every hop runs over the NATS bus; a check re-measures
        the metric after the previous step and branches (the DAG).
      </p>
      <div className="comp-grid">
        <label className="field">
          <span>name</span>
          <input value={comp.name} onChange={(e) => set({ name: e.target.value })} placeholder="disk-full" />
        </label>
        <label className="field">
          <span>cooldown (s) between runs for the same device — 0 = none</span>
          <input
            type="number"
            min="0"
            value={comp.cooldown_seconds}
            onChange={(e) => set({ cooldown_seconds: e.target.value })}
          />
        </label>
        <label className="field comp-wide">
          <span>description</span>
          <input
            value={comp.description}
            onChange={(e) => set({ description: e.target.value })}
            placeholder="What this chain does"
          />
        </label>
      </div>

      <div className="comp-section">
        <h4>① trigger — when does the chain fire?</h4>
        <div className="comp-grid">
          <label className="field">
            <span>label (shown in the pipeline)</span>
            <input value={comp.trigger.name} onChange={(e) => setTrig({ name: e.target.value })} placeholder="disk > 90%" />
          </label>
          <label className="field">
            <span>metric</span>
            <input value={comp.trigger.metric} onChange={(e) => setTrig({ metric: e.target.value })} placeholder="disk.used_percent" />
          </label>
          <label className="field">
            <span>source (empty = any)</span>
            <input value={comp.trigger.source} onChange={(e) => setTrig({ source: e.target.value })} placeholder="/dev/sda1" />
          </label>
          <label className="field">
            <span>condition</span>
            <span className="op-row">
              <select value={comp.trigger.op} onChange={(e) => setTrig({ op: e.target.value })}>
                {OPS.map((o) => (
                  <option key={o}>{o}</option>
                ))}
              </select>
              <input
                type="number"
                value={comp.trigger.threshold}
                onChange={(e) => setTrig({ threshold: e.target.value })}
              />
            </span>
          </label>
        </div>
      </div>

      <div className="comp-section">
        <h4>② steps — what happens</h4>
        {comp.steps.length === 0 && (
          <p className="muted">No steps yet — a trigger-only flow just records the detection.</p>
        )}
        {comp.steps.map((st, i) => (
          <div key={i} className="step-card">
            <div className="step-head">
              <span className="step-idx">{i + 1}</span>
              <select value={st.type} onChange={(e) => setStep(i, { type: e.target.value, then: "next", else: "end", lang: "sh", script: "", timeout: 300, metric: comp.trigger.metric, op: comp.trigger.op, threshold: comp.trigger.threshold, source: "", message: "" })}>
                {STEP_TYPES.map((t) => (
                  <option key={t.key} value={t.key}>
                    {t.label}
                  </option>
                ))}
              </select>
              <input
                className="step-name"
                value={st.name}
                placeholder="label (shown in the pipeline)"
                onChange={(e) => setStep(i, { name: e.target.value })}
              />
              <button className="btn ghost" title="Remove step" onClick={() => rmStep(i)}>
                ✕
              </button>
            </div>
            {st.type === "script" && (
              <div className="comp-grid">
                <label className="field">
                  <span>lang</span>
                  <select value={st.lang} onChange={(e) => setStep(i, { lang: e.target.value })}>
                    <option>sh</option>
                    <option>powershell</option>
                    <option>python</option>
                  </select>
                </label>
                <label className="field">
                  <span>timeout (s)</span>
                  <input type="number" value={st.timeout} onChange={(e) => setStep(i, { timeout: e.target.value })} />
                </label>
                <label className="field comp-wide">
                  <span>script ({{source}} is substituted with the triggered series' source)</span>
                  <textarea rows={4} value={st.script} onChange={(e) => setStep(i, { script: e.target.value })} placeholder={"#!/bin/sh\ndf -h {{source}}"} />
                </label>
              </div>
            )}
            {st.type === "check" && (
              <div className="comp-grid">
                <label className="field">
                  <span>metric</span>
                  <input value={st.metric} onChange={(e) => setStep(i, { metric: e.target.value })} />
                </label>
                <label className="field">
                  <span>source (empty = any)</span>
                  <input value={st.source} onChange={(e) => setStep(i, { source: e.target.value })} />
                </label>
                <label className="field">
                  <span>condition (re-measured after the previous step)</span>
                  <span className="op-row">
                    <select value={st.op} onChange={(e) => setStep(i, { op: e.target.value })}>
                      {OPS.map((o) => (
                        <option key={o}>{o}</option>
                      ))}
                    </select>
                    <input type="number" value={st.threshold} onChange={(e) => setStep(i, { threshold: e.target.value })} />
                  </span>
                </label>
                <label className="field">
                  <span>if condition HOLDS →</span>
                  <select value={st.then} onChange={(e) => setStep(i, { then: e.target.value })}>
                    {targetOptions(i).map((o) => (
                      <option key={o.v} value={o.v}>
                        {o.label}
                      </option>
                    ))}
                  </select>
                </label>
                <label className="field">
                  <span>otherwise →</span>
                  <select value={st.else} onChange={(e) => setStep(i, { else: e.target.value })}>
                    {targetOptions(i).map((o) => (
                      <option key={o.v} value={o.v}>
                        {o.label}
                      </option>
                    ))}
                  </select>
                </label>
                <label className="field">
                  <span>timeout (s) waiting for a fresh sample</span>
                  <input type="number" value={st.timeout} onChange={(e) => setStep(i, { timeout: e.target.value })} />
                </label>
              </div>
            )}
            {st.type === "notify" && (
              <label className="field comp-wide">
                <span>message (fired through the notification seam; W6-2 adds webhooks)</span>
                <input value={st.message} onChange={(e) => setStep(i, { message: e.target.value })} placeholder="disk still full after cleanup — needs a human" />
              </label>
            )}
          </div>
        ))}
        <div className="row-actions">
          {STEP_TYPES.map((t) => (
            <button key={t.key} className="btn ghost" onClick={() => addStep(t.key)}>
              + {t.label}
            </button>
          ))}
        </div>
      </div>

      {err && <div className="banner err">{err}</div>}
      <div className="row-actions comp-foot">
        <label className="field check">
          <input type="checkbox" checked={comp.enabled} onChange={(e) => set({ enabled: e.target.checked })} />
          enabled
        </label>
        <span style={{ flex: 1 }} />
        <button className="btn ghost" onClick={onClose}>
          cancel
        </button>
        <button className="btn primary" onClick={save}>
          {editing ? "save flow" : "create flow"}
        </button>
      </div>
    </div>
  );
}

// ---- runs table -------------------------------------------------------------

function RunsTable({ token, onUnauthorized, pollKey }) {
  const [runs, setRuns] = useState(null);
  const [err, setErr] = useState(null);
  const [open, setOpen] = useState(null);
  const [detail, setDetail] = useState(null);

  const load = useCallback(async () => {
    try {
      const r = await api.flowRuns(token);
      setRuns(r);
      setErr(null);
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setErr(e.message);
    }
  }, [token, onUnauthorized]);

  useEffect(() => {
    load();
    const id = setInterval(load, 5000);
    return () => clearInterval(id);
  }, [load, pollKey]);

  const toggle = async (id) => {
    if (open === id) {
      setOpen(null);
      setDetail(null);
      return;
    }
    setOpen(id);
    setDetail(null);
    try {
      setDetail(await api.flowRun(token, id));
    } catch (e) {
      if (!e.unauthorized) setErr(e.message);
    }
  };

  if (err) return <div className="banner err">{err}</div>;
  if (runs === null) return <div className="empty">Loading runs…</div>;
  if (runs.length === 0)
    return (
      <div className="empty">
        <p>No runs yet.</p>
        <p className="muted">
          A run starts when a flow's trigger fires — from a fresh metric sample
          (the sampler) or a synthetic trigger (the "test trigger" button above).
        </p>
      </div>
    );
  return (
    <div className="table-wrap">
      <table className="devices runs">
        <thead>
          <tr>
            <th>Flow</th>
            <th>Device</th>
            <th>Status</th>
            <th>Node</th>
            <th>Trigger</th>
            <th>Started</th>
            <th>Why / outcome</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {runs.map((r) => (
            <tr key={r.id} onClick={() => toggle(r.id)} className="run-row">
              <td>
                <div className="host">{r.flow_name}</div>
              </td>
              <td className="id">{r.device_id}</td>
              <td>{pill(r.status)}</td>
              <td className="mono">{r.current_node}</td>
              <td className="mono">{r.trigger_value != null ? String(r.trigger_value) : "—"}</td>
              <td className="muted">{relTime(r.started_at)}</td>
              <td className="muted run-reason">{r.reason || "—"}</td>
              <td className="muted">{open === r.id ? "▾" : "▸"}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {open !== null && detail && (
        <div className="run-detail">
          <div className="muted tiny">
            run {detail.run.id} · {detail.run.flow_name} · {detail.run.device_id} · {detail.run.status}
          </div>
          <table className="events">
            <thead>
              <tr>
                <th>node</th>
                <th>hop</th>
                <th>detail</th>
                <th>at</th>
              </tr>
            </thead>
            <tbody>
              {detail.events.map((e) => (
                <tr key={e.id}>
                  <td className="mono">{e.node}</td>
                  <td>{pill2(e.status)}</td>
                  <td className="muted">{e.reason || "—"}</td>
                  <td className="muted">{new Date(e.at).toLocaleTimeString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function pill2(status) {
  const cls =
    status === "ok" || status === "branched" ? "pill-ok" : status === "waiting" ? "pill-run" : status === "failed" || status === "timeout" ? "pill-bad" : "pill-mut";
  return <span className={"pill " + cls}>{status}</span>;
}

// ---- page -------------------------------------------------------------------

export default function Flows({ token, onUnauthorized }) {
  const [flows, setFlows] = useState(null);
  const [devices, setDevices] = useState([]);
  const [error, setError] = useState(null);
  const [composing, setComposing] = useState(null); // null | "new" | flow
  const [pollKey, setPollKey] = useState(0);

  const load = useCallback(async () => {
    try {
      const [f, d] = await Promise.all([api.flows(token), api.devices(token)]);
      setFlows(f);
      setDevices(d);
      setError(null);
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setError(e.message);
    }
  }, [token, onUnauthorized]);

  useEffect(() => {
    load();
  }, [load]);

  return (
    <section className="view">
      <div className="view-head">
        <div>
          <h2>Flows</h2>
          <p className="muted">
            {flows === null
              ? "loading…"
              : "event-driven automations — DAGs of trigger → actions, executed over the NATS event bus"}
          </p>
        </div>
        <div className="view-actions">
          <button className="btn ghost" onClick={load} title="Refresh now">
            ↻ refresh
          </button>
          <button className="btn primary" onClick={() => setComposing("new")}>
            + new flow
          </button>
        </div>
      </div>

      {error && <div className="banner err">{error}</div>}

      {composing !== null && (
        <Composer
          token={token}
          initial={composing === "new" ? null : composing}
          onClose={() => setComposing(null)}
          onSaved={() => {
            setComposing(null);
            load();
          }}
          onUnauthorized={onUnauthorized}
        />
      )}

      {flows !== null && !composing && (
        <div className="flow-list">
          {flows.length === 0 && (
            <div className="empty">
              <p>No flows yet.</p>
              <p className="muted">
                Compose one — e.g. <code>disk &gt; 90% → free space → if still &gt; 90% → notify</code> —
                then fire a synthetic trigger to watch the chain run over NATS.
              </p>
            </div>
          )}
          {flows.map((f) => (
            <div key={f.id} className="flow-wrap">
              <FlowCard
                token={token}
                flow={f}
                devices={devices}
                onUnauthorized={onUnauthorized}
                onChanged={load}
                onEdit={() => setComposing(f)}
              />
            </div>
          ))}
        </div>
      )}

      <div className="runs-head">
        <h3>Runs</h3>
        <button
          className="btn ghost"
          title="Re-cover in-flight runs (re-publish pending hops)"
          onClick={async () => {
            try {
              await api.sweepFlows(token);
              setPollKey((k) => k + 1);
            } catch (e) {
              if (!e.unauthorized) setError(e.message);
            }
          }}
        >
          sweep
        </button>
      </div>
      <RunsTable token={token} onUnauthorized={onUnauthorized} pollKey={pollKey} />
    </section>
  );
}
