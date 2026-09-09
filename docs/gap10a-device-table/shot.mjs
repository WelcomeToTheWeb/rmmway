// Screenshots for gap #10a (wave 2, lane C): device-table ergonomics.
//
// Usage:
//   node shot.mjs            (builds nothing; expects a fresh `npm run build`)
//
// Spawns the shared fake-API rig (docs/ui-revamp/rig/serve.js, extended in
// this wave with /api/clients + ?client= scoping) against the local
// frontend/dist, logs in with the rig's static token, and captures:
//
//   desktop-1280-default.png        1280px, default view
//   desktop-1280-stateful.png       1280px, URL state (sort=host:desc +
//                                   hidden=tags), multi-IP row expanded,
//                                   the columns menu open
//   desktop-1280-client-scoped.png  1280px, client filter -> Acme Corp
//   mobile-390-default.png          390px, default view (narrow layout)
//
// All output lands in this folder only — no existing evidence file is
// touched.
import { spawn } from "node:child_process";
import { setTimeout as sleep } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { chromium } from "playwright";

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = join(HERE, "..", "..");
const RIG = join(REPO, "docs", "ui-revamp", "rig", "serve.js");
const DIST = join(REPO, "frontend", "dist");
const PORT = Number(process.env.PORT || 8231);
const BASE = `http://127.0.0.1:${PORT}`;
const TOKEN_KEY = "rmmway.operator.token";
const TOKEN = "evidence-token"; // the rig's /api/login token

const rig = spawn(process.execPath, [RIG], {
  env: { ...process.env, DIST, PORT: String(PORT) },
  stdio: ["ignore", "inherit", "inherit"],
});

async function main() {
  await sleep(500); // rig listen
  const browser = await chromium.launch();

  const mkCtx = (width, height) =>
    browser
      .newContext({
        viewport: { width, height },
        deviceScaleFactor: 1,
      })
      .then(async (ctx) => {
        await ctx.addInitScript(
          ([k, v]) => localStorage.setItem(k, v),
          [TOKEN_KEY, TOKEN],
        );
        return ctx;
      });

  const shot = (page, name) =>
    page
      .screenshot({ path: join(HERE, name) })
      .then(() => console.log("captured", name));

  // ---- desktop 1280 ------------------------------------------------------
  const dctx = await mkCtx(1280, 800);
  const dpage = await dctx.newPage();
  await dpage.goto(`${BASE}/#/devices`, { waitUntil: "networkidle" });
  await dpage.waitForSelector("table.devices tbody tr");
  await shot(dpage, "desktop-1280-default.png");

  // stateful: URL-driven sort + hidden column, multi-IP row expanded,
  // columns menu open (each is a URL state this lane added)
  await dpage.evaluate(
    () => (window.location.hash = "#/devices?sort=host:desc&hidden=tags"),
  );
  await dpage.waitForSelector(".th-btn.active .sort-ind");
  await dpage
    .locator("button.ip-primary", { hasText: "10.0.9.3" })
    .first()
    .click();
  await dpage.waitForSelector(".ip-list");
  await dpage.locator(".menu-toggle").click();
  await dpage.waitForSelector(".menu-panel");
  await sleep(150); // let the panel settle
  await shot(dpage, "desktop-1280-stateful.png");

  // client-scoped: pick Acme Corp in the toolbar filter (server-side
  // ?client= via the rig)
  await dpage.keyboard.press("Escape"); // close the menu
  await dpage.evaluate(() => (window.location.hash = "#/devices"));
  await dpage.waitForTimeout(200);
  await dpage.locator("select.client-filter").selectOption("clt-acme");
  await dpage.waitForFunction(() =>
    (document.querySelector(".view-head p.muted")?.textContent || "").includes(
      "2 in Acme Corp",
    ),
  );
  await sleep(250); // let the scoped render settle
  await shot(dpage, "desktop-1280-client-scoped.png");
  await dctx.close();

  // ---- mobile 390 --------------------------------------------------------
  const mctx = await mkCtx(390, 844);
  const mpage = await mctx.newPage();
  await mpage.goto(`${BASE}/#/devices`, { waitUntil: "networkidle" });
  await mpage.waitForSelector("table.devices tbody tr");
  await shot(mpage, "mobile-390-default.png");
  await mctx.close();

  await browser.close();
}

main()
  .catch((e) => {
    console.error(e);
    process.exitCode = 1;
  })
  .finally(() => rig.kill());
