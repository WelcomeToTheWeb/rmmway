import { useEffect, useState } from "react";
import { AuthProvider, useAuth } from "./auth.jsx";
import Login from "./Login.jsx";
import Devices from "./Devices.jsx";

function Header() {
  const { token, logout } = useAuth();
  const [health, setHealth] = useState(null);
  useEffect(() => {
    let alive = true;
    const tick = async () => {
      try {
        const res = await fetch("/healthz");
        const j = await res.json();
        if (alive) setHealth(j);
      } catch {
        if (alive) setHealth(null);
      }
    };
    tick();
    const id = setInterval(tick, 10000);
    return () => { alive = false; clearInterval(id); };
  }, []);

  const ok = health ? health.ok : null;
  return (
    <header className="topbar">
      <div className="brand">RMMWay</div>
      <nav className="nav">
        <a className="nav-item active" href="#/devices">Devices</a>
        <a className="nav-item disabled" title="Comes in W2-2 (Cmd-K palette)">
          Search
        </a>
        <a className="nav-item disabled" title="Comes in W2-4 (alerts inbox)">
          Alerts
        </a>
      </nav>
      <div className="topbar-right">
        {health && (
          <span className={"health " + (ok ? "ok" : "bad")} title={JSON.stringify(health.probes)}>
            <span className={"dot " + (ok ? "on" : "off")} />
            {ok ? "all services ok" : "degraded"}
          </span>
        )}
        <button className="btn ghost" onClick={logout} title="Sign out">
          sign out
        </button>
      </div>
      {token && <span className="sr-only" aria-hidden>session active</span>}
    </header>
  );
}

function Shell() {
  const { token, logout } = useAuth();
  if (!token) return <Login />;
  return (
    <div className="shell">
      <Header />
      <main className="content">
        <Devices token={token} onUnauthorized={logout} />
      </main>
    </div>
  );
}

export default function App() {
  return (
    <AuthProvider>
      <Shell />
    </AuthProvider>
  );
}
