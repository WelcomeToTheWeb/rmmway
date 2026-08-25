import { useEffect, useState, useCallback } from "react";
import { AuthProvider, useAuth } from "./auth.jsx";
import { api } from "./api.js";
import Login from "./Login.jsx";
import Devices from "./Devices.jsx";
import Alerts from "./Alerts.jsx";
import Flows from "./Flows.jsx";
import Palette from "./Palette.jsx";

// ---- tiny hash router: #/devices (default), #/alerts, #/flows --------------
function parseRoute() {
  const h = window.location.hash;
  if (h.startsWith("#/alerts")) return "alerts";
  if (h.startsWith("#/flows")) return "flows";
  return "devices";
}

function Header({ route, openCount, onOpenPalette }) {
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
        <a className={"nav-item" + (route === "devices" ? " active" : "")} href="#/devices">
          Devices
        </a>
        <a
          className={"nav-item" + (route === "alerts" ? " active" : "")}
          href="#/alerts"
        >
          Alerts
          {openCount > 0 && <span className="badge">{openCount}</span>}
        </a>
        <a
          className={"nav-item" + (route === "flows" ? " active" : "")}
          href="#/flows"
        >
          Flows
        </a>
        <a
          className="nav-item"
          href="#/search"
          role="button"
          onClick={(e) => { e.preventDefault(); onOpenPalette(); }}
          title="Search devices & run actions (Ctrl+K)"
        >
          Search <kbd className="kbd">⌘K</kbd>
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
  const [route, setRoute] = useState(parseRoute);
  const [openCount, setOpenCount] = useState(0);

  // Route follows the location hash (back/forward + nav links).
  useEffect(() => {
    const onHash = () => setRoute(parseRoute());
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

  // The nav badge: poll the open-alert count while signed in.
  useEffect(() => {
    if (!token) { setOpenCount(0); return; }
    let alive = true;
    const tick = async () => {
      try {
        const c = await api.alertCounts(token);
        if (alive) setOpenCount(c && c.open ? c.open : 0);
      } catch {
        /* keep the last known count */
      }
    };
    tick();
    const id = setInterval(tick, 15000);
    return () => { alive = false; clearInterval(id); };
  }, [token]);

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
    window.location.hash = "#/devices";
    setFocusFilter(hostname || id);
    setFocusKey((k) => k + 1);
  }, []);
  const goToAll = useCallback(() => {
    window.location.hash = "#/devices";
    setFocusFilter("");
    setFocusKey((k) => k + 1);
  }, []);

  if (!token) return <Login />;
  return (
    <div className="shell">
      <Header route={route} openCount={openCount} onOpenPalette={openPalette} />
      <main className="content">
        {route === "alerts" ? (
          <Alerts token={token} onUnauthorized={logout} />
        ) : route === "flows" ? (
          <Flows token={token} onUnauthorized={logout} />
        ) : (
          <Devices
            token={token}
            onUnauthorized={logout}
            focusFilter={focusFilter}
            focusKey={focusKey}
          />
        )}
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
