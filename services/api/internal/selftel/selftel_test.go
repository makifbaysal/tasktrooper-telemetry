package selftel

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/makifbaysal/tasktrooper-telemetry/sdks/go/troopertest"

	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

func noDSN(t *testing.T) {
	t.Setenv("TROOPER_DSN", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
}

func TestDisabledIsNoop(t *testing.T) {
	noDSN(t)
	before := otel.GetTracerProvider()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	flush := Start(context.Background(), "v1", log)
	flush()
	if otel.GetTracerProvider() != before {
		t.Error("Start without DSN replaced the global TracerProvider")
	}
	if Logger(log) != log {
		t.Error("Logger without DSN must return the logger unchanged")
	}
	c := &http.Client{}
	if Client(c, "x") != c {
		t.Error("Client without DSN must return the client unchanged")
	}
	if LLMTransport() != nil {
		t.Error("LLMTransport without DSN must be nil")
	}
}

func TestConfig(t *testing.T) {
	tests := []struct {
		name                              string
		env                               map[string]string
		wantService, wantEnv, wantVersion string
	}{
		{"defaults", nil, "tt-api", "development", "build"},
		{"tt env", map[string]string{"TT_ENV": "production"}, "tt-api", "production", "build"},
		{"overrides", map[string]string{"TROOPER_SERVICE": "api-canary", "TROOPER_ENV": "staging", "TT_ENV": "production", "TROOPER_VERSION": "abc"},
			"api-canary", "staging", "abc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"OTEL_SERVICE_NAME", "TROOPER_SERVICE", "TROOPER_ENV", "TT_ENV", "TROOPER_VERSION"} {
				t.Setenv(k, tc.env[k])
			}
			c := Config("build")
			if c.Service != tc.wantService || c.Environment != tc.wantEnv || c.Version != tc.wantVersion {
				t.Errorf("got %q/%q/%q", c.Service, c.Environment, c.Version)
			}
		})
	}
}

func TestOperationAndTable(t *testing.T) {
	tests := []struct{ sql, op, table string }{
		{"SELECT count() FROM telemetry.spans WHERE org_id = 1", "SELECT", "spans"},
		{"  -- c\nselect x from red_1m r JOIN svc s ON 1", "SELECT", "red_1m"},
		{"WITH a AS (SELECT 1) SELECT * FROM (SELECT 1)", "SELECT", ""},
		{"INSERT INTO deploy_markers VALUES (1)", "INSERT", "deploy_markers"},
		{"ALTER TABLE logs DELETE WHERE 1", "ALTER", "logs"},
	}
	for _, tc := range tests {
		if op, tb := Operation(tc.sql), Table(tc.sql); op != tc.op || tb != tc.table {
			t.Errorf("%q: got %q %q, want %q %q", tc.sql, op, tb, tc.op, tc.table)
		}
	}
}

type fakeCH struct{ driver.Conn }

func (fakeCH) Exec(context.Context, string, ...any) error { return nil }

func TestClickHouseSpan(t *testing.T) {
	h := troopertest.New(t, "tt-api")
	conn := ClickHouse(fakeCH{}, "telemetry", "localhost:9000")
	ctx := tenant.With(context.Background(), tenant.Tenant{OrgID: 3, ProjectID: 7})
	if err := conn.Exec(ctx, "ALTER TABLE logs DELETE WHERE org_id = 3 AND project_id = 7 AND body = 'secret'"); err != nil {
		t.Fatal(err)
	}
	s := h.SpanByKind(trace.SpanKindClient)
	if s == nil || s.Name() != "ALTER logs" {
		t.Fatalf("span = %v", s)
	}
	want := map[string]string{"db.system": "clickhouse", "db.namespace": "telemetry", "db.collection.name": "logs",
		"db.operation.name": "ALTER", "server.address": "localhost:9000"}
	for k, v := range want {
		if got, _ := troopertest.Attr(s, attribute.Key(k)); got.AsString() != v {
			t.Errorf("%s = %q, want %q", k, got.AsString(), v)
		}
	}
	q, _ := troopertest.Attr(s, "db.query.text")
	if strings.Contains(q.AsString(), "secret") || strings.Contains(q.AsString(), "= 7") {
		t.Errorf("query not sanitized: %q", q.AsString())
	}
	if p, _ := troopertest.Attr(s, "tt.project_id"); p.AsInt64() != 7 {
		t.Errorf("tt.project_id = %d", p.AsInt64())
	}
}
