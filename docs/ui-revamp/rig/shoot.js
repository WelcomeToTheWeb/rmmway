// UI evidence screenshots: login, expand first device, capture topbar +
// device detail (chart), in dark and light themes.
// Usage: LD_LIBRARY_PATH=... node shoot.js <url> <outdir> <tag>
import { chromium } from "playwright";
import { mkdirSync } from "node:fs";

const [url, outdir, tag] = process.argv.slice(2);
mkdirSync(outdir, { recursive: true });

const browser = await chromium.launch();

async function session(theme) {
  const ctx = await browser.newContext({
    viewport: { width: 1440, height: 960 },
    deviceScaleFactor: 2,
  });
  await ctx.addInitScript((t) => {
    if (t) localStorage.setItem("rmmway-theme", t);
  }, theme);
  const page = await ctx.newPage();
  await page.goto(url, { waitUntil: "networkidle" });

  // sign in
  await page.fill('input[type="password"]', "smokepass");
  await page.press('input[type="password"]', "Enter");
  await page.waitForSelector("table.devices tbody tr:not(.detail-row)", {
    timeout: 15000,
  });

  // expand the first device row
  const row = page.locator("table.devices tbody tr:not(.detail-row)").first();
  await row.click();
  await page.waitForSelector("tr.detail-row", { timeout: 15000 });
  await page.waitForSelector(".metrics-chart polyline", { timeout: 15000 });
  await page.waitForTimeout(1600); // let stats/live labels settle

  // topbar (full width)
  await page
    .locator("header.topbar")
    .screenshot({ path: `${outdir}/${tag}-topbar-${theme}.png` });

  // device detail area (the chart surface + panels below)
  const detail = page.locator("tr.detail-row");
  await detail.screenshot({ path: `${outdir}/${tag}-detail-${theme}.png` });

  // full page for context (nav grouping / shell)
  await page.screenshot({
    path: `${outdir}/${tag}-page-${theme}.png`,
    fullPage: true,
  });
  await ctx.close();
}

await session("dark");
await session("light");
await browser.close();
console.log(`done: ${outdir}/${tag}-*`);
