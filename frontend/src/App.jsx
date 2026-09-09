import { Fragment, useEffect, useState, useCallback } from "react";
import { AuthProvider, useAuth } from "./auth.jsx";
import { api } from "./api.js";
import { openEventStream } from "./sse.js";
import Login from "./Login.jsx";
import Setup from "./Setup.jsx";
import Devices from "./Devices.jsx";
import Alerts from "./Alerts.jsx";
import Clients from "./Clients.jsx";
import Flows from "./Flows.jsx";
import Events from "./Events.jsx";
import Dashboard from "./Dashboard.jsx";
import Heal from "./Heal.jsx";
import Webhooks from "./Webhooks.jsx";
import Baseline from "./Baseline.jsx";
import Settings from "./Settings.jsx";
import Palette from "./Palette.jsx";
import { ThemeToggle } from "./ui/theme.jsx";
import { ErrorBoundary } from "./ui/index.js";

// ---- data-driven nav (F3, wave 0) -----------------------------------------
// NAV_ITEMS is the single source of truth for the top-nav. Other lanes add
// items via a 1-line PR to this array only (see DEVELOPER.md "Ownership").
// Shape: { label, path, group, kind, badge?, kbd?, title? }
//   group — section label; a separator renders before the first item of a
//           new group ("" = unlabeled separator)
//   kind  — "route" renders a hash link; "palette" renders the ⌘K action
//   badge — "alerts" renders the open-alert count badge when > 0
const NAV_ITEMS = [
  { label: "Dashboard", path: "dashboard", group: "Fleet", kind: "route" },
  { label: "Devices", path: "devices", group: "Fleet", kind: "route" },
  {
    label: "Alerts",
    path: "alerts",
    group: "Fleet",
    kind: "route",
    badge: "alerts",
  },
  { label: "Clients", path: "clients", group: "Fleet", kind: "route" },
  { label: "Flows", path: "flows", group: "Ops", kind: "route" },
  { label: "Events", path: "events", group: "Ops", kind: "route" },
  { label: "Heal", path: "heal", group: "Ops", kind: "route" },
  { label: "Webhooks", path: "webhooks", group: "System", kind: "route" },
  { label: "Baseline", path: "baseline", group: "System", kind: "route" },
  { label: "Settings", path: "settings", group: "System", kind: "route" },
  {
    label: "Search",
    path: "search",
    group: "",
    kind: "palette",
    kbd: "⌘K",
    title: "Search devices & run actions (Ctrl+K)",
  },
];

// ---- tiny hash router: #/dashboard (default) + the NAV_ITEMS routes.
// ---- Known routes derive from NAV_ITEMS above (one array, one truth).
// The dashboard is the home view (gap #8a), so a bare # / unknown paths
// fall back there; #/devices stays a live deep link for shared table URLs.
function parseRoute() {
  const h = window.location.hash;
  for (const item of NAV_ITEMS) {
    if (item.kind === "route" && h.startsWith("#/" + item.path)) {
      return item.path;
    }
  }
  return "dashboard";
}

// probeTitle renders /healthz probes as a human-readable tooltip string.
// Handles the real server shape (array of {service, ok, latency, detail})
// and object-shaped probes alike.
function probeTitle(probes) {
  if (!probes) return "";
  const entries = Array.isArray(probes)
    ? probes
    : Object.keys(probes).map((k) => [k, probes[k]]);
  return entries
    .map((entry) => {
      // The real server sends an ARRAY OF PROBE OBJECTS
      // ({service, ok, latency, detail}); normalize both that and legacy
      // [name, probe] pairs to (name, probe). Destructuring the object
      // directly as [name, p] threw "(destructured parameter) is not
      // iterable" and blanked the whole app (Header is outside the
      // per-route boundary).
      let name;
      let p;
      if (Array.isArray(entry)) {
        [name, p] = entry;
      } else {
        name = entry && entry.service ? entry.service : "probe";
        p = entry;
      }
      if (typeof p === "string" || typeof p === "number")
        return `${name}: ${p}`;
      if (typeof p === "boolean") return `${name}: ${p ? "ok" : "down"}`;
      if (!p || typeof p !== "object") return String(name);
      const label = p.service || String(name);
      if (p.ok) return p.latency ? `${label} ok (${p.latency})` : `${label} ok`;
      return p.detail ? `${label} down: ${p.detail}` : `${label} down`;
    })
    .join(" · ");
}

function Header({ route, openCount, onOpenPalette }) {
  const { token, logout } = useAuth();
  const [health, setHealth] = useState(null);
  // #10b: the mobile nav drawer (hamburger). Hidden above 768px by CSS;
  // closes on route change, on item click, and on Escape.
  const [navOpen, setNavOpen] = useState(false);
  useEffect(() => {
    setNavOpen(false);
  }, [route]);
  useEffect(() => {
    if (!navOpen) return;
    const onKey = (e) => {
      if (e.key === "Escape") setNavOpen(false);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [navOpen]);
  useEffect(() => {
    let alive = true;
    let id = null;
    const tick = async () => {
      try {
        const res = await fetch("/healthz");
        const j = await res.json();
        if (alive) setHealth(j);
      } catch {
        if (alive) setHealth(null);
      }
    };
    const start = () => {
      if (id === null) id = setInterval(tick, 10000);
    };
    const stop = () => {
      if (id !== null) {
        clearInterval(id);
        id = null;
      }
    };
    // Phase 4: a hidden tab has no operator to serve — pause the health
    // poll while document.hidden, refresh immediately when it returns.
    const onVisibility = () => {
      if (document.hidden) stop();
      else {
        tick();
        start();
      }
    };
    tick();
    start();
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      stop();
      document.removeEventListener("visibilitychange", onVisibility);
      alive = false;
    };
  }, []);

  const ok = health ? health.ok : null;
  return (
    <header className="topbar">
      <div className="brand">
        RMMWay
        {health && health.version && (
          <span className="brand-version" title={`RMMWay ${health.version}`}>
            {health.version}
          </span>
        )}
      </div>
      <button
        className="nav-burger"
        aria-label={navOpen ? "Close menu" : "Open menu"}
        aria-expanded={navOpen}
        aria-controls="mainnav"
        onClick={() => setNavOpen((o) => !o)}
        title={navOpen ? "Close menu (Esc)" : "Menu"}
      >
        <span aria-hidden="true">{navOpen ? "✕" : "☰"}</span>
      </button>
      <nav id="mainnav" className={"nav" + (navOpen ? " open" : "")}>
        {NAV_ITEMS.map((item, i) => {
          const prev = i > 0 ? NAV_ITEMS[i - 1] : null;
          return (
            <Fragment key={item.path}>
              {(!prev || prev.group !== item.group) && (
                <span
                  className={"nav-sep" + (i === 0 ? " first" : "")}
                  aria-hidden="true"
                >
                  {item.group}
                </span>
              )}
              {item.kind === "palette" ? (
                <a
                  className="nav-item"
                  href={"#/" + item.path}
                  role="button"
                  onClick={(e) => {
                    e.preventDefault();
                    setNavOpen(false);
                    onOpenPalette();
                  }}
                  title={item.title}
                >
                  {item.label} <kbd className="kbd">{item.kbd}</kbd>
                </a>
              ) : (
                <a
                  className={
                    "nav-item" + (route === item.path ? " active" : "")
                  }
                  href={"#/" + item.path}
                  aria-current={route === item.path ? "page" : undefined}
                  onClick={() => setNavOpen(false)}
                >
                  {item.label}
                  {item.badge === "alerts" && openCount > 0 && (
                    <span className="badge">{openCount}</span>
                  )}
                </a>
              )}
            </Fragment>
          );
        })}
      </nav>
      <div className="topbar-right">
        {health && (
          <span
            className={"health " + (ok ? "ok" : "bad")}
            title={probeTitle(health.probes)}
          >
            <span className={"dot " + (ok ? "on" : "off")} />
            <span className="health-label">
              {ok ? "all services ok" : "degraded"}
            </span>
          </span>
        )}
        <ThemeToggle />
        <button className="btn ghost" onClick={logout} title="Sign out">
          sign out
        </button>
      </div>
      {token && (
        <span className="sr-only" aria-hidden>
          session active
        </span>
      )}
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
    return () => {
      alive = false;
    };
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
    if (!token) {
      setOpenCount(0);
      return;
    }
    refreshAlerts();
    const id = setInterval(refreshAlerts, 15000);
    return () => clearInterval(id);
  }, [token, refreshAlerts]);

  // B-1: the live event stream. A device online/offline flip re-pulls the
  // device list (the status badge updates without waiting for the 5s poll);
  // a command result (D-1) re-pulls the device view so the open device's
  // Commands panel refreshes at once; an alert event re-pulls the open count
  // (the nav badge updates at once) AND bumps the alerts inbox (a new alert
  // appears in the open list immediately, not on the next 10s poll).
  // The stream is best-effort — it auto-reconnects and resumes from
  // Last-Event-ID; when the framework is unwired (in-memory server) it 401s
  // / 503s and the polls above remain the fallback.
  const [deviceTick, setDeviceTick] = useState(0);
  const [alertTick, setAlertTick] = useState(0);
  // D-2: the latest journaled envelope off the live stream — the Events page
  // prepends it straight into the journal view (no re-fetch).
  const [lastEvent, setLastEvent] = useState(null);
  useEffect(() => {
    if (!token) return;
    const close = openEventStream({
      token,
      onEvent: (env) => {
        if (!env || !env.category) return;
        setLastEvent(env);
        // Command results are journaled under the "automation" category
        // (rmmway.events.command.result); "command" is kept for streams that
        // still label them that way.
        if (
          env.category === "inventory" ||
          env.category === "command" ||
          env.category === "automation"
        ) {
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
    return (
      <Setup onDone={() => setSetupState((s) => ({ ...s, setup: true }))} />
    );
  }
  if (!token) return <Login />;
  return (
    <div className="shell">
      <Header route={route} openCount={openCount} onOpenPalette={openPalette} />
      <main className="content">
        {/* One boundary per route: a caught error panels that view but
            never blocks navigating to the rest of the app. */}
        <ErrorBoundary key={route}>
          {route === "dashboard" ? (
            <Dashboard token={token} onUnauthorized={logout} />
          ) : route === "alerts" ? (
            <Alerts
              token={token}
              onUnauthorized={logout}
              liveTick={alertTick}
            />
          ) : route === "clients" ? (
            <Clients token={token} onUnauthorized={logout} />
          ) : route === "flows" ? (
            <Flows token={token} onUnauthorized={logout} />
          ) : route === "events" ? (
            <Events
              token={token}
              onUnauthorized={logout}
              onGoToDevice={goToDevice}
              lastEvent={lastEvent}
            />
          ) : route === "heal" ? (
            <Heal
              token={token}
              onUnauthorized={logout}
              onGoToDevice={goToDevice}
            />
          ) : route === "webhooks" ? (
            <Webhooks
              token={token}
              onUnauthorized={logout}
              onGoToDevice={goToDevice}
            />
          ) : route === "baseline" ? (
            <Baseline
              token={token}
              onUnauthorized={logout}
              onGoToDevice={goToDevice}
            />
          ) : route === "settings" ? (
            <Settings token={token} onUnauthorized={logout} />
          ) : (
            <Devices
              token={token}
              onUnauthorized={logout}
              focusFilter={focusFilter}
              focusKey={focusKey}
              liveTick={deviceTick}
            />
          )}
        </ErrorBoundary>
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
    <ErrorBoundary>
      <AuthProvider>
        <Shell />
      </AuthProvider>
    </ErrorBoundary>
  );
}
