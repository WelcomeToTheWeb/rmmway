import { useEffect, useState, useCallback } from "react";
import { AuthProvider, useAuth } from "./auth.jsx";
import Login from "./Login.jsx";
import Devices from "./Devices.jsx";
import Palette from "./Palette.jsx";

function Header({ onOpenPalette }) {
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
        <a
          className="nav-item"
          href="#/search"
          role="button"
          onClick={(e) => { e.preventDefault(); onOpenPalette(); }}
          title="Search devices & run actions (Ctrl+K)"
        >
          Search <kbd className="kbd">⌘K</kbd>
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
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [focusKey, setFocusKey] = useState(0);
  const [focusFilter, setFocusFilter] = useState(null);

  // Global ⌘K / Ctrl+K opens the palette (only when signed in).
  const openPalette = useCallback(() => setPaletteOpen(true), []);
  useEffect(() => {
    if (!token) return;
    const onKey = (e) => {
      if ((e.metaKey || e.ctrlKey) && (e.key === "k" || e.key === "K")) {
        e.preventDefault();
        setPaletteOpen(true);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [token]);

  const goToDevice = useCallback((id, hostname) => {
    setFocusFilter(hostname || id);
    setFocusKey((k) => k + 1);
  }, []);
  const goToAll = useCallback(() => {
    setFocusFilter("");
    setFocusKey((k) => k + 1);
  }, []);

  if (!token) return <Login />;
  return (
    <div className="shell">
      <Header onOpenPalette={openPalette} />
      <main className="content">
        <Devices
          token={token}
          onUnauthorized={logout}
          focusFilter={focusFilter}
          focusKey={focusKey}
        />
      </main>
      <Palette
        open={paletteOpen}
        onClose={() => setPaletteOpen(false)}
        onGoToDevice={goToDevice}
        onGoToAll={goToAll}
      />
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