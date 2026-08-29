# RMMWay frontend (A-1). Multi-stage: build the SPA with node, serve it with
# a minimal nonroot Caddy. The image serves the STATIC SPA and also
# reverse-proxies /api, /agent and /healthz to the Go server (see
# deploy/Caddyfile.frontend) so bring-your-own-reverse-proxy mode works when a
# self-hoster points their whole proxy at this one port. In the bundled Caddy
# edge the proxy forwards /api/* to the server directly, so the browser only
# ever talks to the one TLS host either way.
FROM node:22-alpine AS build
WORKDIR /app
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM caddy:2.11-alpine
COPY --from=build /app/dist /srv/rmmway
COPY deploy/Caddyfile.frontend /etc/caddy/Caddyfile
EXPOSE 8080
