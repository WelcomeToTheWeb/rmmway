# UI/UX Revamp Plan — "Modernize in place"

Goal: make the RMMWay operator UI production-grade without changing what the
product does. Keep the dark operator aesthetic and the existing pages; rebuild
them on a real design system, strip all development-task references from
user-facing text, and make the metrics charts actually readable.

Direction confirmed with the owner: **modernize in place** (same IA, better
craft). The two complaints this plan answers first:

1. **Dev-task references visible in the UI** — internal task IDs (W6-1, D-1,
   D-6, B-2, W2-2, W6-2) rendered in labels, tooltips and even a default
   script body.
2. **Charts are hard to read** — the single metrics chart is a bare SVG
   polyline with no gridlines, no ticks, no hover, and no baseline context.

---

## Status — executed 2026-07-19 (all phases shipped)

| Phase | Commit(s) | Result |
| --- | --- | --- |
| 0 — Design foundation | `3fab65c` | tokens (dark default + light), self-hosted Inter/JetBrains Mono, 17-component kit (`src/ui/`), kit-rebuilt topbar, theme toggle, human-readable health chip, `styles/` split; summary in `frontend/PHASE0-SUMMARY.md` |
| 1 — Copy cleanup | `5b1f07f` | all operator-visible task IDs + jargon removed (9 spots incl. the W2-2 default script → `rmmway-ping` probe) |
| 2+3 — Chart revamp & device detail | `3203907` | `src/ui/TimeSeriesChart.jsx` (nice ticks, gridlines, human time axis, hover crosshair/tooltip, unit-aware stats, live affordance, grouped human picker); device-detail extraction to `src/views/devices/DeviceDetail.jsx`; new `chart` smoke (`make chart-ui-smoke`); summary in `frontend/CHART-SUMMARY.md` |
| 4 — Shell & hardening | `d9fbd8b` | per-route error boundary, FLEET/OPS/SYSTEM nav grouping (anchor order untouched), `aria-current`, favicon/meta, real `/healthz` version chip, 768px responsive shell, visibility-aware health poll; summary in `frontend/PHASE4-SHELL-SUMMARY.md` |
| build chores | `e4ecbf5` | `chart-ui-smoke` Makefile target; Makefile CRLF→LF |
| 5 — Sign-off | this commit | full matrix 12/12 smokes green; headless-browser visual pass dark+light; before/after screenshot set in `docs/ui-revamp/evidence/` (regenerable via `docs/ui-revamp/rig/`) |

Verification at sign-off: `npm run build` green; 12/12 jsdom UI smokes pass
(11 pre-existing + chart); zero runtime deps added (`package.json` untouched);
all immutable smoke selectors preserved.

---

## 1. Current-state audit

Frontend: Vite + React 18, **zero runtime deps beyond react/react-dom**,
hash routing, hand-rolled SVG chart, 822-line single `styles.css`, 13 source
files, 11 jsdom smoke suites (`make *-ui-smoke`) that drive the real `<App/>`.

### 1.1 Dev-task references in user-visible copy (complaint #1)

These render in the UI today. Code *comments* citing task IDs are fine and
stay; everything below is operator-visible:

| # | Where | Visible text |
| --- | ------- | -------------- |
| 1 | `Devices.jsx:53` | `Recent indexed events (W6-1 · also shipped to Loki)` |
| 2 | `Devices.jsx:990` (title attr) | `Show/hide recent indexed events (W6-1)` |
| 3 | `Devices.jsx:211` | `Commands (D-1 · newest first, live over the event stream)` |
| 4 | `Devices.jsx:328` | `Client export (D-6) — one self-verifying ZIP: …` |
| 5 | `Devices.jsx:617` | `Tags (B-2)` |
| 6 | `Devices.jsx:1111` (title attr) | `Fan one capability-gated command out to every device carrying a tag (B-2)` |
| 7 | `Flows.jsx:568` | `message (fired through the notification seam; W6-2 adds webhooks)` |
| 8 | `Palette.jsx` (run-script default) | script body literally echoes `hello from RMMWay W2-2` |
| 9 | `Palette.jsx` (hint) | `Navigate (B-2 group)` |

Related dev-isms to fix in the same pass:

- `Login.jsx` — "Local dev default: `admin` / `admin`" on the production
  login screen.
- `Devices.jsx:317`, `Heal.jsx`, `Baseline.jsx` — "…not wired on this server
  (in-memory mode) — start with Postgres…" — internal architecture terms.
- Jargon pass: "capability-gated", "notification seam", "journaled",
  "envelope", "seq", "z-band", "σ" (keep σ in the chart, explain it), "pass
  summary" wording, raw `e.message` surfaced verbatim in error banners.
- `App.jsx` health chip: `title={JSON.stringify(health.probes)}` — raw JSON in
  a tooltip.

### 1.2 The metrics chart (complaint #2)

`MetricChart` (`Devices.jsx:~400`), the **only chart in the app**:

- 680×150 fixed viewBox; one `<polyline>`, 1px, no area fill.
- **No gridlines, no y-axis ticks** — only the min and max values, left side.
- **No x-axis ticks** — only start and end labels.
- **No hover/tooltip** — the operator cannot read a value at any point in
  time. This is the core readability failure.
- Y-scale is min/max of the visible window with no "nice" rounding → tiny
  variance reads as dramatic spikes; scale shifts silently on every 30s
  refresh.
- **No baseline context.** The product's core differentiator is dynamic
  baselining, yet the chart never shows the expected range — the one band an
  RMM operator wants to see. (Stretch: server-side, see Phase 2.)
- Metric picker is a `<select>` of raw dotted names
  (`cpu.utilization_percent`, `net.rx_bytes_total (lo0)`) — ungrouped,
  unlabelled, no units.
- Stats row is now/min/max/count only; no percentiles, no units in labels.
- Flat-line hack (`vmin -= 1; vmax += 1`) — brittle.
- 30s auto-refresh swaps data with no visual cue; chart doesn't indicate the
  data is live.

### 1.3 Design-system gaps (why the UI feels "in bad shape")

- **No shared components.** Every view hand-rolls buttons, tables, pills,
  banners, modals, tabs. `styles.css` is one 822-line file with per-view
  sections; identical patterns (`.banner.err`, `.row-actions`, `.empty`) are
  re-styled slightly per view.
- **Typography for prose, not data.** 16px body / 1.6 line-height is too big
  for a dense operator console; tables lack `tabular-nums`; mixed 11–16px
  ad-hoc sizes.
- **Iconography is ad-hoc unicode** (⚡ ↻ ✕ ▸ ◈ × →) — inconsistent weight,
  misaligned, poor a11y.
- **No light theme, no theming layer** beyond the dark `:root` block; dark is
  hard-wired.
- **Fonts load from Google Fonts CDN** — a self-hosted, privacy-first RMM
  product that phones home to `fonts.googleapis.com` on every load (and fails
  open to fallback fonts offline).
- **No responsive layout**: fixed 260px search inputs, `max-width: 1100px`
  content, tables that overflow under ~1000px.
- **Accessibility**: row-expansion is a plain `tr onClick` (no keyboard path,
  no `aria-expanded`), modals have no focus trap / Esc, `title` attributes as
  the only tooltips, no `:focus-visible` styles.
- **State handling is ad-hoc**: inconsistent empty/loading/error states, no
  error boundary (an exception = blank page), polling continues in hidden
  tabs, no "last updated" indicator on polling views.
- **Brand chrome missing**: no favicon, `<title>` is bare, no meta
  description, no version/org info anywhere in the shell.

### 1.4 Constraints

- **Zero runtime deps** (only react/react-dom) is an established value
  (self-hosting, supply chain). The revamp keeps it: hand-rolled component
  kit and chart, self-hosted font *files* (not a framework).
- **11 jsdom smoke suites must stay green.** They assert on DOM structure
  (`.device-export`, `.metrics-chart`, …) more than copy, but Phase 1/2 text
  changes and the chart rebuild must be validated against the full suite
  (`make` targets listed in §5) after every phase.
- Dev stack has no browser; visual verification = the prod stack
  (`docker-compose.prod.yml`) or `make dev` + vite proxy, per README.

---

## 2. Design direction

- **Keep**: dark-first operator aesthetic, the accent-blue palette family,
  Inter + JetBrains Mono, the top-bar shell, hash routes, the ⌘K palette.
- **Rebuild**: on a token-driven design system in `frontend/src/ui/` —
  one place for every visual primitive, so views stop re-inventing them.
- **Add**: light theme (CSS custom properties + `prefers-color-scheme` +
  persisted manual toggle), responsive behavior down to ~768px, and a
  production-grade data-readability pass (tabular numerals, dense table
  mode, honest time formatting).

### Token & component inventory (Phase 0 deliverables)

- `styles/tokens.css` — color scales (incl. light theme), spacing scale
  (4px base), type scale (14px body, 12.5–13px table data, `tabular-nums`
  for numeric columns), radius, elevation, motion (keep existing
  120/200/300ms easings + `prefers-reduced-motion`).
- Self-hosted fonts: `frontend/public/fonts/` (Inter 400/500/600/700,
  JetBrains Mono 400/500 woff2) + `@font-face`; system-stack fallback.
  Removes the Google CDN dependency.
- `src/ui/` kit, each with a smoke-tested render path:
  `Button`, `IconButton`, `Badge`/`StatusPill`, `Tabs`, `Modal`
  (focus trap + Esc), `Banner` (error/ok/info), `Field` (input/select/
  textarea + label + hint), `Table` (sticky header, optional sort),
  `EmptyState`, `Spinner`/`Progress`, `CodeBlock` (copy button, language
  label), `CopyField`, `Tooltip`, `Dropdown`, `SegmentedControl`,
  `Icon` (one small hand-rolled inline-SVG icon set replacing the unicode
  glyphs), `RelTime` (relative time, absolute on hover — kills the
  five duplicated `relTime`/`fmtAt` helpers).
- View styles move to `styles/views/<name>.css`; `styles.css` becomes
  tokens + base + kit only.

---

## 3. Phased plan

Phases are sequenced so each one ships independently, keeps the smoke suite
green, and reduces risk for the next. Estimate = working days for one
engineer.

### Phase 0 — Design foundation · M (2–3d)

**Description:** Introduce tokens, theming (dark default + light), the
`src/ui/` component kit, the icon set, and self-hosted fonts. Migrate **two
representative views** (Login + Alerts) onto the kit to prove the approach
before touching the rest. No behavior changes, no new features.

**Definition of done:**

- `:root` dark + `[data-theme="light"]` palettes; theme toggle in the top bar
  (persisted, respects `prefers-color-scheme`); `:focus-visible` styles
  everywhere.
- Google Fonts `<link>` and `@import` removed; fonts load from
  `/fonts/*` (verified offline: `npm run build` + page loads with network to
  fonts blocked).
- `src/ui/` kit exists and Login/Alerts are fully on it; all other views
  unchanged and all 11 smokes green.
- `npm run build` clean; no visual regression on the 2 migrated views
  (manual check on the prod stack, dark + light).

### Phase 1 — Copy & dev-reference cleanup · S (0.5–1d)

**Description:** Every operator-visible string rewritten. Delete the
§1.1 table entries; reword the in-memory-mode messages; hide the
`admin/admin` hint behind a dev-mode signal (e.g. only when `/healthz`
reports a dev env, or drop it into the setup-wizard screen where it actually
applies); replace the palette's `W2-2` echo script with something
meaningful (`echo RMMWay ping <hostname> $(date -u +%FT%TZ)`); fix the
health-chip raw-JSON tooltip; consistent error-banner wording.

**Definition of done:**

- `grep -rnE "\(W[0-9]+-[0-9]+|\(D-[0-9]|\(B-[0-9]|\(A-[0-9]|\(C-[0-9]|W2-2|W6-2" frontend/src/*.jsx`
  returns **zero matches in JSX text/attributes** (comments excluded).
- No view shows dev-task IDs, "in-memory mode", or "notification seam".
- Login screen shows no credential hints in a non-dev deployment.
- All 11 smokes green (assertions are selector-based; verify `metrics`,
  `groups`, `export` smokes after the label rewrites).

### Phase 2 — Chart revamp · M–L (2–4d; +1d stretch)

**Description:** Rebuild the metrics chart as a real, readable time-series
widget (`src/ui/TimeSeriesChart.jsx` + `src/charts/` helpers):

- **Hover crosshair + tooltip**: time (absolute + relative) and value with
  units at any x position; the tooltip follows the nearest sample.
- **Proper axes**: "nice-number" y ticks (4–6 gridlines) with units, x ticks
  at human intervals (minutes/hours/days by range); subtle gridlines;
  stable y-scale option (a "fit / fixed" toggle) so refreshes don't
  rescale under the operator.
- **Live affordance**: soft area fill, pulse on the last point, "updated
  Ns ago" label; range selector as a `SegmentedControl`.
- **Readable series picker**: grouped by category (CPU / Memory / Disk /
  Network / System / Other) with human labels (`CPU utilization %`),
  raw metric name in a sub-line; per-source series clearly marked
  (`net.in (lo0)`).
- **Stats row**: now, min, max, p95, sample count, units, range.
- **Empty/no-data/error/loading** states from the kit; no flat-line hack
  (proper constant-series handling).
- Still pure SVG, zero deps; `viewBox` computed from measured container
  width (responsive), fixed aspect.
- **Stretch (server, +1d):** extend `GET /api/devices/{id}/metrics/series`
  with the baseline expected band (seasonal + trend channel bounds for the
  window) and render it as a shaded "expected range" band on the chart.
  This is the feature that makes RMMWay's charting different; it's isolated
  so Phase 2 ships without it (client falls back to plain chart when the
  field is absent / 503).

**Definition of done:**

- Operator can hover any point and read exact time + value; axes are
  self-explanatory for all five ranges (1h…30d).
- Series picker shows grouped human labels; switching series/range works.
- New `scripts/chart.smoke.jsx` (+ Makefile `chart-ui-smoke`): drives the
  real `<App/>`, expands a device, asserts gridlines/ticks render,
  simulates mousemove over the plot and asserts the tooltip shows time +
  value, asserts range switching re-requests (mirrors `metrics.smoke`
  harness). Existing `metrics-ui-smoke` stays green.
- Stretch (if done): band renders when the server returns bounds; absent
  bounds → chart still fine.

### Phase 3 — Device detail & stream surfaces · M (2–3d)

**Description:** The device row currently expands into one stacked column
(Export, Tags, Metrics, Commands, Logs) — a long, flat scroll. Rebuild the
detail as a **tabbed panel** (Overview · Metrics · Logs · Commands) with
`src/ui/Tabs`, and make the stream surfaces readable:

- Split `Devices.jsx` (1,250 lines) into `src/views/devices/*` components
  (pure refactor; keep stable DOM classes the smokes select on).
- **Logs (agent events)**: level color-coding with left rail, relative time
  - absolute-on-hover, structured attrs as chips (not one `k=v k=v`
  string), word-wrap toggle, level filter + text filter, paused-when-
  scrolled-up indicator (live tail without yanking the operator's scroll).
- **Commands**: stdout/stderr as separate labelled `CodeBlock`s with copy
  buttons, exit code as a status badge, duration column.
- **Events journal** (`Events.jsx`): structured summary first, raw JSON
  demoted to a toggle (`CodeBlock`) — the raw-JSON-first expansion is the
  other "hard to read" surface.
- **Heal run trace**: stage timeline with human labels + durations; reason
  blocks as `CodeBlock`.

**Definition of done:**

- Device detail is tabbed; each tab loads independently; all 11 smokes
  green (selectors preserved or updated in lockstep).
- Logs: an operator can scan levels at a glance and expand one line's
  structured attrs without reading `k=v` soup.
- `Devices.jsx` no longer exists as a single 44KB file.

### Phase 4 — Shell, IA polish & production hardening · M (2–3d)

**Description:**

- Nav: group the 7 items into a lighter shell — keep all routes, add
  section dividers or a second-row grouping (Fleet: Devices, Alerts ·
  Automation: Flows, Heal · System: Events, Webhooks, Baseline); rename
  jargon items if the owner agrees ("Baseline" → "Anomalies" candidate).
- Favicon, proper `<title>`/meta, org name + build version in the top bar
  (from `/healthz` or a `/api/version` if needed).
- `ErrorBoundary` wrapping each view + global fallback (no more blank page).
- Consistent empty/loading/error states via the kit; "last updated" chip on
  polling views; polling paused when `document.hidden`.
- Responsive pass to ~768px (nav collapse, tables → card lists where
  overflow can't be fixed with scroll).
- a11y pass: keyboard-expansible rows (`aria-expanded`, Enter/Space),
  modal focus trap, `role="dialog"`/labels, `aria-live` on toasts, screen-
  reader smoke over the main flows.

**Definition of done:**

- Full a11y checklist green for: login, device list → detail tabs, alert
  triage, ⌘K palette, add-device modal.
- Page at 1280 / 1024 / 768 widths: no horizontal scroll on the shell;
  tables scroll inside their own containers.
- Kill the process mid-page (simulated throw): error boundary renders, app
  stays navigable.
- All smokes green; `npm run build` clean.

### Phase 5 — Verification & sign-off · S (1d)

- Run the **full UI smoke matrix** (all `make *-ui-smoke`), both Go modules
  (`build` + `test`) if the stretch server change landed, and `npm run
  build`.
- Manual visual pass on the prod stack, dark **and** light, all 9 routes +
  both modals + palette: §1.1 table entries gone, chart readable, tabs
  working, no console errors, fonts loading offline.
- Screenshot set (before/after per page) committed under
  `docs/ui-revamp-shots/` for the release notes. (Delivered as
  `docs/ui-revamp/evidence/` — before = `347d1ea`, after = sign-off commit.)
- README updated (self-hosted fonts, theme toggle).

**Effort summary:** ~10–14 working days total (12–15 with the baseline-band
stretch), 5 independently shippable phases, smoke suite green after each.

---

## 4. Non-goals

- No drag-and-drop fleet dashboard (that's B-3 in TASKS.md — separate
  track).
- No ChatOps/LLM features (Track C).
- No server schema/protocol changes beyond the optional Phase-2 stretch
  field.
- No framework adoption (no Tailwind/UI library) — the zero-runtime-dep
  policy stands.

## 5. Verification commands (after every phase)

```sh
make frontend-deps
make setup-ui-smoke adddevice-ui-smoke sse-ui-smoke groups-ui-smoke \
     metrics-ui-smoke commands-ui-smoke events-ui-smoke heal-ui-smoke \
     webhooks-ui-smoke baseline-ui-smoke export-ui-smoke
# Phase 2 adds:
make chart-ui-smoke
cd frontend && npm run build
```

## 6. Risks & mitigations

| Risk | Mitigation |
| ------ | ------------ |
| Smoke suites assert DOM structure; kit migration could break selectors | Phase 0 migrates only 2 views first; keep existing class names as stable aliases; run the full matrix after every phase, not at the end |
| No browser in the dev environment | Visual verification is explicit per-phase DoD on the prod stack; smokes cover behavior, not pixels |
| Hand-rolled chart edge cases (flat series, 1-point series, gaps) | `chart.smoke` includes degenerate fixtures (constant, single point, holes in the series) |
| Baseline-band stretch touches the Go server | Additive JSON field, backward compatible; 503/absent → client renders plain chart; separate commit, separate PR |
| Theme toggle regresses dark-mode polish | Dark stays the default and the primary tested theme; light is verified page-by-page in Phase 0/4 |
