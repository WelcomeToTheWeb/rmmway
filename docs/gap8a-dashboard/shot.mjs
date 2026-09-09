#!/usr/bin/env node
// gap #8a evidence — fleet dashboard home at 1280px and 390px.
// RMMWay round-1 rig pattern: serve frontend/dist + a fixture API, log in
// with the rig's admin account, drive the hash router, screenshot.
//
// Playwright is resolved from the sibling symlink ./node_modules (->
// /tmp/pw/node_modules, browsers in ~/.cache/ms-playwright) — the rig is
// evidence-only, it is not a dependency of the app.
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { chromium } from "playwright";

const here = dirname(fileURLToPath(import.meta.url));
const REPO = join(here, "..", "..");
const DIST = join(REPO, "frontend", "dist");
const PORT = 8245;
const BASE = `http://127.0.0.1:${PORT}`;

const rig = spawn(
  process.execPath,
  [join(REPO, "docs", "ui-revamp", "rig", "serve.js")],
  {
    cwd: REPO,
    env: { ...process.env, DIST, PORT: String(PORT) },
    stdio: ["ignore", "pipe", "pipe"],
  },
);
rig.stdout.on("data", (d) => process.stderr.write(`[rig] ${d}`));
rig.stderr.on("data", (d) => process.stderr.write(`[rig] ${d}`));

async function waitUp(url, ms = 8000) {
  const t0 = Date.now();
  while (Date.now() - t0 < ms) {
    try {
      const res = await fetch(url);
      if (res.ok) return;
    } catch {}
    await new Promise((r) => setTimeout(r, 120));
  }
  throw new Error(`rig did not come up at ${url}`);
}

async function main() {
  await waitUp(`${BASE}/healthz`);
  const browser = await chromium.launch();
  const page = await browser.newPage({
    viewport: { width: 1280, height: 800 },
  });
  page.on("console", (m) => {
    if (m.type() === "error")
      process.stderr.write(`[console.error] ${m.text()}\n`);
  });

  // rig login: any password for "admin" mints the static evidence token
  await page.goto(`${BASE}/#/login`);
  await page.waitForSelector("form");
  await page.fill('input[autocomplete="username"]', "admin");
  await page.fill('input[type="password"]', "evidence-pass");
  await Promise.all([
    page.waitForLoadState("networkidle"),
    page.click('button[type="submit"]'),
  ]);
  await page.waitForFunction(() =>
    window.localStorage.getItem("rmmway.operator.token"),
  );

  // 1) 1280px: default-route proof — a bare #/ must land on the dashboard
  await page.goto(`${BASE}/#/`);
  await page.waitForSelector(".dash .tile");
  await page.waitForTimeout(900); // tiles settle (fixtures resolve fast)
  await page.screenshot({ path: join(here, "dashboard-1280.png") });

  // 2) explicit #/dashboard deep link (same screen, proves the route)
  await page.goto(`${BASE}/#/dashboard`);
  await page.waitForSelector(".dash .tile");
  await page.waitForTimeout(400);

  // 3) #/devices still works (shared deep-link promise)
  await page.goto(`${BASE}/#/devices`);
  await page.waitForSelector("table.devices");
  await page.waitForTimeout(400);

  // 4) 390px: the same home on a phone
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`${BASE}/#/dashboard`);
  await page.waitForSelector(".dash .tile");
  await page.waitForTimeout(700);
  await page.screenshot({
    path: join(here, "dashboard-390.png"),
    fullPage: true,
  });

  // 5) 390px: alert + anomaly tiles' compact row layout (scroll to them)
  await page
    .locator(".dash .tile", { hasText: "Top anomalies" })
    .scrollIntoViewIfNeeded();
  await page.waitForTimeout(300);
  await page.screenshot({ path: join(here, "dashboard-390-tiles.png") });

  await browser.close();
  console.log(
    "gap8a shots: dashboard-1280.png dashboard-390.png dashboard-390-tiles.png",
  );
}

main()
  .catch((e) => {
    console.error("shot failed:", e.message);
    process.exitCode = 1;
  })
  .finally(() => {
    rig.kill("SIGTERM");
  });
