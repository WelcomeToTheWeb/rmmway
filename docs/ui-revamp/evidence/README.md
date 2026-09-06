# UI revamp — before/after evidence

Headless-Chromium screenshots of the real built frontend (Vite `dist/`) running
against a deterministic fake API (`rig/serve.js`), at the same fixture data, so
before and after are an apples-to-apples comparison.

- **before** = commit `347d1ea` (the UI-REVAMP.md baseline: pre-Phase 0)
- **after**  = the sign-off commit (Phases 0–4)

## Images

| File | Shows |
| --- | --- |
| `before-detail-dark.png` | Old device detail: dev-task copy ("Client export (D-6)", "Tags (B-2)", "Commands (D-1…)", "…(W6-1…)"), bare polyline chart with 2 y labels / 2 x labels, no gridlines or hover |
| `after-detail-dark.png` | Revamped device detail: clean copy, gridlines + 5 y ticks + human time ticks, area fill, "CPU utilization" human title + raw name, live · updated affordance, now/min/max/avg/p95 stats, agent log with level badges + dimmed attributes, command status pills |
| `before-topbar-dark.png` | Old topbar (plain nav, raw health chip) |
| `after-topbar-dark.png` | Revamped topbar: version chip from `/healthz`, FLEET / OPS / SYSTEM nav groups, alerts badge, "all services ok" health chip, theme toggle |
| `*-page-*.png` | Full device page (table + expanded detail) |
| `*-*-light.png` | Same surfaces in the light theme |

## Regenerating

```bash
# 0) one-time: playwright + chromium (any dir, e.g. /tmp/pw)
cd /tmp/pw && npm i playwright && npx playwright install chromium
#    headless Linux boxes without the system libs: apt-get download
#    libnss3 libnspr4 libxdamage1 libxres1 libatk1.0-0t64 libatk-bridge2.0-0t64
#    libatspi2.0-0t64 libasound2t64, dpkg -x each into a dir, and
#    LD_LIBRARY_PATH=<that dir>/usr/lib/x86_64-linux-gnu

# 1) builds
git worktree add /tmp/pw/before 347d1ea
ln -s $(pwd)/frontend/node_modules /tmp/pw/before/frontend/node_modules
(cd /tmp/pw/before/frontend && npm run build)
(cd frontend && npm run build)

# 2) fake-API servers
DIST=/tmp/pw/before/frontend/dist PORT=8123 node rig/serve.js &
DIST=$PWD/frontend/dist PORT=8124 node rig/serve.js &

# 3) screenshots (dark + light, topbar + detail + full page)
LD_LIBRARY_PATH=... node rig/shoot.js http://127.0.0.1:8123/ evidence/ before
LD_LIBRARY_PATH=... node rig/shoot.js http://127.0.0.1:8124/ evidence/ after
```

`rig/serve.js` serves any built `dist/` plus a fake RMMWay API (devices,
bucketed metrics series, commands, agent log events, alerts, healthz with a
version). `rig/shoot.js` signs in, expands the first device, waits for the
chart, and captures topbar / detail / full page in both themes.
