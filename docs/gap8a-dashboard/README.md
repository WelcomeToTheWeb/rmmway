# Gap #8a — Fleet dashboard home (wave 2, round 2, lane C)

The dashboard is now the **default route**: `#/` (bare hash), `#/dashboard`,
and any unknown hash all land on it. `#/devices` remains a live deep link
for shared table URLs. It is the first item of the Fleet nav group.

## What it shows

One screen, the whole fleet's health — every tile composes **existing**
endpoints client-side (no new server surface):

| Tile | Source |
| --- | --- |
| Fleet status (online/offline donut + legend) | `GET /api/devices` |
| Devices per OS (bars) | same `/api/devices` payload |
| Alerts (open/acked pills + latest open) | `GET /api/alerts/counts` + `GET /api/alerts?status=open&limit=5` |
| Top anomalies (host, metric, z-score, when) | `GET /api/baseline/anomalies?limit=5` |
| Recent activity (category dot, type, host, when) | `GET /api/events?limit=12` (journal Envelopes) |
| Patch compliance | placeholder — "arrives with wave 3 inventory (#4)" |
| Tickets | placeholder — "arrives with wave 3 ticketing (#7)" |

Devices + alert counts re-poll every 30s (a live-ish home); the rest settle
on load and refresh via the ↻ button. Every tile degrades to a muted
"unavailable" note on fetch failure, a "loading…" state while pending, and a
sensible empty message when the data set is empty — the grid never breaks.

## Screenshots (this folder)

| File | Viewport | What it proves |
| --- | --- | --- |
| `dashboard-1280.png` | 1280×800 | default route + 12-col grid, all tiles live |
| `dashboard-390.png` | 390×844 (full page) | phone: single column, centered donut, compact rows |
| `dashboard-390-tiles.png` | 390×844 | phone: anomaly/activity tiles' compact row layout |

## How the shots were made

Round-1 rig pattern: `shot.mjs` serves `frontend/dist` + the fixture API
(`docs/ui-revamp/rig/serve.js`), logs in with the rig's `admin` account,
drives the hash router, and screenshots. Playwright resolves through the
`./node_modules` symlink (`/tmp/pw/node_modules`, browsers in
`~/.cache/ms-playwright`) — the rig is evidence-only, not an app dependency.

The rig needed additive fixtures for the tiles (this round, lane C):
`/api/alerts` + `/api/alerts/counts` with status filtering,
`/api/baseline/anomalies` as a `StoredAnomaly[]` fixture, and `/api/events`
now returns the top-level `Envelope[]` journal shape that `api.js` documents
(the earlier object-shape branch was unreachable — a URL pathname never
contains `?`).

Run:

```sh
cd frontend && npm run build
cd docs/gap8a-dashboard && node shot.mjs
```
