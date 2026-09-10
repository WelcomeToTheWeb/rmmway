import { chromium } from "playwright";

const browser = await chromium.launch({
  executablePath:
    "/home/webadmin/.cache/ms-playwright/chromium-1243/chrome-linux64/chrome",
  args: ["--no-sandbox"],
});

const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });

// Load and login
await page.goto("http://localhost:5173/", {
  waitUntil: "networkidle",
  timeout: 30000,
});
const userInput = await page.$(
  'input[autocomplete="username"], input[type="text"]',
);
if (userInput) await userInput.fill("admin");
const passInput = await page.$(
  'input[autocomplete="current-password"], input[type="password"]',
);
if (passInput) await passInput.fill("admin");
await page.click('button[type="submit"], button:has-text("Sign in")');
await page.waitForTimeout(2500);

// Go to Devices
await page.click('a[href="#/devices"]');
await page.waitForTimeout(2000);

// Expand a device
const hostname = await page.$("tbody tr td .host");
if (hostname) {
  await hostname.click();
  await page.waitForTimeout(1500);
}

// Test 1: Click Command button and check URL
const cmdBtn = await page.$('a:has-text("Command")');
if (cmdBtn) {
  await cmdBtn.click();
  await page.waitForTimeout(2000);
  const url = page.url();
  console.log(`After Command click, URL: ${url}`);
  await page.screenshot({
    path: "/tmp/rmmway-07-after-command.png",
    fullPage: true,
  });
}

// Go back to devices
await page.goto("http://localhost:5173/#/devices", {
  waitUntil: "networkidle",
});
await page.waitForTimeout(2000);

// Expand a device again
const hostname2 = await page.$("tbody tr td .host");
if (hostname2) {
  await hostname2.click();
  await page.waitForTimeout(1500);
}

// Test 2: Click Connect button
const connBtn = await page.$('a:has-text("Connect")');
if (connBtn) {
  await connBtn.click();
  await page.waitForTimeout(2000);
  const url2 = page.url();
  console.log(`After Connect click, URL: ${url2}`);
  await page.screenshot({
    path: "/tmp/rmmway-08-after-connect.png",
    fullPage: true,
  });
}

// Go back to devices
await page.goto("http://localhost:5173/#/devices", {
  waitUntil: "networkidle",
});
await page.waitForTimeout(2000);

// Expand a device again
const hostname3 = await page.$("tbody tr td .host");
if (hostname3) {
  await hostname3.click();
  await page.waitForTimeout(1500);
}

// Test 3: Click "Query Available Patches" button in Inventory tab
const invTab = await page.$('button:has-text("Inventory")');
if (invTab) {
  await invTab.click();
  await page.waitForTimeout(1500);
}

const queryBtn = await page.$('button:has-text("Query Available Patches")');
if (queryBtn) {
  // Listen for console errors
  page.on("console", (msg) => {
    if (msg.type() === "error") {
      console.log("CONSOLE ERROR:", msg.text());
    }
  });

  await queryBtn.click();
  await page.waitForTimeout(2500);
  await page.screenshot({
    path: "/tmp/rmmway-09-patch-error.png",
    fullPage: true,
  });
  console.log("Clicked Query Available Patches");
}

await browser.close();
console.log("Done.");
