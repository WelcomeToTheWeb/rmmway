# RMMWay UI/UX Improvement Plan — Post-1.0

**Date:** 2026-09-10
**Method:** Three-angle codebase review (structural, user-flow, backend/API) + live browser testing
**Status:** Complete
**Date Completed:** 2026-09-10

---

## Completion Summary

All critical phases are complete:

- Phase 0: All bug fixes verified (routing, patching API, dashboard tiles) — already in code
- Phase 1: Core functionality restored (device overview, commands, inventory UX) — already in code
- Phase 2: Dashboard polish completed — already in code
- Phase 3: Metrics improvements implemented (multi-series, correlated, better defaults) — already in code
- Phase 4: Empty states and loading skeletons improved — implemented this session
- Phase 5: Navigation fixes and breadcrumbs added — implemented this session
- Phase 6: Responsive design verified — already in code

Build passes. All changes lint-clean.

### New code added this session

- `frontend/src/ui/Breadcrumb.jsx` — breadcrumb navigation component
- Breadcrumb styles in `frontend/src/styles/shell.css`
- Integrated breadcrumbs into `frontend/src/App.jsx`
- Replaced text "loading…" states with `Skeleton` components in Dashboard, DeviceInventory, QuickHealth, Palette
- Replaced text-only empty states with `EmptyState` component + icons in DeviceDetail
- Fixed nav grouping: "Baseline" → "Anomalies", "Reports" moved to System group

Remaining (deferred):

- Phase 4.1: Devices.jsx UI kit migration (large refactoring)
- Phase 6.1: Virtual scrolling for large device lists (requires new dependency)

---

## Executive Summary

The UI is **functionally broken** in four specific ways that the user has reported. These are not aesthetic issues — they are bugs that block core workflows. The good news: there is already a solid design-system foundation (`src/ui/`, token-based CSS) that was built during the 1.0 revamp. The work below is mostly **repair + enrichment**, not a rebuild.

| # | User Complaint | Root Cause (verified) | Severity | Fix Effort |
| --- | --- | --- | --- | --- |
| 1 | Connect/Command buttons send to Dashboard | `#/session/:id` and `#/commands/:id` routes are unregistered; `parseRoute()` falls through to `"dashboard"` | 🔴 Critical | 0.5 day |
| 2 | Patching gives an error | `DevicePatches.jsx` calls `api.dispatchCommand` — this method does not exist (should be `api.dispatch`) | 🔴 Critical | 15 min |
| 3 | Inventory doesn't work correctly | Works, but (a) data shows as dashes when uncollected, (b) no "Collect now" button in the UI, (c) patches section is buried at the bottom | 🟡 Medium | 1 day |
| 4 | Device Overview shows only Export | Overview tab renders `DeviceExport` + `TagEditor` only — missing device identity, quick stats, inventory snapshot | 🟡 Medium | 2 days |
| 5 | Dashboard tiles broken (Tickets, Patch compliance) | `Dashboard.jsx` calls `api.get(...)` — this method does not exist on the `api` object | 🟡 Medium | 1 day |
| 6 | Metrics is 1 graph, not easy to read | By design — single series per chart. Needs multi-series layout or comparison mode | 🟢 Enhancement | 3–4 days |
| 7 | Overall "messy / thrown together" feel | Inconsistent use of UI kit components; some views (Devices) are 1,000+ lines with hand-rolled markup alongside kit components | 🟢 Enhancement | 3–5 days |

**Total effort: ~9–13 working days** to reach a production-grade, trustworthy UI.

---

## Phase 0 — Stop the Bleeding (Critical Bugs)

**Goal:** Fix the four confirmed bugs so core workflows work again. No visual changes.

### 0.1. Fix the Connect/Command routing bug

**File:** `frontend/src/App.jsx`
**Problem:** `parseRoute()` only returns paths from `NAV_ITEMS`. `session/:id` and `commands/:id` are not in that list, so they fall through to the default `"dashboard"` route. The `Shell` component checks `route.startsWith("session/")` for the SessionViewer, but this never fires because `parseRoute()` already returned `"dashboard"`.

**Fix:** Change `parseRoute()` to return the full path for sub-routes, or move the sub-route detection into `parseRoute`:

```javascript
function parseRoute() {
  const h = window.location.hash;
  // Check sub-routes first (they are not in NAV_ITEMS)
  if (h.startsWith("#/session/")) return h.slice(2); // "session/dev-abc..."
  if (h.startsWith("#/commands/")) return h.slice(2); // "commands/dev-abc..."
  // Then check NAV_ITEMS
  for (const item of NAV_ITEMS) {
    if (item.kind === "route" && h.startsWith("#/" + item.path)) {
      return item.path;
    }
  }
  return "dashboard";
}
```

**Verification:**

- Click "Connect" on a device → should navigate to `#/session/:id` and render `SessionViewer`
- Click "Command" on a device → should navigate to `#/commands/:id` and render the Commands view (which currently does not exist — see Phase 1.2)

### 0.2. Fix the patching API method

**File:** `frontend/src/views/devices/DevicePatches.jsx` (lines 20 and 56)
**Problem:** Calls `api.dispatchCommand(...)` which is undefined.

**Fix:** Change `api.dispatchCommand` → `api.dispatch`.

**Verification:**

- Navigate to Device → Inventory tab → click "Query Available Patches"
- Expected: No error, command dispatched to agent (if online)

### 0.3. Fix Dashboard tiles (Tickets + Patch Compliance)

**File:** `frontend/src/Dashboard.jsx` (lines 157 and 169)
**Problem:** Calls `api.get(...)` which is undefined. The `api` object has named methods, not a generic `get`.

**Fix:** Add the missing API methods to `frontend/src/api.js` and use them:

```javascript
// In api.js:
tickets: (token, { status = "", limit = 100 } = {}) => {
  const q = new URLSearchParams();
  if (status) q.set("status", status);
  if (limit) q.set("limit", String(limit));
  const qs = q.toString();
  return request(`/api/tickets${qs ? "?" + qs : ""}`, { token });
},
```

Then in Dashboard.jsx, replace:

```javascript
// BEFORE (broken):
const res = await api.get("/api/tickets?status=open,in_progress&limit=5");
// AFTER (fixed):
const res = await api.tickets(token, { status: "open,in_progress", limit: 5 });
```

**Verification:**

- Load Dashboard → scroll to bottom → Tickets tile shows ticket data, not "unavailable"
- Patch compliance tile shows inventory status, not "unavailable"

---

## Phase 1 — Restore Core Functionality

**Goal:** Make the UI actually do what it claims to do.

### 1.1. Rebuild the Device Overview tab

**File:** `frontend/src/views/devices/DeviceDetail.jsx`
**Problem:** The "Overview" tab is the default landing view when expanding a device, but it only shows the Export widget and a Tag editor. An RMM operator needs to see the device's identity, health, and key facts immediately.

**Current Overview tab content:**

- Export this device (widget)
- Tags (tag editor)

**Proposed Overview tab content** (in this order):

1. **Device Identity Card**
   - Hostname (large, with edit button to change display name)
   - Device ID (copyable, mono font)
   - Status pill (online/offline + last seen)
   - OS / Architecture (e.g., "linux/amd64")
   - Agent version
   - IP addresses (primary + expandable list of all)
   - Client assignment (from Clients list)
   - Uptime (if available from metrics)

2. **Quick Health Snapshot**
   - CPU utilization (last hour sparkline)
   - Memory usage (last hour sparkline)
   - Disk usage (last hour sparkline)
   - Network throughput (last hour sparkline)
   - Each with a "view in Metrics →" link

3. **Inventory Snapshot** (from `DeviceInventory` data)
   - Hardware summary: CPU model, cores, RAM
   - Software count (installed packages)
   - Last inventory collection timestamp
   - "Collect now" button (calls `api.collectDeviceInventory`)
   - "View full inventory →" link to the Inventory tab

4. **Tags** (existing TagEditor, moved below)

5. **Export** (existing DeviceExport, moved below)

**Implementation notes:**

- Use the existing `DeviceInventory` component (already fetches hardware/software)
- Add new API calls for quick metrics (the Metrics API exists)
- Reuse existing UI kit components: `Badge`, `Button`, `Table`, `RelTime`
- Keep the Export and TagEditor where they are (bottom), so existing smokes still pass

**Effort:** 2 days
**Risk:** Low — pure frontend, uses existing API endpoints

### 1.2. Add a proper Commands view for `#/commands/:id`

**Problem:** The Command button navigates to `#/commands/:id`, but no component is registered for that route. Currently it falls through to Dashboard (see Phase 0.1).

**Solution:** Either:

- **(A) Quick fix (0.5 day):** Point the Command button to `#/devices` and auto-expand the Commands tab of that device. This reuses the existing `DeviceCommands` component.
- **(B) Full fix (2 days):** Create a dedicated `Commands.jsx` view at `frontend/src/Commands.jsx` that renders the `DeviceCommands` component for the specified device ID, with a "Back to device" breadcrumb.

**Recommendation:** Option A for now — it's functional and uses existing code.

**Implementation for Option A:**
In `DeviceDetail.jsx`, change the Command button:

```javascript
// BEFORE:
<a href={`#/commands/${device.id}`} className="btn">⚡ Command</a>
// AFTER:
<button 
  className="btn" 
  onClick={() => {
    // Close the device detail, then open Commands tab
    window.location.hash = `#/devices`;
    // Trigger tab change via a custom event or parent state
    document.dispatchEvent(new CustomEvent('device-open-tab', { detail: { id: device.id, tab: 'commands' } }));
  }}
>
  ⚡ Command
</button>
```

**Effort:** 0.5 day (Option A) or 2 days (Option B)

### 1.3. Improve Inventory UX

**File:** `frontend/src/views/devices/DeviceInventory.jsx` and `DevicePatches.jsx`

**Problems:**

1. When no inventory is collected, the tab shows dashes and "No software inventory collected yet" — but there is no way to trigger collection.
2. The Patch Management section is at the very bottom, easy to miss.
3. The "Query Available Patches" button gives no feedback when it works (it dispatches a command but says "Results will appear after next heartbeat" — which is misleading).

**Fixes:**

1. **Add "Collect Inventory Now" button** to the Hardware section when empty:

   ```javascript
   <button className="btn" onClick={collectInventory}>
     🔄 Collect Inventory Now
   </button>
   ```

   (Calls `api.collectDeviceInventory(token, device.id)`)

2. **Move Patch Management above Hardware/Software** — it's the more actionable item.

3. **Show patch query results** — poll the device's inventory/patch data and display it. Currently the UI just says "results will appear after next heartbeat."

4. **Add inline error/loading states** for the patch operations.

**Effort:** 1 day
**Risk:** Low

---

## Phase 2 — Dashboard Polish & Data Integrity

**Goal:** Fix the broken tiles and make the Dashboard trustworthy.

### 2.1. Fix the Tickets tile

Already covered in Phase 0.3, but also needs:

- A link to `#/tickets` (the Tickets view exists but is not in NAV_ITEMS — verify it works)
- Status breakdown (open/in_progress/resolved) as pills

### 2.2. Fix the Patch Compliance tile

Already covered in Phase 0.3. Additionally:

- Show patch compliance rate (% of devices with recent inventory)
- List devices with stale/missing inventory (with links to collect)
- Add a "Refresh" button

### 2.3. Verify the Tiles link destinations

The Dashboard has "devices →", "inbox →", "baseline →", "journal →" links. Verify each target view works and is in NAV_ITEMS. Currently `tickets` and `reports` are in NAV_ITEMS but may need verification.

**Effort:** 0.5 day
**Risk:** Low

---

## Phase 3 — Metrics Readability (The "1 Graph" Problem)

**Goal:** Make metrics actually readable and useful.

### 3.1. Multi-series view

**Current:** One dropdown, one chart, one metric at a time.
**Problem:** Comparing CPU and Memory requires clicking the dropdown repeatedly and memorizing values.

**Proposed:** Add a "Multi-metric" mode to `DeviceMetrics`:

- Show 3-4 small charts stacked (CPU, Memory, Disk, Network)
- Or show one chart with multiple series (overlaid)
- Add a "Compare" button to select multiple metrics

**Implementation:**

- Create a `MultiMetricDashboard` component in `src/views/devices/`
- Reuse `TimeSeriesChart` from `src/ui/`
- Fetch multiple series in parallel via `api.metricsSeries`

**Effort:** 3 days
**Risk:** Medium — needs careful state management

### 3.2. Better chart defaults

- **Time range:** Auto-detect best range based on data availability (e.g., if device is only 2 hours old, don't default to 30d)
- **Y-axis:** Auto-scale with nice round numbers (already partially done in `TimeSeriesChart`)
- **Baseline overlay:** Show expected baseline range as a shaded band (requires backend work — stretch)

### 3.3. Correlated metrics view

- Show CPU + Load average together
- Show Memory + Swap together
- Show Network In + Out together (dual-axis or stacked)

**Effort:** 1 day (if 3.1 is done)

---

## Phase 4 — Consistency & Craft (The "Thrown Together" Fix)

**Goal:** Make the UI feel cohesive and professional.

### 4.1. Audit and standardize UI kit usage

**Problem:** `Devices.jsx` is 1,000+ lines and mixes hand-rolled markup with UI kit components. Some views (Dashboard, Alerts) use the kit well; others (Devices, Heal) hand-roll tables, buttons, and banners.

**Approach:**

1. **Inventory the gaps:** List all hand-rolled components in `Devices.jsx`, `Heal.jsx`, `Flows.jsx`
2. **Migrate to kit:** Replace with `Table`, `Button`, `Badge`, `Banner`, `Tabs`, etc. from `src/ui/`
3. **Extract view-specific components:** Move device row rendering into `src/views/devices/DeviceRow.jsx`

**Key migrations:**

- `Devices.jsx` table → `src/ui/Table.jsx`
- `Devices.jsx` status pills → `src/ui/Badge.jsx` (StatusPill)
- `Devices.jsx` banners → `src/ui/Banner.jsx`
- `Devices.jsx` tabs → `src/ui/Tabs.jsx` (already used in DeviceDetail, but not in Devices)

**Effort:** 3 days
**Risk:** Medium — must preserve existing smoke test selectors

### 4.2. Empty state design

**Problem:** Many views show bare "Loading..." or "No data" text. The Login page and Devices empty state are decent, but Alerts, Inventory, and others need work.

**Approach:**

- Use `src/ui/EmptyState.jsx` consistently
- Add illustration-style icons (SVG) for empty states
- Add "How to get data here" instructions (e.g., "Run 'Collect Inventory' to populate this view")

**Effort:** 1 day
**Risk:** Low

### 4.3. Loading skeletons

**Problem:** Views show "loading..." text, which looks unfinished.

**Approach:**

- Replace text loading states with skeleton screens (shimmer effect)
- Use `src/ui/Spinner.jsx` for small inline loading, not for full page

**Effort:** 1 day
**Risk:** Low

---

## Phase 5 — Navigation & IA (Information Architecture)

**Goal:** Make the UI easier to navigate and discover features.

### 5.1. Fix the top nav grouping

**Current:**

- FLEET: Dashboard, Devices, Alerts, Clients
- OPS: Flows, Events, Heal, Reports
- SYSTEM: Webhooks, Baseline, Settings

**Issues:**

- "Baseline" is confusing — it's really an anomaly detection view
- "Reports" is under OPS but is a data export feature
- No "Tickets" in the nav (exists as a view but not in NAV_ITEMS)
- No "Inventory" in the nav (it's a tab inside Devices, but could be a top-level view for cross-device inventory search)

**Proposed changes:**

1. Rename "Baseline" → "Anomalies" (clearer)
2. Add "Tickets" under FLEET (it's a core feature)
3. Move "Reports" under SYSTEM (it's a reporting/export tool)
4. Add "Sessions" under OPS (for the remote session feature)

**Effort:** 0.5 day
**Risk:** Low

### 5.2. Breadcrumb navigation

**Current:** No breadcrumbs. When you're in a device detail, it's not obvious how to get back.

**Approach:**

- Add breadcrumb bar below the top nav
- Format: `Devices > PiCodingWay > Metrics`
- Click to navigate up

**Effort:** 0.5 day
**Risk:** Low

---

## Phase 6 — Performance & Responsiveness

### 6.1. Virtual scrolling for large device lists

**Current:** `Devices.jsx` renders all rows. With 1,000+ devices this will be slow.

**Approach:**

- Add windowing/virtualization to the device table
- Libraries to consider: `@tanstack/react-virtual` (if dependency allowed) or hand-roll

**Effort:** 2 days
**Risk:** Medium

### 6.2. Responsive design

**Current:** Partially responsive (there's a `devices-mobile.css`), but Dashboard tiles and some other views may not adapt well.

**Approach:**

- Test at 375px (mobile), 768px (tablet), 1024px (laptop)
- Fix overflow issues on wide tables
- Ensure touch targets are ≥ 44px

**Effort:** 2 days
**Risk:** Low

---

## Implementation Sequence & Milestones

### Milestone 1: Critical Fixes (Days 1–2)

- Phase 0.1: Fix routing (Connect/Command)
- Phase 0.2: Fix patching API
- Phase 0.3: Fix Dashboard tiles

**Milestone:** All confirmed bugs are fixed. Core workflows work.

### Milestone 2: Core Functionality (Days 3–6)

- Phase 1.1: Rebuild Device Overview
- Phase 1.2: Add Commands view (Option A)
- Phase 1.3: Improve Inventory UX

**Milestone:** Device detail is useful. Inventory and patching work end-to-end.

### Milestone 3: Dashboard Trust (Day 7)

- Phase 2.1–2.3: Dashboard polish

**Milestone:** Dashboard is trustworthy — no broken tiles.

### Milestone 4: Metrics Readability (Days 8–11)

- Phase 3.1–3.3: Multi-series metrics

**Milestone:** Metrics are actually readable and comparable.

### Milestone 5: Craft (Days 12–17)

- Phase 4.1–4.3: UI consistency, empty states, loading

**Milestone:** UI feels professional and cohesive.

### Milestone 6: Navigation (Days 18–19)

- Phase 5.1–5.2: Nav fixes, breadcrumbs

**Milestone:** Easy to navigate.

### Milestone 7: Performance (Days 20–24)

- Phase 6.1–6.2: Virtual scrolling, responsive

**Milestone:** Fast with 1,000+ devices, works on mobile.

---

## Technical Debt Notes

From the code review, these are non-blocking but should be tracked:

1. **`Devices.jsx` is 1,087 lines** — should be split into `DevicesList.jsx`, `DeviceRow.jsx`, `DeviceToolbar.jsx`, `DeviceTable.jsx`
2. **Duplicate `relTime` function** — defined in `Devices.jsx`, `Dashboard.jsx`, and others. Should be in `src/ui/RelTime.jsx` (already exists!)
3. **`api.js` has no generic `get` method** — Dashboard tried to use one. Either add it or enforce named methods (current approach is safer).
4. **Inconsistent token handling** — some views use `useAuth()`, some receive `token` as prop. Standardize on one pattern.
5. **Missing error boundaries** — `App.jsx` has `ErrorBoundary` per route, but view-level errors (e.g., in a tile) can still crash the page.

---

## Verification Checklist

After each phase, verify:

- [ ] `npm run build` succeeds
- [ ] `make frontend-ui-smoke` (or equivalent) passes
- [ ] Manual test: login → dashboard → devices → expand device → all tabs work
- [ ] Manual test: Connect and Command buttons navigate correctly
- [ ] Manual test: Patching "Query Available Patches" works
- [ ] Manual test: Dashboard tiles show data (not "unavailable")

---

## Appendix: File Inventory

**Critical files to modify:**

- `frontend/src/App.jsx` — routing fix
- `frontend/src/views/devices/DevicePatches.jsx` — API method fix
- `frontend/src/api.js` — add tickets method
- `frontend/src/Dashboard.jsx` — fix api.get calls
- `frontend/src/views/devices/DeviceDetail.jsx` — rebuild Overview tab
- `frontend/src/views/devices/DeviceInventory.jsx` — add collect button, move patches up

**New files to create:**

- `frontend/src/views/devices/DeviceIdentity.jsx` — identity card component
- `frontend/src/views/devices/QuickHealth.jsx` — metrics sparklines
- `frontend/src/views/devices/MultiMetricChart.jsx` — multi-series view

**UI kit files to extend:**

- `frontend/src/ui/EmptyState.jsx` — add illustration support
- `frontend/src/ui/Spinner.jsx` — add skeleton variant

---

*End of plan.*
