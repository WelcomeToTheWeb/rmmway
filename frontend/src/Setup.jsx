import { useState } from "react";
import { api } from "./api.js";
import { useAuth } from "./auth.jsx";

// A-2: the first-boot setup wizard. Shown when the server reports a fresh
// database (GET /api/setup/status -> setup:false). One screen, three
// numbered sections: mint the initial root admin, define the organization
// (its name is stamped into the org root CA), and configure the SMTP outbox
// (optional — test it before finishing). Completing auto-signs-in with the
// minted credentials.
export default function Setup({ onDone }) {
  const { login } = useAuth();

  // 1. root admin
  const [user, setUser] = useState("admin");
  const [pass, setPass] = useState("");
  const [pass2, setPass2] = useState("");
  // 2. organization
  const [org, setOrg] = useState("");
  // 3. smtp outbox (optional)
  const [smtpOn, setSmtpOn] = useState(false);
  const [host, setHost] = useState("");
  const [port, setPort] = useState("587");
  const [from, setFrom] = useState("");
  const [suser, setSuser] = useState("");
  const [spass, setSpass] = useState("");
  const [testTo, setTestTo] = useState("");

  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState(null); // {ok, msg}

  function validate() {
    if (!user || user.trim().length < 3) return "Admin username must be 3-64 characters.";
    if (user.trim().length > 64) return "Admin username must be 3-64 characters.";
    if (pass.length < 8) return "Admin password must be at least 8 characters.";
    if (pass.length > 128) return "Admin password must be at most 128 characters.";
    if (pass !== pass2) return "Passwords do not match.";
    if (!org || !org.trim()) return "Organization name is required (it is stamped into the org root CA).";
    if (org.trim().length > 128) return "Organization name must be at most 128 characters.";
    if (smtpOn) {
      if (!host.trim()) return "SMTP host is required when the outbox is enabled.";
      const p = Number(port);
      if (!Number.isInteger(p) || p < 1 || p > 65535) return "SMTP port must be 1-65535.";
      if (!from || !from.includes("@")) return "SMTP from address must be a valid address.";
    }
    return null;
  }

  function smtpBody() {
    if (!smtpOn) return { host: "", port: 0, from: "", username: "", password: "" };
    return {
      host: host.trim(),
      port: Number(port) || 587,
      from: from.trim(),
      username: suser,
      password: spass,
    };
  }

  async function runTest() {
    setTesting(true);
    setTestResult(null);
    try {
      const res = await api.testSmtp({ smtp: smtpBody(), to: testTo || undefined });
      setTestResult({ ok: true, msg: `Test mail sent to ${res.to}.` });
    } catch (e) {
      setTestResult({ ok: false, msg: e.message || "test send failed" });
    } finally {
      setTesting(false);
    }
  }

  async function submit(e) {
    e?.preventDefault();
    const v = validate();
    if (v) {
      setError(v);
      return;
    }
    setBusy(true);
    setError(null);
    try {
      await api.completeSetup({
        admin_user: user.trim(),
        admin_password: pass,
        org_name: org.trim(),
        smtp: smtpBody(),
      });
      // The wizard is done — sign straight in with the minted credentials.
      const ok = await login(user.trim(), pass);
      if (!ok) throw new Error("setup completed, but the sign-in with the new credentials failed");
      onDone();
    } catch (err) {
      setError(err.message || "setup failed");
      setBusy(false);
    }
  }

  return (
    <div className="login-wrap">
      <form className="wizard card" onSubmit={submit}>
        <header className="login-head">
          <div className="brand">RMMWay</div>
          <p className="muted">First boot — initialize this server</p>
        </header>

        {/* 1. root admin */}
        <section className="wiz-section">
          <div className="wiz-step">
            <span className="step-idx">1</span>
            <h3>Root admin</h3>
            <p className="muted tiny">
              The initial operator account. It is stored hashed (PBKDF2) and
              is what you will sign in with.
            </p>
          </div>
          <label className="field">
            <span>Username</span>
            <input value={user} onChange={(e) => setUser(e.target.value)} autoComplete="username" spellCheck={false} />
          </label>
          <div className="wiz-row">
            <label className="field">
              <span>Password (min 8)</span>
              <input type="password" value={pass} onChange={(e) => setPass(e.target.value)} autoComplete="new-password" />
            </label>
            <label className="field">
              <span>Confirm password</span>
              <input type="password" value={pass2} onChange={(e) => setPass2(e.target.value)} autoComplete="new-password" />
            </label>
          </div>
        </section>

        {/* 2. organization + CA */}
        <section className="wiz-section">
          <div className="wiz-step">
            <span className="step-idx">2</span>
            <h3>Organization &amp; CA</h3>
            <p className="muted tiny">
              This name is stamped into the org root CA that every agent pins
              for mTLS. It can only be defined before the first device
              enrolls — this wizard is the moment.
            </p>
          </div>
          <label className="field">
            <span>Organization name</span>
            <input value={org} onChange={(e) => setOrg(e.target.value)} placeholder="Acme Corp" spellCheck={false} />
          </label>
        </section>

        {/* 3. smtp outbox */}
        <section className="wiz-section">
          <div className="wiz-step">
            <span className="step-idx">3</span>
            <h3>SMTP outbox</h3>
            <p className="muted tiny">
              Optional — where the server sends mail (alert/operator
              notifications). Port 587 = STARTTLS, 465 = implicit TLS,
              25 = plaintext. Test it before finishing.
            </p>
          </div>
          <label className="field check">
            <input type="checkbox" checked={smtpOn} onChange={(e) => setSmtpOn(e.target.checked)} />
            <span>Configure SMTP now (leave unchecked to skip)</span>
          </label>
          {smtpOn && (
            <>
              <div className="wiz-row">
                <label className="field">
                  <span>Host</span>
                  <input value={host} onChange={(e) => setHost(e.target.value)} placeholder="smtp.example.com" spellCheck={false} />
                </label>
                <label className="field">
                  <span>Port</span>
                  <input value={port} onChange={(e) => setPort(e.target.value)} inputMode="numeric" />
                </label>
              </div>
              <label className="field">
                <span>From address</span>
                <input value={from} onChange={(e) => setFrom(e.target.value)} placeholder="rmmway@example.com" spellCheck={false} />
              </label>
              <div className="wiz-row">
                <label className="field">
                  <span>Auth username (optional)</span>
                  <input value={suser} onChange={(e) => setSuser(e.target.value)} autoComplete="off" spellCheck={false} />
                </label>
                <label className="field">
                  <span>Auth password (optional)</span>
                  <input type="password" value={spass} onChange={(e) => setSpass(e.target.value)} autoComplete="new-password" />
                </label>
              </div>
              <label className="field">
                <span>Test recipient (defaults to the from address)</span>
                <input value={testTo} onChange={(e) => setTestTo(e.target.value)} placeholder={from || "you@example.com"} spellCheck={false} />
              </label>
              <div className="wiz-test-row">
                <button type="button" className="btn" onClick={runTest} disabled={testing || !host || !from}>
                  {testing ? "Sending…" : "Send test email"}
                </button>
                {testResult && (
                  <span className={testResult.ok ? "wiz-test ok" : "wiz-test err"}>
                    {testResult.msg}
                  </span>
                )}
              </div>
            </>
          )}
        </section>

        {error && <div className="banner err">{error}</div>}

        <button className="btn primary" type="submit" disabled={busy}>
          {busy ? "Initializing…" : "Complete setup"}
        </button>
        <p className="muted tiny">
          After this step the wizard is gone (it runs once per database).
          You will be signed in as the new admin.
        </p>
      </form>
    </div>
  );
}
