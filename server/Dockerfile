# RMMWay server image (W0-3). Static Go binary; runs on :8080 (HTTP) and
# :50051 (gRPC agent ingest). Backing services (Postgres/Timescale, NATS,
# Redis, MinIO, Meilisearch) are external — configured via env.
FROM golang:1.24 AS build
WORKDIR /src
COPY agent/ agent/
COPY proto/ proto/
COPY server/ server/
RUN cd server && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
EXPOSE 8080 50051
ENTRYPOINT ["/server"]
