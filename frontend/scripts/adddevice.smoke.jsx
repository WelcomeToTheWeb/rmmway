// Add-device frontend smoke test: drives the REAL <App/> in jsdom through the
// "Add a device" flow.
//
//   1. sign in (fresh, already-set-up server -> the login screen);
//   2. the Devices view's "Add a device" action is present;
//   3. clicking it opens the modal, which mints a one-time token
//      (POST /api/bootstrap) and renders a copy-paste install command per OS,
//      with the server URL pre-filled from the current origin and the minted
//      token + pre-allocated device id embedded.
//
// Run: node scripts/adddevice.smoke.mjs   (bundles the JSX with esbuild)
import { JSDOM } from "jsdom";

const dom = new JSDOM("<!doctype html><html><body><div id=root></div></body></html>", {
  url: "http://rmm.example.com/",
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

const ORIGIN = "http://rmm.example.com";
const PUBLIC_URL = "https://rmm.public.example.com";
const TOKEN = "bt-smoketoken12345";
const DEVICE_ID = "dev-smokedevice01";

// ---- the fake backend -------------------------------------------------------
const calls = [];
async function fakeFetch(path, init = {}) {
  const method = init.method || "GET";
  calls.push(`${method} ${path}`);
  const json = (obj, status = 200) =>
    new Response(JSON.stringify(obj), { status, headers: { "Content-Type": "application/json" } });
  if (path === "/api/setup/status") return json({ available: true, setup: true });
  if (path === "/api/login") {
    const body = JSON.parse(init.body || "{}");
    if (body.username === "admin" && body.password === "smoke-pass") {
      return json({ token: "smoke-operator-token", expiry: "2030-01-01T00:00:00Z", capabilities: [] });
    }
    return json({ error: "invalid username or password" }, 401);
  }
  if (path === "/api/devices") {
    return json([
      {
        id: "dev-existing", hostname: "fileserver-01", os: "linux", arch: "amd64",
        agent_version: "0.1.0", interfaces: ["10.0.0.9"], tags: [],
        online: true, first_seen: "2026-01-01T00:00:00Z", last_seen: "2026-01-01T00:00:00Z",
      },
    ]);
  }
  if (path === "/api/public-url") return json({ url: PUBLIC_URL });
  if (path === "/api/alerts/counts") return json({ open: 0, acked: 0, resolved: 0 });
  if (path === "/api/bootstrap") {
    // The operator "Add a device" mint (auth-gated).
    if ((init.headers || {})["Authorization"] !== "Bearer smoke-operator-token") {
      return json({ error: "unauthorized" }, 401);
    }
    return json({ bootstrap_token: TOKEN, device_id: DEVICE_ID });
  }
  return json({});
}
globalThis.fetch = fakeFetch;

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
function waitUntil(cond, what, ms = 4000) {
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

// ---- 1. already-set-up server -> the login screen ----------------------------
await waitUntil(() => text().includes("Sign in to continue"), "login screen");
console.log("ok 1: a set-up server shows the login screen (no wizard)");

// ---- 2. sign in -> the Devices view ------------------------------------------
await act(async () => {
  setVal(container.querySelector('input[type="password"]'), "smoke-pass");
  container.querySelector("form").dispatchEvent(new window.Event("submit", { bubbles: true, cancelable: true }));
});
await waitUntil(() => text().includes("Devices") && text().includes("fileserver-01"), "devices view after login");
console.log("ok 2: sign-in lands on the Devices view (existing device listed)");

// ---- 3. the "Add a device" action is present ---------------------------------
const addBtn = [...container.querySelectorAll("button")].find((b) => /add device/i.test(b.textContent));
if (!addBtn) throw new Error("no 'Add a device' button found in the Devices view");
console.log("ok 3: the 'Add a device' action is present in the Devices view");

// ---- 4. click it -> modal opens + mints a token (POST /api/bootstrap) --------
await act(async () => {
  addBtn.dispatchEvent(new window.MouseEvent("click", { bubbles: true }));
});
await waitUntil(() => calls.includes("POST /api/bootstrap"), "bootstrap mint call");
await waitUntil(() => text().includes("Add a device") && text().includes(DEVICE_ID), "modal with device id");
console.log("ok 4: clicking it mints a token (POST /api/bootstrap) and opens the modal");

// ---- 5. the modal shows the copy-paste commands (origin + token + device id) -
const modalText = text();
for (const needle of [DEVICE_ID, TOKEN]) {
  if (!modalText.includes(needle)) {
    throw new Error(`modal is missing ${needle}: ${modalText.slice(0, 400)}`);
  }
}
// Linux command: curl|bash with the PUBLIC URL + the token embedded.
const linuxTail = `bash -s -- --server ${PUBLIC_URL} --bootstrap ${TOKEN}`;
if (!modalText.includes(linuxTail) || !modalText.includes("install.sh | bash")) {
  throw new Error("Linux install command missing or wrong (public URL/token not embedded): " + modalText.slice(0, 600));
}
// Windows command: iwr|powershell with the PUBLIC URL + the token embedded.
const winTail = `-File install.ps1 -Server ${PUBLIC_URL} -Bootstrap ${TOKEN}`;
if (!modalText.includes(winTail) || !modalText.includes("-ExecutionPolicy Bypass")) {
  throw new Error("Windows install command missing or wrong (public URL/token not embedded): " + modalText.slice(0, 600));
}
console.log("ok 5: the modal shows ready-to-paste Linux + Windows commands with the origin + token + device id");

// The server URL is pre-filled from GET /api/public-url (the configured public
// URL) — NOT from window.location.origin. This matters when the UI is behind
// a reverse proxy (e.g. localhost:8080) but the agents need to dial the real
// public hostname.
if (!calls.includes("GET /api/public-url")) {
  throw new Error("expected the Add Device modal to call GET /api/public-url");
}
const serverInput = container.querySelector(".adddev-server");
if (!serverInput || serverInput.value !== PUBLIC_URL) {
  throw new Error(
    `expected Server URL prefilled with ${PUBLIC_URL} (from /api/public-url), got ${serverInput && serverInput.value}`
  );
}
console.log("ok 6: the server URL is pre-filled from GET /api/public-url (configured public URL)");

console.log("\nAdd-device frontend smoke PASS: login -> Devices -> Add a device -> mint + copy-paste commands.");
process.exit(0);
