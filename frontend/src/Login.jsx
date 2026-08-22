import { useState, useEffect, useRef } from "react";
import { useAuth } from "./auth.jsx";

export default function Login() {
  const { login, booting, error } = useAuth();
  const [user, setUser] = useState("admin");
  const [pass, setPass] = useState("");
  const [touched, setTouched] = useState(false);
  const passRef = useRef(null);

  useEffect(() => {
    // Move focus to the password field on mount (username defaults to
    // "admin" so the operator only has to type the password).
    passRef.current?.focus();
  }, []);

  async function submit(e) {
    e?.preventDefault();
    setTouched(true);
    await login(user, pass);
  }

  return (
    <div className="login-wrap">
      <form className="login card" onSubmit={submit}>
        <header className="login-head">
          <div className="brand">RMMWay</div>
          <p className="muted">Sign in to continue</p>
        </header>
        <label className="field">
          <span>Username</span>
          <input
            value={user}
            onChange={(e) => setUser(e.target.value)}
            autoComplete="username"
            spellCheck={false}
          />
        </label>
        <label className="field">
          <span>Password</span>
          <input
            ref={passRef}
            type="password"
            value={pass}
            onChange={(e) => setPass(e.target.value)}
            autoComplete="current-password"
          />
        </label>
        {touched && error ? (
          <div className="banner err">{error}</div>
        ) : null}
        <button className="btn primary" type="submit" disabled={booting}>
          {booting ? "Signing in…" : "Sign in"}
        </button>
        <p className="muted tiny">
          Local dev default: <code>admin</code> / <code>admin</code> —
          override with <code>RMMWAY_ADMIN_USER</code> / <code>RMMWAY_ADMIN_PASSWORD</code>.
        </p>
      </form>
    </div>
  );
}
