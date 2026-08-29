module github.com/welcometotheweb/rmmway/server

go 1.24.9

require (
	github.com/jackc/pgx/v5 v5.7.5
	github.com/nats-io/nats.go v1.42.0
	github.com/redis/go-redis/v9 v9.11.0
	google.golang.org/grpc v1.77.0
	google.golang.org/protobuf v1.36.12 // indirect
)

require (
	github.com/golang-jwt/jwt/v5 v5.3.0
	github.com/parquet-go/parquet-go v0.32.0
	github.com/welcometotheweb/rmmway/proto/gen v0.0.0
	golang.org/x/crypto v0.43.0
)

require (
	github.com/andybalholm/brotli v1.1.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jedisct1/go-minisign v0.0.0-20230811132847-661be99b8267
	github.com/klauspost/compress v1.18.0 // indirect
	github.com/nats-io/nkeys v0.4.11 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	github.com/parquet-go/bitpack v1.0.0 // indirect
	github.com/parquet-go/jsonlite v1.0.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.21 // indirect
	github.com/twpayne/go-geom v1.6.1 // indirect
	golang.org/x/net v0.46.1-0.20251013234738-63d1a5100f82 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/sys v0.38.0 // indirect
	golang.org/x/text v0.30.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251022142026-3a174f9686a8 // indirect
)

replace github.com/welcometotheweb/rmmway/proto/gen => ../proto/gen
