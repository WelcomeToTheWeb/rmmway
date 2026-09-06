# Phase 2+3 — Chart revamp & device-detail surfaces (UI revamp)

Completed 2026-07-19 (child produced `TimeSeriesChart.jsx` + `DeviceDetail.jsx`
before the 45-min workflow window closed; rewire, smoke, and styles finished by
the orchestrator). Build green; all 12 jsdom UI smokes green (11 pre-existing +
new `chart`).

## What changed

### 1. New chart — `src/ui/TimeSeriesChart.jsx` (pure SVG, zero deps)

Replaces the old 680×150 `MetricChart` (bare `<polyline>`, no axes, no hover):

- **Y axis**: "nice" round tick steps (1/2/5×10^n, 4–7 ticks) with gridlines
  (`line.metrics-grid`) and compact unit-aware tick labels (1.2k, 3.4M, %,
  h/d for seconds). Percent metrics floor at 0; constant series get ±10%
  padding so a flat line still has a plot.
- **X axis**: human time ticks — 1m/5m/15m/30m/1h/3h/6h/12h/1d steps, HH:MM
  within a day, date-prefixed across days, midnight ticks show the date only.
- **Series**: `<polyline class="metrics-line">` (the smoke contract) + soft
  area fill (`.metrics-area`) + last-sample dot (`.metrics-last`).
- **Hover**: crosshair (`line.metrics-crosshair`) + hover dot + a tooltip
  snapped to the nearest sample showing the human label, the unit-formatted
  value, relative time ("12m ago") and absolute time. Mouseleave clears it.
  The hover math is deterministic in jsdom (zero-width rect ⇒ clientX is used
  as SVG user units) — which is exactly what the new smoke exercises.
- **Stats row**: now / min / max / avg / p95 (unit-aware: `61.0%`, `2.3 GiB`,
  `342m`) + `N samples · range · Ns buckets` meta line.
- **Title row**: humanized metric name (`cpu.utilization_percent` →
  "CPU utilization", with a hand-written label table for the metrics agents
  actually ship and a "Category <leaf>" fallback), the raw dotted name as
  secondary context, and a **live · updated Ns ago** affordance (10s tick).
- **Series picker**: `optgroup` grouping by category (CPU / Memory / Disk /
  Network / Process / System / Other). Option values stay the ORIGINAL server
  list indices, so selection math and the smoke contract are
  order-independent.
- Single-sample edge case: a ±1h rendered domain so one point sits mid-plot
  instead of pinning a zero-width window.

### 2. Device-detail extraction — `src/views/devices/DeviceDetail.jsx`

The five device-detail components moved out of the 1,339-line `Devices.jsx`
(→ 590 lines) into `src/views/devices/DeviceDetail.jsx`, composed by a single
`DeviceDetail` component. DOM classes are byte-identical (the smoke contract);
the readability pass landed in styles:

- `DeviceEvents` — the agent-log panel gained a line filter (text + level
  select) and a bounded scroll region.
- `DeviceCommands` — expandable rows now render the agent's reported output
  through the kit `CodeBlock` (copy button, stderr tinted) with the exit
  code on its own line; `liveTick` re-fetch on SSE command envelopes kept.
- `DeviceExport`, `TagEditor`, `DeviceMetrics` — moved unchanged apart from
  `DeviceMetrics` now rendering `TimeSeriesChart` instead of the old chart.
- Dead code removed (the now-unused local `fmtMetricValue` in the extracted
  file).

### 3. Styles — `src/styles/views/devices.css`

- New chart surface: title row, live dot, plot wrapper (tooltip anchoring),
  gridlines, area fill, crosshair, hover dot, tooltip card (all on tokens —
  `--border`, `--accent-2`, `--chart-fill`, `--bg-elev2`, …). Base chart
  rules (`.metrics-chart/.metrics-plot/.metrics-line/.metrics-last`,
  `text`) were already in place from Phase 0.
- Command/log readability: bounded log scroll, attribute dimming, exit-code
  line, stdout/stderr block labels + bounded scroll.
- `src/styles/kit.css`: **formatting-only** churn from the autofix (one
  property per line, transition lists wrapped) — verified semantically
  identical (0 selectors added/removed, 0 value changes).

### 4. New smoke — `scripts/chart.smoke.{jsx,sh}` + `make chart-ui-smoke`

Drives the real `<App/>` in jsdom and asserts the chart DoD:

1. `<polyline>` with all 40 fixture points + ≥3 gridlines + ≥3 y ticks +
   ≥2 x ticks + area + last dot;
2. stats row with unit-aware `now/min/max` + `40 samples · 24h · 600s buckets`;
3. picker groups via `optgroup[label='CPU']`, option contract intact
   (`cpu.utilization_percent`), human title "CPU utilization" + raw name;
4. live/updated affordance;
5. hover (clientX=380 → mid-plot) shows crosshair + hover dot + tooltip with
   human label, `%` value, relative time; crosshair x inside the plot area;
6. mouseout clears the tooltip (React's synthetic `onMouseLeave` derives from
   native `mouseout` — that is the event the smoke dispatches).

## Files changed

- `frontend/src/ui/TimeSeriesChart.jsx` — new.
- `frontend/src/views/devices/DeviceDetail.jsx` — new (extracted + readability).
- `frontend/src/Devices.jsx` — detail components removed, renders
  `<DeviceDetail>`; unused imports dropped.
- `frontend/src/styles/views/devices.css` — chart surface + command/log
  readability rules.
- `frontend/src/styles/kit.css` — formatting-only autofix churn.
- `frontend/scripts/chart.smoke.jsx`, `chart.smoke.sh` — new.
- `Makefile` — `chart-ui-smoke` target (also normalized to LF; see commit).
- `frontend/CHART-SUMMARY.md` — this file.

## Verification

- `npm run build` — green (62 modules, 256.9 kB JS / 42.1 kB CSS).
- Smoke matrix — **12/12 pass**: setup-wizard, sse, adddevice, metrics,
  commands, events, heal, webhooks, baseline, groups, export, **chart**.
- All immutable selectors preserved: `.metrics-chart polyline`,
  `.device-metrics select` (index-valued, optgroup-wrapped), `.device-commands
  .cmd-row/.cmd-detail`, `.device-export/.export-confirm`,
  `input[placeholder^="add a tag"]`, `.modal.bulk`, `.adddev-server`,
  `table.devices tbody tr:not(.detail-row)`.

## Notes

- The chart's hover/tooltip is pointer-only (no touch/keyboard variant yet) —
  acceptable for the v1 operator console; the stats row carries the numbers
  anyway.
- The server-side baseline expected-range band (the plan's optional stretch)
  is NOT included — no server change in this phase.
