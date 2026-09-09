#!/usr/bin/env node
// gap #10b evidence — the mobile pass at 390 / 768 / 1280.
// Covers: dashboard on phone, hamburger nav closed+open, device card list
// (+ one expanded card), full-screen modal (390), the 768 boundary, and the
// 1280 no-regression pair (row nav, centered modal).
//
// Round-1 rig pattern: serve frontend/dist + fixture API, log in with the
// rig's admin account, drive the hash router, screenshot. Playwright
// resolves through the ./node_modules symlink (/tmp/pw/node_modules).
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { chromium } from "playwright";

const here = dirname(fileURLToPath(import.meta.url));
const REPO = join(here, "..", "..");
const DIST = join(REPO, "frontend", "dist");
const PORT = 8255;
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
  const page = await browser.newPage({ viewport: { width: 390, height: 844 } });
  page.on("console", (m) => {
    if (m.type() === "error")
      process.stderr.write(`[console.error] ${m.text()}\n`);
  });
  page.on("pageerror", (e) =>
    process.stderr.write(`[pageerror] ${e.message}\n`),
  );

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

  // ---- 390px -------------------------------------------------------------
  // dashboard on a phone
  await page.goto(`${BASE}/#/dashboard`);
  await page.waitForSelector(".dash .tile");
  await page.waitForTimeout(700);
  await page.screenshot({ path: join(here, "mobile-390-dashboard.png") });

  // hamburger: closed (top bar only, nav hidden)
  await page.goto(`${BASE}/#/devices`);
  await page.waitForSelector(".device-cards .dev-card");
  await page.waitForTimeout(500);
  await page.screenshot({ path: join(here, "mobile-390-nav-closed.png") });

  // hamburger: open (drawer with all nav items)
  await page.click(".nav-burger");
  await page.waitForSelector(".nav.open");
  await page.waitForTimeout(300);
  await page.screenshot({ path: join(here, "mobile-390-nav-open.png") });
  // Esc closes the drawer again (keyboard operability proof)
  await page.keyboard.press("Escape");
  await page.waitForTimeout(200);
  const drawerClosed = await page.locator(".nav.open").count();
  if (drawerClosed !== 0) throw new Error("Esc did not close the nav drawer");

  // device card list (table hidden at 390)
  await page.screenshot({ path: join(here, "mobile-390-devices-cards.png") });

  // one card expanded -> the same DeviceDetail the table row expands to
  await page.click(".device-cards .dev-card");
  await page.waitForSelector(".dev-card-detail");
  await page.waitForTimeout(700); // detail fetches settle
  await page.screenshot({
    path: join(here, "mobile-390-devices-card-open.png"),
    fullPage: true,
  });

  // full-screen modal (Add device)
  await page.click('button[title*="one-time token"]');
  await page.waitForSelector(".modal");
  await page.waitForTimeout(400);
  await page.screenshot({ path: join(here, "mobile-390-modal.png") });
  const modalFull = await page.locator(".modal").evaluate((el) => {
    const r = el.getBoundingClientRect();
    return r.width >= innerWidth - 2 && r.height >= innerHeight - 2;
  });
  if (!modalFull) throw new Error("modal is not full-screen at 390px");
  await page.keyboard.press("Escape");
  await page.waitForTimeout(200);

  // ---- 768px (the boundary: max-width: 768px applies) --------------------
  await page.setViewportSize({ width: 768, height: 1024 });
  await page.goto(`${BASE}/#/devices`);
  await page.waitForSelector(".device-cards .dev-card");
  await page.waitForTimeout(500);
  await page.screenshot({ path: join(here, "tablet-768-devices-cards.png") });
  await page.click(".nav-burger");
  await page.waitForSelector(".nav.open");
  await page.waitForTimeout(300);
  await page.screenshot({ path: join(here, "tablet-768-nav-open.png") });

  // ---- 1280px (no-regression: row nav, table, centered modal) ------------
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto(`${BASE}/#/devices`);
  await page.waitForSelector("table.devices");
  const burgerHidden = await page.locator(".nav-burger").isHidden();
  if (!burgerHidden) throw new Error("hamburger visible at 1280px");
  await page.waitForTimeout(500);
  await page.screenshot({ path: join(here, "desktop-1280-devices.png") });
  await page.click('button[title*="one-time token"]');
  await page.waitForSelector(".modal");
  await page.waitForTimeout(400);
  await page.screenshot({ path: join(here, "desktop-1280-modal.png") });
  const modalCentered = await page.locator(".modal").evaluate((el) => {
    const r = el.getBoundingClientRect();
    return r.height < innerHeight - 2 && r.width < innerWidth - 2;
  });
  if (!modalCentered)
    throw new Error("modal full-screen at 1280px (regression)");

  await browser.close();
  console.log("gap10b shots: 9 files (390: 6, 768: 2, 1280: 2 - see README)");
}

main()
  .catch((e) => {
    console.error("shot failed:", e.message);
    process.exitCode = 1;
  })
  .finally(() => {
    rig.kill("SIGTERM");
  });
