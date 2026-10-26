package trooperredis_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
	trooperredis "github.com/tasktrooper/telemetry-go/integrations/goredis"
	"github.com/tasktrooper/telemetry-go/troopertest"
)

func TestInstrument(t *testing.T) {
	tests := []struct {
		name string
		run  func(ctx context.Context, rdb *redis.Client)
	}{
		{"set", func(ctx context.Context, rdb *redis.Client) { rdb.Set(ctx, "user:42", "secret-value", 0) }},
		{"pipeline", func(ctx context.Context, rdb *redis.Client) {
			p := rdb.Pipeline()
			p.Set(ctx, "user:42", "secret-value", 0)
			_, _ = p.Exec(ctx)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := troopertest.New(t, "svc")
			// Nothing listens on port 1: commands fail fast but spans are still recorded.
			rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
			defer rdb.Close()
			if err := trooperredis.Instrument(rdb, trooperredis.WithNamespace("0"), trooperredis.WithoutMetrics()); err != nil {
				t.Fatal(err)
			}
			ctx, srv := otel.Tracer("x").Start(context.Background(), "srv", trace.WithSpanKind(trace.SpanKindServer))
			ctx, _ = trooper.BeginEntry(ctx, "POST", "/users/:id", nil)
			tt.run(ctx, rdb)
			srv.End()

			var cmd bool
			for _, s := range h.Spans() {
				if s.SpanKind() != trace.SpanKindClient {
					continue
				}
				if s.Name() == "redis.dial" {
					continue
				}
				cmd = true
				for k, want := range map[attribute.Key]string{
					"db.system":           "redis",
					"db.namespace":        "0",
					"server.address":      "127.0.0.1",
					"trooper.entry.route": "/users/:id",
				} {
					if got := troopertest.AttrString(s, k); got != want {
						t.Errorf("%s: %s = %q, want %q", s.Name(), k, got, want)
					}
				}
				for _, kv := range s.Attributes() {
					if v := kv.Value.Emit(); kv.Key == "db.statement" || strings.Contains(v, "secret-value") {
						t.Errorf("command text recorded: %s=%s", kv.Key, v)
					}
				}
			}
			if !cmd {
				t.Fatalf("no command span; got %d spans", len(h.Spans()))
			}
		})
	}
}
