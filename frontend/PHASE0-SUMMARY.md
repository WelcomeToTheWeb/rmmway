# Phase 0 — Design foundation (UI revamp)

Completed 2026-07-19. Build green; all 11 jsdom UI smokes green.

## What landed

### 1. Self-hosted fonts (offline)

- `public/fonts/`: Inter 400/500/600/700 + JetBrains Mono 400/500 (woff2, ~90 KB total)
- `src/styles/fonts.css`: `@font-face` declarations, font-display swap
- Removed the Google Fonts `@import` (was blocking first paint on offline
  installs) and the `<link rel="preconnect">` pair in `index.html`

### 2. Token system — `src/styles/tokens.css`

- Full dark palette (default) **and** light palette via `[data-theme="light"]`
- Surfaces, ink, borders, semantic status (ok/warn/err/info/accent), type
  scale (14px base), spacing, radii, elevation
- Every component file now references tokens only — no raw hex in component
  styles

### 3. Component kit — `src/ui/` (17 files) + `src/styles/kit.css`

- `Icon` (hand-drawn SVG set, no icon dependency), `Button` (variants +
  sizes), `IconButton`, `StatusPill`/`Badge`, `Banner`, `Tabs`, `Modal`,
  `EmptyState`, `Spinner`, `Field`, `CodeBlock`, `RelTime`, `Segmented`,
  `Tooltip`, `theme.jsx`
- Exposed via `src/ui/index.js`

### 4. Shell + migrations

- `App.jsx`: topbar rebuilt on kit (nav, human-readable health chip, theme
  toggle). Theme = `data-theme` on `<html>`, persisted in localStorage
  `rmmway-theme`, default follows `prefers-color-scheme`
- `Login.jsx` + `Alerts.jsx` migrated onto kit components; every class the
  setup-wizard/sse smokes assert on is preserved
- `styles.css` is now 8 `@import` lines; every pre-existing class/selector
  still resolves (views not yet migrated keep working)

## Verification

- `npm run build` — green
- 11/11 smokes: setup-wizard, sse, adddevice, metrics, commands, events,
  heal, webhooks, baseline, groups, export

## Notes / known issues

- pi-lens autofix reformats touched files on write (100-col prettier) —
  diffs include formatter churn beyond content edits; judge content via
  `git diff -w`
- One real bug found & fixed during verification: a CSS comment in
  `kit.css` contained `*/` inside the text, terminating the comment early
  (PostCSS "Unknown word"); reworded
- `Modal.jsx` was importing a named export from `IconButton.jsx` which is a
  default export — fixed
- Theme toggle is wired but views not yet migrated will restyle automatically
  (tokens cascade); full visual pass happens in Phase 5
