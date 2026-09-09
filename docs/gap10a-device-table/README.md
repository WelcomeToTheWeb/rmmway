# Device-table ergonomics — gap #10a evidence (wave 2, lane C)

Headless-Chromium screenshots of the built frontend (`frontend/dist`)
running against the shared fake-API rig (`docs/ui-revamp/rig/serve.js`)
at the same fixture data as the UI-revamp evidence, so the views are
apples-to-apples comparable. This folder is self-contained; no file
under `docs/ui-revamp/` was modified except an **additive** rig
extension (see below).

## What each shot shows

| File | Shows |
| --- | --- |
| `desktop-1280-default.png` | Default view: new **Client** column (client_id → name via `/api/clients`, "Unassigned" for null), the **client filter** dropdown in the toolbar, **primary-IP cells** (`10.0.9.3 +2` on the multi-interface device) |
| `desktop-1280-stateful.png` | The same view restored from the URL `#/devices?sort=host:desc&hidden=tags`: **sortable header** with the active direction (HOST ▼), **Tags column hidden**, the multi-IP row **expanded** (10.0.9.3 / 10.0.9.4 / fe80::1), and the **columns show/hide menu** open (checklist, Tags dimmed) |
| `desktop-1280-client-scoped.png` | Client filter set to Acme Corp — the list AND the counts come from the server (`GET /api/devices?client=clt-acme`): "2 in Acme Corp · 2 online · 0 offline" |
| `mobile-390-default.png` | 390px viewport: the table is wider than the viewport and the page scrolls horizontally (pre-existing pattern; a mobile/tablet table layout is out of scope for this gap) |

## URL-state shape (the shareable view state)

```text
#/devices?sort=<key>:<asc|desc>&hidden=<key>[,<key>...]&client=<client-id>
```

- `sort` — `host | client | os | agent | ips | status` (each step of the
  header cycle natural → asc → desc → natural is one URL)
- `hidden` — any subset of the hideable columns (`host` never hideable);
  canonical column order when written back
- `client` — client id; absent = all clients (server-scoped)

The hash still starts with `#/devices`, so the app's existing hash router
is untouched; state is written back on every change (normalizing the URL)
and re-read on `hashchange`, so back/forward walks table states and a
copied URL restores the exact view.

## Regenerating

```bash
cd frontend && npm run build
cd docs/gap10a-device-table && node shot.mjs
# (playwright + chromium must be installed once, e.g. in /tmp/pw:
#  cd /tmp/pw && npm i playwright && npx playwright install chromium)
```

## Rig extension (additive)

`docs/ui-revamp/rig/serve.js` gained, without touching existing behavior:

- `client_id` on the four device fixtures (Acme Corp ×2, Globex ×1,
  unassigned ×1) and a 3rd IP on `dev-gw03` so the expand control is
  exercised
- a `CLIENTS` fixture + `GET /api/clients` route
- `?client=` scoping on `GET /api/devices` (`unassigned` matches null)
