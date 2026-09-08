import { useCallback, useEffect, useState } from "react";
import { api } from "./api.js";
import { Banner, Button, Field } from "./ui/index.js";

// C #10a: the settings/profile page (wave 1). Operator-gated, reachable
// AFTER first boot (the Setup wizard is the pre-setup surface):
//   Organization — read-only: the name is stamped into the org root CA
//   SMTP outbox  — edit the stored config, test it by actually sending
//                  (the test uses the SAVED config, not the draft)
//   Profile      — change password; 2FA status row (not yet enabled —
//                  TOTP arrives in wave 2, B #3)
// In-memory mode (no database): the read model still renders (env
// defaults); saves and sends surface the server's 503 message.
export default function Settings({ token, onUnauthorized }) {
  const [data, setData] = useState(null);
  const [loadError, setLoadError] = useState(null);
  const [reloading, setReloading] = useState(false);

  // SMTP draft. `password` is always blank on load — the read model never
  // returns it (pass_set only says one is on file); blank = keep stored.
  const [smtp, setSmtp] = useState({
    host: "",
    port: "587",
    from: "",
    username: "",
    password: "",
  });
  const [smtpErr, setSmtpErr] = useState(null);
  const [smtpBusy, setSmtpBusy] = useState(false);
  const [smtpSaved, setSmtpSaved] = useState(false);
  const [testTo, setTestTo] = useState("");
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState(null); // {ok, msg}

  // Profile: password change + 2FA status.
  const [pw, setPw] = useState({ current: "", next: "", confirm: "" });
  const [pwErr, setPwErr] = useState(null);
  const [pwBusy, setPwBusy] = useState(false);
  const [pwSaved, setPwSaved] = useState(false);

  const load = useCallback(
    async (reloadingRun = false) => {
      if (reloadingRun) setReloading(true);
      setLoadError(null);
      try {
        const res = await api.getSettings(token);
        setData(res);
        const s = res.smtp || {};
        setSmtp({
          host: s.host || "",
          port: String(s.port || 587),
          from: s.from || "",
          username: s.username || "",
          password: "",
        });
      } catch (e) {
        if (e.unauthorized) onUnauthorized();
        else setLoadError(e.message || "failed to load settings");
      } finally {
        setReloading(false);
      }
    },
    [token, onUnauthorized],
  );

  useEffect(() => {
    load();
  }, [load]);

  // Mirrors the server's smtp.Config.Normalize: a blank host means
  // "clear the outbox" (port/from are ignored); otherwise port must be
  // 1-65535 and from a plausible address.
  function smtpError(f) {
    if (!f.host.trim()) return null;
    const p = Number(f.port);
    if (!Number.isInteger(p) || p < 1 || p > 65535)
      return "SMTP port must be 1-65535.";
    if (!f.from.trim() || !f.from.includes("@"))
      return "SMTP from address must be a valid address.";
    return null;
  }

  async function saveSmtp(e) {
    e.preventDefault();
    if (smtpBusy) return;
    const v = smtpError(smtp);
    if (v) {
      setSmtpErr(v);
      setSmtpSaved(false);
      return;
    }
    setSmtpBusy(true);
    setSmtpErr(null);
    setSmtpSaved(false);
    try {
      const res = await api.patchSettings(token, {
        host: smtp.host.trim(),
        port: smtp.host.trim() ? Number(smtp.port) || 587 : 0,
        from: smtp.from.trim(),
        username: smtp.username.trim(),
        password: smtp.password,
      });
      if (res && res.smtp) setData((d) => ({ ...d, smtp: res.smtp }));
      setSmtp((s) => ({ ...s, password: "" }));
      setSmtpSaved(true);
    } catch (ex) {
      if (ex.unauthorized) onUnauthorized();
      else setSmtpErr(ex.message || "failed to save SMTP settings");
    } finally {
      setSmtpBusy(false);
    }
  }

  // Sends via the STORED config — if the draft above hasn't been saved yet,
  // this tests what is actually saved, not the form.
  async function runTest() {
    if (testing) return;
    setTesting(true);
    setTestResult(null);
    try {
      const res = await api.testSMTP(token, { to: testTo.trim() });
      setTestResult({ ok: true, msg: `Test mail sent to ${res.to}.` });
    } catch (ex) {
      if (ex.unauthorized) onUnauthorized();
      else setTestResult({ ok: false, msg: ex.message || "test send failed" });
    } finally {
      setTesting(false);
    }
  }

  function pwError() {
    if (!pw.current) return "Enter your current password.";
    if (pw.next.length < 8)
      return "New password must be at least 8 characters.";
    if (pw.next.length > 128)
      return "New password must be at most 128 characters.";
    if (pw.next !== pw.confirm) return "Passwords do not match.";
    return null;
  }

  async function changePw(e) {
    e.preventDefault();
    if (pwBusy) return;
    const v = pwError();
    if (v) {
      setPwErr(v);
      setPwSaved(false);
      return;
    }
    setPwBusy(true);
    setPwErr(null);
    setPwSaved(false);
    try {
      await api.changePassword(token, {
        current_password: pw.current,
        new_password: pw.next,
      });
      setPw({ current: "", next: "", confirm: "" });
      setPwSaved(true);
    } catch (ex) {
      if (ex.unauthorized) onUnauthorized();
      else setPwErr(ex.message || "failed to change password");
    } finally {
      setPwBusy(false);
    }
  }

  const profile = data?.profile || {};
  const s = data?.smtp || {};

  return (
    <div className="view">
      <div className="view-head">
        <div>
          <h2>Settings</h2>
          <p className="muted">
            Server configuration and your operator account.
          </p>
        </div>
      </div>

      {loadError && (
        <Banner tone="err">
          {loadError}{" "}
          <Button variant="ghost" onClick={() => load(true)} busy={reloading}>
            Retry
          </Button>
        </Banner>
      )}

      {!data && !loadError && <div className="empty">Loading settings…</div>}

      {data && (
        <>
          {/* Organization — read-only (CA-stamped). */}
          <section className="settings-card">
            <h3>Organization</h3>
            <p className="muted">
              The organization name is stamped into the org root CA that every
              agent pins for mTLS — it cannot be changed after first boot.
            </p>
            <Field
              label="Organization name"
              value={data.org_name || ""}
              readOnly
              hint={
                data.org_name
                  ? "set during first-boot setup"
                  : "not set (in-memory server)"
              }
            />
          </section>

          {/* SMTP outbox — edit + test. */}
          <section className="settings-card">
            <h3>
              SMTP outbox{" "}
              {s.configured ? (
                <span className="settings-badge on">configured</span>
              ) : (
                <span className="settings-badge">not configured</span>
              )}
            </h3>
            <p className="muted">
              Where the server sends mail (alert and operator notifications).
              Port 587 = STARTTLS, 465 = implicit TLS, 25 = plaintext. Leave the
              host blank to clear the outbox.
            </p>
            <form onSubmit={saveSmtp} noValidate>
              <div className="settings-grid">
                <Field
                  label="Host"
                  value={smtp.host}
                  onChange={(e) =>
                    setSmtp((f) => ({ ...f, host: e.target.value }))
                  }
                  placeholder="smtp.example.com"
                  spellCheck={false}
                  autoComplete="off"
                />
                <Field
                  label="Port"
                  value={smtp.port}
                  onChange={(e) =>
                    setSmtp((f) => ({ ...f, port: e.target.value }))
                  }
                  inputMode="numeric"
                  autoComplete="off"
                />
                <Field
                  label="From address"
                  value={smtp.from}
                  onChange={(e) =>
                    setSmtp((f) => ({ ...f, from: e.target.value }))
                  }
                  placeholder="rmmway@example.com"
                  spellCheck={false}
                  autoComplete="off"
                />
                <Field
                  label="Auth username (optional)"
                  value={smtp.username}
                  onChange={(e) =>
                    setSmtp((f) => ({ ...f, username: e.target.value }))
                  }
                  autoComplete="off"
                  spellCheck={false}
                />
                <Field
                  label={
                    s.pass_set
                      ? "Auth password (leave blank to keep)"
                      : "Auth password (optional)"
                  }
                  type="password"
                  value={smtp.password}
                  onChange={(e) =>
                    setSmtp((f) => ({ ...f, password: e.target.value }))
                  }
                  autoComplete="new-password"
                />
              </div>
              {smtpErr && (
                <Banner tone="err" className="settings-err">
                  {smtpErr}
                </Banner>
              )}
              <div className="settings-actions">
                <Button variant="primary" type="submit" busy={smtpBusy}>
                  Save SMTP
                </Button>
                {smtpSaved && (
                  <span className="muted settings-save-msg">Saved.</span>
                )}
              </div>
            </form>
            <div className="settings-test-row">
              <Field
                label="Test recipient (defaults to the from address)"
                value={testTo}
                onChange={(e) => setTestTo(e.target.value)}
                placeholder={s.from || "you@example.com"}
                spellCheck={false}
                autoComplete="off"
                className="settings-test-recipient"
              />
              <Button onClick={runTest} busy={testing}>
                {testing ? "Sending…" : "Send test email"}
              </Button>
            </div>
            <p className="muted tiny settings-test-hint">
              The test sends through the SAVED configuration — save changes
              first.
            </p>
            {testResult && (
              <Banner tone={testResult.ok ? "ok" : "err"}>
                {testResult.msg}
              </Banner>
            )}
          </section>

          {/* Profile — password + 2FA status. */}
          <section className="settings-card">
            <h3>Profile</h3>
            <Field
              label="Username"
              value={profile.username || ""}
              readOnly
              hint={
                data.org_name || s.configured
                  ? "signed in from the database"
                  : "signed in from server environment"
              }
            />
            <form className="settings-pw-form" onSubmit={changePw} noValidate>
              <div className="settings-grid">
                <Field
                  label="Current password"
                  type="password"
                  value={pw.current}
                  onChange={(e) =>
                    setPw((f) => ({ ...f, current: e.target.value }))
                  }
                  autoComplete="current-password"
                />
                <Field
                  label="New password (min 8)"
                  type="password"
                  value={pw.next}
                  onChange={(e) =>
                    setPw((f) => ({ ...f, next: e.target.value }))
                  }
                  autoComplete="new-password"
                />
                <Field
                  label="Confirm new password"
                  type="password"
                  value={pw.confirm}
                  onChange={(e) =>
                    setPw((f) => ({ ...f, confirm: e.target.value }))
                  }
                  autoComplete="new-password"
                />
              </div>
              {pwErr && (
                <Banner tone="err" className="settings-err">
                  {pwErr}
                </Banner>
              )}
              <div className="settings-actions">
                <Button variant="primary" type="submit" busy={pwBusy}>
                  Change password
                </Button>
                {pwSaved && (
                  <span className="muted settings-save-msg">
                    Password changed — use it on your next sign-in.
                  </span>
                )}
              </div>
            </form>
            <div className="settings-mfa">
              <span>Two-factor authentication</span>
              <span className="muted">
                {profile.mfa_enabled
                  ? "enabled"
                  : "not yet enabled (arrives in wave 2)"}
              </span>
            </div>
          </section>
        </>
      )}
    </div>
  );
}
