module github.com/tasktrooper/telemetry/tools/demo

go 1.26.3

require (
	github.com/go-sql-driver/mysql v1.10.1
	github.com/jackc/pgx/v5 v5.11.0
	github.com/rabbitmq/amqp091-go v1.16.0
	github.com/redis/go-redis/v9 v9.23.0
	github.com/tasktrooper/telemetry-go v0.0.0
	github.com/tasktrooper/telemetry-go/integrations/amqp091 v0.0.0
	github.com/tasktrooper/telemetry-go/integrations/franzgo v0.0.0
	github.com/tasktrooper/telemetry-go/integrations/goredis v0.0.0
	github.com/tasktrooper/telemetry-go/integrations/nethttp v0.0.0
	github.com/tasktrooper/telemetry-go/integrations/pgx v0.0.0
	github.com/tasktrooper/telemetry-go/integrations/slog v0.0.0
	github.com/tasktrooper/telemetry-go/integrations/sql v0.0.0
	github.com/twmb/franz-go v1.22.2
	github.com/twmb/franz-go/pkg/kadm v1.19.0
	go.opentelemetry.io/otel v1.47.0
	go.opentelemetry.io/proto/otlp v1.11.1
	google.golang.org/protobuf v1.36.12
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/XSAM/otelsql v0.44.0 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/exaring/otelpgx v0.13.0 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.31.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.20.3 // indirect
	github.com/pierrec/lz4/v4 v4.1.33 // indirect
	github.com/redis/go-redis/extra/rediscmd/v9 v9.23.0 // indirect
	github.com/redis/go-redis/extra/redisotel/v9 v9.23.0 // indirect
	github.com/twmb/franz-go/pkg/kmsg v1.15.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/bridges/otelslog v0.21.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.72.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/runtime v0.72.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp v0.23.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/sdk v1.47.0 // indirect
	go.opentelemetry.io/otel/sdk/log v1.47.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	go.uber.org/atomic v1.12.0 // indirect
	golang.org/x/net v0.61.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260928230214-8a89bd6388cc // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260928230214-8a89bd6388cc // indirect
	google.golang.org/grpc v1.84.0 // indirect
)

replace (
	github.com/tasktrooper/telemetry-go => ../../sdks/go
	github.com/tasktrooper/telemetry-go/integrations/amqp091 => ../../sdks/go/integrations/amqp091
	github.com/tasktrooper/telemetry-go/integrations/franzgo => ../../sdks/go/integrations/franzgo
	github.com/tasktrooper/telemetry-go/integrations/goredis => ../../sdks/go/integrations/goredis
	github.com/tasktrooper/telemetry-go/integrations/nethttp => ../../sdks/go/integrations/nethttp
	github.com/tasktrooper/telemetry-go/integrations/pgx => ../../sdks/go/integrations/pgx
	github.com/tasktrooper/telemetry-go/integrations/slog => ../../sdks/go/integrations/slog
	github.com/tasktrooper/telemetry-go/integrations/sql => ../../sdks/go/integrations/sql
)
