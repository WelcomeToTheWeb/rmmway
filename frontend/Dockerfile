# RMMWay frontend (A-1). Multi-stage: build the SPA with node, serve it with
# a minimal nonroot Caddy. The image holds STATIC files only — no API calls
# are made from this container; the edge proxy forwards /api/* to the Go
# server, so the browser only ever talks to the single TLS host.
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
