// A-2 frontend smoke test: drives the REAL <App/> in jsdom through the
// first-boot flow.
//
//   1. fresh database (fetch /api/setup/status -> {available, setup:false})
//      -> the UI shows the setup wizard (NOT the login screen);
//   2. filling the wizard (admin + org + smtp) and submitting completes
//      setup (POST /api/setup/complete) and auto-signs-in
//      (POST /api/login with the minted credentials);
//   3. the app lands on the main shell (wizard gone, devices view in).
//
// Run: node scripts/setup-wizard.smoke.mjs   (bundles the JSX with esbuild)
import { JSDOM } from "jsdom";

const dom = new JSDOM("<!doctype html><html><body><div id=root></div></body></html>", {
  url: "http://localhost/",
  pretendToBeVisual: true,
});
globalThis.window = dom.window;
globalThis.document = dom.window.document;
Object.defineProperty(globalThis, "navigator", { value: dom.window.navigator, configurable: true });
globalThis.localStorage = dom.window.localStorage;
globalThis.HTMLElement = dom.window.HTMLElement;
globalThis.Event = dom.window.Event;
globalThis.getComputedStyle = dom.window.getComputedStyle;
globalThis.requestAnimationFrame = (cb) => setTimeout(cb, 0);
globalThis.cancelAnimationFrame = (id) => clearTimeout(id);
globalThis.IS_REACT_ACT_ENVIRONMENT = true;

// ---- the fake backend -------------------------------------------------------
const calls = [];
const loginCalls = [];
async function fakeFetch(path, init = {}) {
  const method = init.method || "GET";
  calls.push(`${method} ${path}`);
  const json = (obj, status = 200) =>
    new Response(JSON.stringify(obj), { status, headers: { "Content-Type": "application/json" } });
  if (path === "/api/setup/status") {
    return json({ available: true, setup: SETUP_STATE.setup });
  }
  if (path === "/api/setup/complete") {
    const body = JSON.parse(init.body || "{}");
    if (!body.admin_user || body.admin_password.length < 8 || !body.org_name) {
      return json({ error: "validation failed" }, 400);
    }
    SETUP_STATE.setup = true;
    return json({ available: true, setup: true, org_name: body.org_name, admin_user: body.admin_user });
  }
  if (path === "/api/login") {
    const body = JSON.parse(init.body || "{}");
    loginCalls.push(body);
    if (body.username === "wizadmin" && body.password === "wizpass-123") {
      return json({ token: "wiz-token", expiry: "2030-01-01T00:00:00Z", capabilities: [] });
    }
    return json({ error: "invalid username or password" }, 401);
  }
  if (path === "/api/devices") return json([]);
  if (path === "/api/alerts/counts") return json({ open: 0, acked: 0, resolved: 0 });
  return json({});
}
globalThis.fetch = fakeFetch;

const SETUP_STATE = { setup: false };

// ---- render the real App ----------------------------------------------------
const React = (await import("react")).default;
const { createRoot } = await import("react-dom/client");
const { act } = await import("react");
const App = (await import("../src/App.jsx")).default;

const container = document.getElementById("root");
const root = createRoot(container);
await act(async () => {
  root.render(React.createElement(App));
});

const text = () => container.textContent;
function waitUntil(cond, what, ms = 3000) {
  return new Promise((resolve, reject) => {
    const t0 = Date.now();
    (function poll() {
      if (cond()) return resolve();
      if (Date.now() - t0 > ms) return reject(new Error(`timeout waiting for: ${what}`));
      setTimeout(poll, 25);
    })();
  });
}

function setVal(el, v) {
  const proto = window.HTMLInputElement.prototype;
  Object.getOwnPropertyDescriptor(proto, "value").set.call(el, v);
  el.dispatchEvent(new window.Event("input", { bubbles: true }));
}
const input = (sel) => container.querySelector(sel);
const inputs = () => [...container.querySelectorAll("input")];

// ---- 1. fresh DB -> the WIZARD is shown, not the login -----------------------
await waitUntil(() => text().includes("First boot — initialize this server"), "wizard to render");
if (!text().includes("Root admin") || !text().includes("Organization & CA") || !text().includes("SMTP outbox")) {
  throw new Error("wizard sections missing: " + text().slice(0, 400));
}
if (text().includes("Sign in to continue")) {
  throw new Error("login screen shown instead of the wizard");
}
console.log("ok 1: fresh database -> UI shows the setup wizard (login hidden)");

// ---- 2. fill the wizard + submit -> completes + auto-login -------------------
// input order: username, password, confirm, org, [smtp off by default]
let els = inputs();
setVal(els[0], "wizadmin");
setVal(els[1], "wizpass-123");
setVal(els[2], "wizpass-123");
setVal(els[3], "Acme Corp");
await act(async () => {
  // jsdom does not translate a submit-button click into a form submit event
  // (browsers do), so fire submit directly — what the browser would dispatch.
  container.querySelector("form").dispatchEvent(new window.Event("submit", { bubbles: true, cancelable: true }));
});
await waitUntil(() => calls.some((c) => c === "POST /api/setup/complete"), "completeSetup call");
await waitUntil(() => loginCalls.length === 1, "auto-login call");
if (loginCalls[0].username !== "wizadmin" || loginCalls[0].password !== "wizpass-123") {
  throw new Error("auto-login used wrong credentials: " + JSON.stringify(loginCalls[0]));
}
await waitUntil(() => !text().includes("First boot — initialize this server") && text().includes("Devices"), "main shell after wizard");
console.log("ok 2: wizard submit -> POST /api/setup/complete + auto-login with minted creds -> main shell");

// ---- 3. the wizard never comes back once setup is done ------------------------
// A brand-new App mount (a page reload) must go straight to the login screen
// (token was cleared? no — the wizard auto-signed-in, so straight to shell).
const container2 = document.createElement("div");
document.body.appendChild(container2);
const root2 = createRoot(container2);
await act(async () => {
  root2.render(React.createElement(App));
});
await waitUntil(() => container2.textContent.includes("Devices"), "reloaded app lands on shell (token in localStorage)");
if (container2.textContent.includes("First boot")) {
  throw new Error("wizard re-appeared after setup completed");
}
console.log("ok 3: subsequent boots skip the wizard (setup state persisted server-side)");

console.log("\nA-2 frontend smoke PASS: wizard on fresh DB, auto-login, wizard gone after.");
process.exit(0);
