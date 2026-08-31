import { useEffect, useState, useCallback } from "react";
import { AuthProvider, useAuth } from "./auth.jsx";
import { api } from "./api.js";
import { openEventStream } from "./sse.js";
import Login from "./Login.jsx";
import Setup from "./Setup.jsx";
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
  // A-2: first-boot state. null = still checking the server; when the DB is
  // fresh (available && !setup) the UI is the setup wizard, full stop.
  const [setupState, setSetupState] = useState(null);
  useEffect(() => {
    let alive = true;
    api
      .setupStatus()
      .then((s) => alive && setSetupState(s))
      .catch(() => alive && setSetupState({ available: false, setup: true }));
    return () => { alive = false; };
  }, []);

  // Route follows the location hash (back/forward + nav links).
  useEffect(() => {
    const onHash = () => setRoute(parseRoute());
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

  // The nav badge: the open-alert count. Refreshed on a timer AND the moment
  // an alert-category event lands on the live stream (below), so a new alert
  // bumps the badge instantly instead of on the next 15s poll.
  const refreshAlerts = useCallback(async () => {
    try {
      const c = await api.alertCounts(token);
      setOpenCount(c && c.open ? c.open : 0);
    } catch {
      /* keep the last known count */
    }
  }, [token]);
  useEffect(() => {
    if (!token) { setOpenCount(0); return; }
    refreshAlerts();
    const id = setInterval(refreshAlerts, 15000);
    return () => clearInterval(id);
  }, [token, refreshAlerts]);

  // B-1: the live event stream. A device online/offline flip re-pulls the
  // device list (the status badge updates without waiting for the 5s poll);
  // an alert event re-pulls the open count (the nav badge updates at once)
  // AND bumps the alerts inbox (a new alert appears in the open list
  // immediately, not on the next 10s poll).
  // The stream is best-effort — it auto-reconnects and resumes from
  // Last-Event-ID; when the framework is unwired (in-memory server) it 401s
  // / 503s and the polls above remain the fallback.
  const [deviceTick, setDeviceTick] = useState(0);
  const [alertTick, setAlertTick] = useState(0);
  useEffect(() => {
    if (!token) return;
    const close = openEventStream({
      token,
      onEvent: (env) => {
        if (!env || !env.category) return;
        if (env.category === "inventory") {
          setDeviceTick((t) => t + 1);
        } else if (env.category === "alert") {
          refreshAlerts();
          setAlertTick((t) => t + 1);
        }
      },
    });
    return close;
  }, [token, refreshAlerts]);

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
  // B-2: the palette's `tag:<name>` group row jumps to the device list
  // filtered to that exact tag.
  const goToTag = useCallback((tag) => {
    window.location.hash = "#/devices";
    setFocusFilter("tag:" + tag);
    setFocusKey((k) => k + 1);
  }, []);

  if (setupState === null) {
    return (
      <div className="login-wrap">
        <div className="card muted">Checking server state…</div>
      </div>
    );
  }
  if (setupState.available && !setupState.setup) {
    return <Setup onDone={() => setSetupState((s) => ({ ...s, setup: true }))} />;
  }
  if (!token) return <Login />;
  return (
    <div className="shell">
      <Header route={route} openCount={openCount} onOpenPalette={openPalette} />
      <main className="content">
        {route === "alerts" ? (
          <Alerts token={token} onUnauthorized={logout} liveTick={alertTick} />
        ) : route === "flows" ? (
          <Flows token={token} onUnauthorized={logout} />
        ) : (
          <Devices
            token={token}
            onUnauthorized={logout}
            focusFilter={focusFilter}
            focusKey={focusKey}
            liveTick={deviceTick}
          />
        )}
      </main>
      <Palette
        open={paletteOpen}
        onClose={() => setPaletteOpen(false)}
        onGoToDevice={goToDevice}
        onGoToAll={goToAll}
        onGoToTag={goToTag}
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
