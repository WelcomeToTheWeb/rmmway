# Phase 4 — Shell, IA polish & production hardening (UI revamp)

Completed 2026-07-19. Build green (`--outDir dist-b`); all 11 jsdom UI smokes green.

## Items implemented

### 1. Error boundary (item 1)

- `src/ui/ErrorBoundary.jsx` (new) — class component with
  `getDerivedStateFromError` + `componentDidCatch` (full trace to
  `console.error`).
- `App.jsx` wraps `<main className="content">` in
  `<ErrorBoundary key={route}>`. One boundary **per route**, so a caught
  error panels that view but the rest of the app stays navigable (route
  change remounts a fresh boundary — the "kill mid-page" DoD).
- Panel: kit `.empty` block tinted with the error banner tokens,
  "Something went wrong" + one-line error message in a mono `pre` + a
  primary **Reload** button (`window.location.reload()`). `role="alert"`.
- Exported from `src/ui/index.js` (the only edit to that file).
- Deliberately **not** wrapped: the topbar, the login screen, and the
  setup wizard (the boundary sits only around the post-login content
  area, as specified).

### 2. Nav grouping (item 2)

- All 7 route links kept with the SAME text and SAME order (the smokes
  assert `navHrefs.slice(0,7) === "#/devices,#/alerts,#/flows,#/events,
  #/heal,#/webhooks,#/baseline"` in `baseline`/`events`/`heal` smokes).
- Group separators are non-anchor `<span class="nav-sep">` elements:
  **Fleet** (Devices, Alerts) · **Ops** (Flows, Events, Heal) ·
  **System** (Webhooks, Baseline), plus a divider-only separator before
  the Search/⌘K action link. Styled in `shell.css`: thin 1px divider +
  10px uppercase label, token colors only. `aria-hidden` (labels are
  decorative; the links are the real navigation).
- Note: the plan doc suggested "Fleet: Devices, Alerts · Automation:
  Flows, Heal · System: Events, Webhooks, Baseline" — that grouping
  interleaves with the fixed anchor order the smokes pin (Events sits
  between Flows and Heal), so the labels were adapted to the actual
  order instead of reordering links.

### 3. a11y pass (item 3)

- `aria-current="page"` on the active nav link (undefined/omitted on the
  rest).
- Icon buttons: the theme toggle already carried `aria-label` (Phase 0);
  verified, no change needed. The sign-out button has visible text.
- `:focus-visible` ring: a global rule already exists in `base.css`
  (`:where(a, button, input, select, textarea, [role=tab], [tabindex])`
  → 2px `var(--accent)` outline) covering nav links and all buttons —
  kept as the single source rather than duplicating it in `shell.css`.

### 4. Brand chrome in `index.html` (item 4)

- Favicon: inline SVG **data URI** (no new file, no network request):
  the same ◈ diamond-node glyph the top bar renders, accent blue on the
  dark surface, rounded square.
- `<meta name="description">` — one-line product description.
- `<meta name="theme-color" content="#0b0e14">` — matches the dark
  default background.
- Title kept as "RMMWay".

### 5. Version footer (item 5)

- **Done, not skipped.** `GET /healthz` already returns
  `{"ok", "version", "probes"}` (`server/cmd/server/main.go`, version
  from `RMMWAY_VERSION`, default `0.1.0`) — the topbar's existing 10s
  health poll now renders it as a small mono `.brand-version` chip next
  to the brand (hidden when the field is absent, e.g. in the smokes'
  minimal healthz mock).

### 6. Responsive shell to 768px (item 6)

- CSS-only media query in `shell.css`: topbar wraps; nav drops to its
  own full-width, horizontally scrollable row (`order: 3`,
  `overflow-x: auto`); nav items stay unwrapped; content padding
  relaxes to `--space-4` and `.view` drops its 1100px cap. Inert in
  jsdom, so smokes see the desktop layout.

### 7. Visibility pause (item 7)

- The topbar's health poll (the only shell-level poll) now stops its
  interval while `document.hidden` (via `visibilitychange`) and refreshes
  immediately + resumes when the tab returns. Minimal, jsdom-safe
  (jsdom reports visible; no synthetic events fired).
- The alerts 15s poll and the SSE stream were left running — pausing the
  live stream in a hidden tab would silently drop the instant badge/inbox
  updates B-1 is built around.

## Items skipped

None — all 7 items landed.

## Verification results

- `npm run build -- --outDir dist-b` — **green** (62 modules; the ~2.9k
  pi-lens findings are all lint noise inside the minified single-line
  `dist-b` vendor artifact, not source).
- Smoke matrix — **11/11 pass**: setup-wizard, sse, adddevice, metrics,
  commands, events, heal, webhooks, baseline, groups, export.
- `dist-b` removed after verification.
- Nav anchor order verified against the smoke assertions before editing
  (baseline/events/heal smokes pin the first 7 hrefs; only spans were
  added).

## Files changed

- `frontend/src/App.jsx` — nav groups, `aria-current`, version chip,
  visibility-aware health poll, `<ErrorBoundary key={route}>` wrap.
- `frontend/src/ui/ErrorBoundary.jsx` — new.
- `frontend/src/ui/index.js` — +1 export line.
- `frontend/index.html` — favicon data URI, meta description,
  theme-color.
- `frontend/src/styles/shell.css` — `.nav-sep`, `.brand-version`,
  `.error-fallback`, `@media (max-width: 768px)` block.

## Notes

- pi-lens autofix reformats on write — judge content via `git diff -w`.
- The version chip only shows on a real server (`RMMWAY_VERSION` set);
  the smokes' healthz mock omits `version`, so the chip renders nothing
  there — no behavior change in tests.
