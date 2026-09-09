import { useCallback, useEffect, useState } from "react";
import { api } from "./api.js";
import {
  Button,
  Banner,
  Table,
  EmptyState,
  Modal,
  Field,
  RelTime,
} from "./ui/index.js";

// Users is the operator-account view (gap #3, wave 2, lane B): the account
// list with roles + per-client grants, TOTP MFA setup, and API tokens.
// The route is admin-gated server-side (tech/viewer get 403).
const ROLES = [
  { id: "admin", label: "Admin — everything" },
  { id: "tech", label: "Tech — operational on granted clients" },
  { id: "viewer", label: "Viewer — read-only on granted clients" },
];
const ROLE_LABEL = { admin: "admin", tech: "tech", viewer: "viewer" };

function grantCheckboxes(clients, value, onChange, disabled) {
  if (!clients.length)
    return (
      <span className="muted small">
        No clients yet — grants can be added from the Clients view.
      </span>
    );
  return (
    <div className="user-grants">
      {clients.map((c) => (
        <label key={c.id} className="user-grant">
          <input
            type="checkbox"
            checked={value.includes(c.id)}
            disabled={disabled}
            onChange={() => {
              const next = value.includes(c.id)
                ? value.filter((x) => x !== c.id)
                : [...value, c.id];
              onChange(next);
            }}
          />
          <span>{c.name}</span>
        </label>
      ))}
    </div>
  );
}

// NewUserModal — create: username, password, role, client grants.
function NewUserModal({ clients, onClose, onCreate }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState("viewer");
  const [grants, setGrants] = useState([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);

  async function submit() {
    setBusy(true);
    setError(null);
    try {
      await onCreate({ username, password, role, clients: grants });
    } catch (e) {
      if (e.unauthorized) return;
      setError(e.message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      title="New operator"
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button
            onClick={submit}
            disabled={busy || !username.trim() || password.length < 8}
          >
            {busy ? "creating…" : "Create operator"}
          </Button>
        </>
      }
    >
      <Field
        label="Username"
        placeholder="e.g. jsmith"
        value={username}
        disabled={busy}
        autoFocus
        onChange={(e) => setUsername(e.target.value)}
      />
      <Field
        label="Password"
        type="password"
        hint="At least 8 characters."
        value={password}
        disabled={busy}
        onChange={(e) => setPassword(e.target.value)}
      />
      <Field
        label="Role"
        as="select"
        value={role}
        disabled={busy}
        onChange={(e) => setRole(e.target.value)}
      >
        {ROLES.map((r) => (
          <option key={r.id} value={r.id}>
            {r.label}
          </option>
        ))}
      </Field>
      <div className="field">
        <span>Clients</span>
        {grantCheckboxes(clients, grants, setGrants, busy)}
      </div>
      {error && <Banner tone="err">{error}</Banner>}
    </Modal>
  );
}

// EditUserModal — role, enabled, grants, optional password reset.
function EditUserModal({ user, clients, onClose, onSave }) {
  const [role, setRole] = useState(user.role);
  const [enabled, setEnabled] = useState(user.enabled);
  const [grants, setGrants] = useState(user.clients || []);
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);

  async function submit() {
    setBusy(true);
    setError(null);
    try {
      await onSave({
        role,
        enabled,
        clients: grants,
        password: password || undefined,
      });
    } catch (e) {
      if (e.unauthorized) return;
      setError(e.message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      title={`Edit ${user.username}`}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button onClick={submit} disabled={busy}>
            {busy ? "saving…" : "Save"}
          </Button>
        </>
      }
    >
      <Field
        label="Role"
        as="select"
        value={role}
        disabled={busy}
        onChange={(e) => setRole(e.target.value)}
      >
        {ROLES.map((r) => (
          <option key={r.id} value={r.id}>
            {r.label}
          </option>
        ))}
      </Field>
      <label className="field">
        <span>Enabled</span>
        <input
          type="checkbox"
          checked={enabled}
          disabled={busy}
          onChange={(e) => setEnabled(e.target.checked)}
        />
      </label>
      <div className="field">
        <span>Clients</span>
        {grantCheckboxes(clients, grants, setGrants, busy)}
      </div>
      <Field
        label="Reset password"
        type="password"
        hint="Leave blank to keep the current password."
        value={password}
        disabled={busy}
        onChange={(e) => setPassword(e.target.value)}
      />
      {error && <Banner tone="err">{error}</Banner>}
    </Modal>
  );
}

// TotpModal — MFA setup: start → show secret/URI → confirm with a code.
// Enrollment is live from `start`, so the confirm step must complete
// before closing (v1 lockout window, server-side by design).
function TotpModal({ user, token, onClose, onConfirm, onClear }) {
  const [secret, setSecret] = useState(null);
  const [uri, setUri] = useState("");
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);

  useEffect(() => {
    let cancelled = false;
    async function start() {
      try {
        const r = await api.startUserTotp(token, user.id);
        if (!cancelled) {
          setSecret(r.secret);
          setUri(r.uri);
        }
      } catch (e) {
        if (!cancelled) setError(e.unauthorized ? null : e.message);
      }
    }
    start();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  async function confirm() {
    setBusy(true);
    setError(null);
    try {
      await onConfirm(code);
    } catch (e) {
      if (e.unauthorized) return;
      setError(e.message);
    } finally {
      setBusy(false);
    }
  }

  if (secret == null) {
    return (
      <Modal title={`MFA for ${user.username}`} onClose={onClose}>
        {error ? (
          <Banner tone="err">{error}</Banner>
        ) : (
          <EmptyState title="Starting enrollment…" />
        )}
      </Modal>
    );
  }

  return (
    <Modal
      title={`MFA for ${user.username}`}
      onClose={onClose}
      footer={
        <>
          <Button
            variant="ghost"
            onClick={async () => {
              try {
                await onClear();
              } catch (e) {
                setError(e.message);
              }
            }}
          >
            Cancel &amp; reset
          </Button>
          <Button onClick={confirm} disabled={busy || code.length !== 6}>
            {busy ? "confirming…" : "Confirm code"}
          </Button>
        </>
      }
    >
      <p className="muted small">
        Enrollment is live — {user.username} needs the 6-digit code to log in
        from now on. Enter the code from your authenticator app to finish (or
        “Cancel &amp; reset” to undo).
      </p>
      <Field
        label="Secret (manual entry)"
        value={secret}
        readOnly
        className="mono"
      />
      <Field
        label="otpauth URI"
        value={uri}
        readOnly
        className="mono"
        hint="Paste into your authenticator app if it supports URIs."
      />
      <Field
        label="6-digit code"
        value={code}
        inputMode="numeric"
        maxLength={6}
        disabled={busy}
        onChange={(e) => setCode(e.target.value.replace(/[^0-9]/g, ""))}
      />
      {error && <Banner tone="err">{error}</Banner>}
    </Modal>
  );
}

// TokensModal — the account's API tokens: create (full token shown once),
// list, revoke.
function TokensModal({ user, token, onClose }) {
  const [tokens, setTokens] = useState(null);
  const [name, setName] = useState("");
  const [ttl, setTtl] = useState(30);
  const [fresh, setFresh] = useState(null); // the just-minted full token
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);

  async function list() {
    const rows = await api.listUserTokens(token, user.id);
    setTokens(rows);
  }

  useEffect(() => {
    list().catch((e) => {
      if (!e.unauthorized) setError(e.message);
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  async function create() {
    setBusy(true);
    setError(null);
    try {
      const r = await api.createUserToken(token, user.id, {
        name,
        ttl_days: ttl,
      });
      setFresh(r);
      setName("");
      await list();
    } catch (e) {
      if (e.unauthorized) return;
      setError(e.message);
    } finally {
      setBusy(false);
    }
  }

  async function revoke(tid) {
    setBusy(true);
    setError(null);
    try {
      await api.revokeUserToken(token, user.id, tid);
      await list();
    } catch (e) {
      if (!e.unauthorized) setError(e.message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      title={`API tokens — ${user.username}`}
      onClose={onClose}
      footer={
        <Button variant="ghost" onClick={onClose}>
          Close
        </Button>
      }
    >
      {fresh && (
        <div className="user-token-fresh">
          <Banner tone="ok">
            Token created — copy it now; it is not shown again.
          </Banner>
          <code className="mono">{fresh.token}</code>
          <Button
            variant="ghost"
            onClick={() => navigator.clipboard?.writeText(fresh.token)}
          >
            Copy
          </Button>
        </div>
      )}
      <div className="user-token-new">
        <Field
          label="Name"
          placeholder="e.g. CI runner"
          value={name}
          disabled={busy}
          onChange={(e) => setName(e.target.value)}
        />
        <Field
          label="TTL (days)"
          type="number"
          min={1}
          value={ttl}
          disabled={busy}
          onChange={(e) => setTtl(Number(e.target.value) || 30)}
        />
        <Button onClick={create} disabled={busy || !name.trim()}>
          {busy ? "minting…" : "Mint token"}
        </Button>
      </div>
      {tokens == null ? (
        <EmptyState title="Loading tokens…" />
      ) : tokens.length === 0 ? (
        <EmptyState title="No API tokens" />
      ) : (
        <Table className="users-tokens">
          <thead>
            <tr>
              <th>Name</th>
              <th>Prefix</th>
              <th>Created</th>
              <th>Expires</th>
              <th>Last used</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {tokens.map((tk) => (
              <tr key={tk.id}>
                <td>{tk.name}</td>
                <td className="mono muted">{tk.prefix}</td>
                <td className="muted">
                  <RelTime iso={tk.created_at} />
                </td>
                <td className="muted">
                  {tk.expires_at ? <RelTime iso={tk.expires_at} /> : "never"}
                </td>
                <td className="muted">
                  {tk.last_used_at ? <RelTime iso={tk.last_used_at} /> : "—"}
                </td>
                <td>
                  <Button
                    variant="ghost"
                    disabled={busy}
                    onClick={() => revoke(tk.id)}
                  >
                    Revoke
                  </Button>
                </td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
      {error && <Banner tone="err">{error}</Banner>}
    </Modal>
  );
}

export default function Users({ token, onUnauthorized }) {
  const [users, setUsers] = useState(null);
  const [clients, setClients] = useState([]);
  const [error, setError] = useState(null);
  const [unavailable, setUnavailable] = useState(false);
  const [modal, setModal] = useState(null); // {kind, user?}

  const load = useCallback(async () => {
    try {
      const [u, c] = await Promise.all([
        api.users(token),
        api.clients(token).catch(() => []),
      ]);
      setUsers(u);
      setClients(c || []);
    } catch (e) {
      if (e.unauthorized) return onUnauthorized();
      if (e.status === 403)
        return setError("Operator management is admin-only.");
      if (e.status === 503) return setUnavailable(true);
      setError(e.message);
    }
  }, [token, onUnauthorized]);

  useEffect(() => {
    load();
  }, [load]);

  if (unavailable)
    return (
      <EmptyState
        title="Operator accounts need a database"
        subtitle="This server runs in-memory mode — the account model (users, roles, MFA, API tokens) is not available."
      />
    );

  return (
    <div className="users-view">
      <header className="view-head">
        <div>
          <h1>Operators</h1>
          <p className="muted small">
            Accounts, roles, client grants, MFA, and API tokens.
          </p>
        </div>
        <Button onClick={() => setModal({ kind: "new" })}>New operator</Button>
      </header>

      {error && <Banner tone="err">{error}</Banner>}

      {users == null ? (
        <EmptyState title="Loading operators…" />
      ) : users.length === 0 ? (
        <EmptyState
          title="No operator accounts yet"
          subtitle="Create the first operator account. The wizard admin and the environment pair keep working until the first account exists."
        />
      ) : (
        <Table className="users" sticky>
          <thead>
            <tr>
              <th>Username</th>
              <th>Role</th>
              <th>Status</th>
              <th>MFA</th>
              <th>Clients</th>
              <th>Last login</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {users.map((u) => (
              <tr key={u.id} className={u.enabled ? "" : "disabled"}>
                <td>
                  <div className="host">{u.username}</div>
                  <div className="id mono">{u.id}</div>
                </td>
                <td>
                  <span className={"role role-" + u.role}>
                    {ROLE_LABEL[u.role] || u.role}
                  </span>
                </td>
                <td className={u.enabled ? "muted" : "row-err"}>
                  {u.enabled ? "active" : "disabled"}
                </td>
                <td className="muted">
                  {u.totp_enrolled
                    ? u.totp_verified
                      ? "enabled"
                      : "enrolled — unverified"
                    : "—"}
                </td>
                <td className="muted">
                  {u.role === "admin"
                    ? "all"
                    : (u.clients || []).length === 0
                      ? "none"
                      : (u.clients || []).join(", ")}
                </td>
                <td className="muted">
                  <RelTime iso={u.last_login_at} />
                </td>
                <td className="row-actions">
                  <Button
                    variant="ghost"
                    onClick={() => setModal({ kind: "edit", user: u })}
                  >
                    Edit
                  </Button>
                  <Button
                    variant="ghost"
                    onClick={() => setModal({ kind: "totp", user: u })}
                  >
                    MFA
                  </Button>
                  <Button
                    variant="ghost"
                    onClick={() => setModal({ kind: "tokens", user: u })}
                  >
                    Tokens
                  </Button>
                </td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}

      {modal?.kind === "new" && (
        <NewUserModal
          clients={clients}
          onClose={() => setModal(null)}
          onCreate={async (body) => {
            await api.createUser(token, body);
            setModal(null);
            await load();
          }}
        />
      )}
      {modal?.kind === "edit" && (
        <EditUserModal
          user={modal.user}
          clients={clients}
          onClose={() => setModal(null)}
          onSave={async (body) => {
            await api.updateUser(token, modal.user.id, body);
            setModal(null);
            await load();
          }}
        />
      )}
      {modal?.kind === "totp" && (
        <TotpModal
          user={modal.user}
          token={token}
          onClose={() => {
            setModal(null);
            load();
          }}
          onConfirm={async (code) => {
            await api.confirmUserTotp(token, modal.user.id, code);
            setModal(null);
            await load();
          }}
          onClear={async () => {
            await api.clearUserTotp(token, modal.user.id);
            setModal(null);
            await load();
          }}
        />
      )}
      {modal?.kind === "tokens" && (
        <TokensModal
          user={modal.user}
          token={token}
          onClose={() => setModal(null)}
        />
      )}
    </div>
  );
}
