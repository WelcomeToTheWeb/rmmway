import { useCallback, useEffect, useState } from "react";
import { api } from "./api.js";

// D-6: notification channels + policies dashboard.
// Two tabs: Channels (CRUD + test-fire) and Policies (CRUD).
// Channels: email, slack, teams, pagerduty, webhook.
// Policies: per-category routing by client/role.

const CHANNEL_TYPES = [
  { value: "email", label: "Email (SMTP)" },
  { value: "slack", label: "Slack" },
  { value: "teams", label: "Microsoft Teams" },
  { value: "pagerduty", label: "PagerDuty" },
  { value: "webhook", label: "Generic Webhook" },
];

const POLICY_CATEGORIES = [
  { value: "alert", label: "Alert" },
  { value: "escalation", label: "Escalation" },
];

const ROLES = [
  { value: "", label: "Any role" },
  { value: "admin", label: "Admin" },
  { value: "tech", label: "Tech" },
  { value: "viewer", label: "Viewer" },
];

// Channel configuration forms (simplified for demo).
function ChannelForm({ type }) {
  return (
    <div className="nt-channel-form">
      {type === "email" && (
        <>
          <input name="host" placeholder="SMTP host" />
          <input name="port" placeholder="Port (587)" type="number" />
          <input name="from" placeholder="From address" />
          <input name="to" placeholder="To address" />
          <input name="username" placeholder="Username (optional)" />
          <input name="password" placeholder="Password (optional)" type="password" />
        </>
      )}
      {(type === "slack" || type === "teams" || type === "webhook") && (
        <input name="webhook_url" placeholder="Webhook URL" />
      )}
      {type === "slack" && <input name="channel" placeholder="Channel (#general)" />}
      {type === "pagerduty" && (
        <input name="api_key" placeholder="Routing key / API key" />
      )}
    </div>
  );
}

export default function Notify() {
  const [tab, setTab] = useState("channels");
  const [channels, setChannels] = useState([]);
  const [policies, setPolicies] = useState([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);
  const [showChannelForm, setShowChannelForm] = useState(false);
  const [showPolicyForm, setShowPolicyForm] = useState(false);
  const [busy, setBusy] = useState(false);

  const loadAll = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const [chRes, polRes] = await Promise.all([
        api.get("/api/notify/channels"),
        api.get("/api/notify/policies"),
      ]);
      setChannels(chRes.data);
      setPolicies(polRes.data);
    } catch (e) {
      setError("Failed to load: " + e.message);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    loadAll();
  }, [loadAll]);

  const createChannel = async (type) => {
    setBusy(true);
    try {
      const res = await api.post("/api/notify/channels", {
        type: type,
        name: type.charAt(0).toUpperCase() + type.slice(1) + " channel",
        config: {},
        enabled: true,
      });
      setChannels([...channels, res.data]);
    } catch (e) {
      setError("Failed to create channel: " + e.message);
    } finally {
      setBusy(false);
      setShowChannelForm(false);
    }
  };

  const testChannel = async (id) => {
    setBusy(true);
    try {
      await api.post(`/api/notify/channels/${id}/test`);
    } catch (e) {
      setError("Test failed: " + e.message);
    } finally {
      setBusy(false);
    }
  };

  const createPolicy = async (category, channels, clientID, role) => {
    setBusy(true);
    try {
      const res = await api.post("/api/notify/policies", {
        category: category,
        channels: channels,
        client_id: clientID || null,
        role: role || null,
        enabled: true,
      });
      setPolicies([...policies, res.data]);
    } catch (e) {
      setError("Failed to create policy: " + e.message);
    } finally {
      setBusy(false);
      setShowPolicyForm(false);
    }
  };

  return (
    <div className="notify-page">
      <h2>Notifications</h2>
      <div className="nt-tabs">
        <button
          className={tab === "channels" ? "btn active" : "btn ghost"}
          onClick={() => setTab("channels")}
        >
          Channels
        </button>
        <button
          className={tab === "policies" ? "btn active" : "btn ghost"}
          onClick={() => setTab("policies")}
        >
          Policies
        </button>
      </div>
      {error && <div className="banner err">{error}</div>}
      {tab === "channels" && (
        <div>
          {!showChannelForm && (
            <button
              className="btn"
              onClick={() => setShowChannelForm(true)}
            >
              + New channel
            </button>
          )}
          {showChannelForm && (
            <div className="nt-form">
              <h3>New channel</h3>
              <select
                id="nt-channel-type"
                defaultValue=""
              >
                <option value="" disabled>Select type…</option>
                {CHANNEL_TYPES.map((t) => (
                  <option key={t.value} value={t.value}>
                    {t.label}
                  </option>
                ))}
              </select>
              <button
                className="btn small"
                disabled={busy}
                onClick={() => {
                  const type = document.getElementById("nt-channel-type").value;
                  if (type) createChannel(type);
                }}
              >
                Create
              </button>
              <button
                className="btn small ghost"
                onClick={() => setShowChannelForm(false)}
              >
                Cancel
              </button>
            </div>
          )}
          {loading ? (
            <p className="muted">Loading channels…</p>
          ) : channels.length === 0 ? (
            <p className="muted">No channels configured.</p>
          ) : (
            <div className="table-wrap">
              <table className="nt-table">
                <thead>
                  <tr>
                    <th>Name</th>
                    <th>Type</th>
                    <th>Enabled</th>
                    <th>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {channels.map((ch) => (
                    <tr key={ch.id}>
                      <td>{ch.name}</td>
                      <td>{ch.type}</td>
                      <td>
                        {ch.enabled ? (
                          <span className="badge success">enabled</span>
                        ) : (
                          <span className="badge">disabled</span>
                        )}
                      </td>
                      <td>
                        <button
                          className="btn small ghost"
                          disabled={busy}
                          onClick={() => testChannel(ch.id)}
                        >
                          Test
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      )}
      {tab === "policies" && (
        <div>
          {!showPolicyForm && (
            <button
              className="btn"
              onClick={() => setShowPolicyForm(true)}
            >
              + New policy
            </button>
          )}
          {showPolicyForm && (
            <div className="nt-form">
              <h3>New policy</h3>
              <select id="nt-policy-category">
                {POLICY_CATEGORIES.map((c) => (
                  <option key={c.value} value={c.value}>
                    {c.label}
                  </option>
                ))}
              </select>
              <select id="nt-policy-role">
                {ROLES.map((r) => (
                  <option key={r.value} value={r.value}>
                    {r.label}
                  </option>
                ))}
              </select>
              <button
                className="btn small"
                disabled={busy}
                onClick={() => {
                  const category = document.getElementById("nt-policy-category").value;
                  const role = document.getElementById("nt-policy-role").value;
                  createPolicy(category, [], null, role || null);
                }}
              >
                Create
              </button>
              <button
                className="btn small ghost"
                onClick={() => setShowPolicyForm(false)}
              >
                Cancel
              </button>
            </div>
          )}
          {loading ? (
            <p className="muted">Loading policies…</p>
          ) : policies.length === 0 ? (
            <p className="muted">No policies configured.</p>
          ) : (
            <div className="table-wrap">
              <table className="nt-table">
                <thead>
                  <tr>
                    <th>Category</th>
                    <th>Role</th>
                    <th>Channels</th>
                    <th>Enabled</th>
                  </tr>
                </thead>
                <tbody>
                  {policies.map((p) => (
                    <tr key={p.id}>
                      <td>{p.category}</td>
                      <td>{p.role || "any"}</td>
                      <td>{p.channels.length} channel(s)</td>
                      <td>
                        {p.enabled ? (
                          <span className="badge success">enabled</span>
                        ) : (
                          <span className="badge">disabled</span>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      )}
    </div>
  );
}