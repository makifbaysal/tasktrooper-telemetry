package selfobs_test

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/tasktrooper/telemetry/services/ingest/internal/selfobs"
	"github.com/tasktrooper/telemetry/services/ingest/internal/selfobs/selfobstest"
)

func TestBeginLoopGuard(t *testing.T) {
	tests := []struct {
		name       string
		self       uint32
		nilTel     bool
		project    uint32
		rpc        bool
		wantSpans  []string // ended spans, server span last
		suppressed bool
	}{
		{name: "customer project", self: 2, project: 9, wantSpans: []string{"child", "send tt.otlp.traces", "POST /v1/traces"}},
		{name: "self project", self: 2, project: 2, suppressed: true},
		{name: "guard off traces self project", self: 0, project: 2, wantSpans: []string{"child", "send tt.otlp.traces", "POST /v1/traces"}},
		{name: "grpc customer", self: 2, project: 9, rpc: true,
			wantSpans: []string{"child", "send tt.otlp.traces", "opentelemetry.proto.collector.trace.v1.TraceService/Export"}},
		{name: "grpc self", self: 2, project: 2, rpc: true, suppressed: true},
		{name: "telemetry off", nilTel: true, project: 9, wantSpans: []string{"child"}}, // only the test's own root span,
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := selfobstest.Install(t)
			var tel *selfobs.Tel
			if !tt.nilTel {
				tel = selfobs.New(nil, tt.self)
			}
			method, route := "POST", "/v1/traces"
			if tt.rpc {
				method, route = "", "/opentelemetry.proto.collector.trace.v1.TraceService/Export"
			}
			ctx, req := tel.WithRequest(context.Background(), method, route, tt.rpc, time.Now())
			ctx = selfobs.Begin(ctx, 7, tt.project)
			if got := selfobs.Suppressed(ctx); got != tt.suppressed {
				t.Fatalf("Suppressed = %v, want %v", got, tt.suppressed)
			}
			// Downstream instrumentation (pgx, go-redis, producer) starts children from ctx.
			_, child := otel.Tracer("test").Start(ctx, "child", trace.WithSpanKind(trace.SpanKindClient))
			child.End()
			if _, p := selfobs.ProducerSpan(ctx, "tt.otlp.traces"); p != nil {
				p.End()
			}
			req.End(200, 0, nil)
			if got := rec.SpanNames(); !slices.Equal(got, tt.wantSpans) {
				t.Fatalf("spans = %q, want %q", got, tt.wantSpans)
			}
			if len(tt.wantSpans) < 3 {
				return
			}
			server := rec.Spans.Ended()[len(tt.wantSpans)-1]
			if server.SpanKind() != trace.SpanKindServer || server.Parent().IsValid() {
				t.Fatalf("server span kind %v parent %v", server.SpanKind(), server.Parent())
			}
			attrs := map[string]string{}
			for _, kv := range server.Attributes() {
				attrs[string(kv.Key)] = kv.Value.Emit()
			}
			if attrs["tt.project_id"] != "9" && attrs["tt.project_id"] != "2" || attrs["trooper.route"] == "" {
				t.Fatalf("server attrs %v", attrs)
			}
		})
	}
}

func TestBeginWithoutPendingRequest(t *testing.T) {
	rec := selfobstest.Install(t)
	ctx := selfobs.Begin(context.Background(), 7, 2)
	if selfobs.Suppressed(ctx) || len(rec.Spans.Ended()) != 0 {
		t.Fatal("Begin without WithRequest must be a no-op")
	}
}

func TestStartFlushLoopGuard(t *testing.T) {
	tests := []struct {
		name      string
		self      uint32
		stats     selfobs.FlushStats
		wantSpans []string
	}{
		{name: "only self records", self: 2, stats: selfobs.FlushStats{Records: 3, Self: 3}},
		{name: "mixed", self: 2, stats: selfobs.FlushStats{Records: 3, Self: 1}, wantSpans: []string{"INSERT spans", "process flush"}},
		{name: "no self records", self: 2, stats: selfobs.FlushStats{Records: 3}, wantSpans: []string{"INSERT spans", "process flush"}},
		{name: "guard off", self: 0, stats: selfobs.FlushStats{Records: 3}, wantSpans: []string{"INSERT spans", "process flush"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := selfobstest.Install(t)
			tel := selfobs.New(nil, tt.self)
			ctx, span := tel.StartFlush(context.Background(), "tt-ingest-writer", tt.stats)
			ictx, ins := selfobs.InsertSpan(ctx, "spans", 10)
			_ = ictx
			selfobs.EndSpan(ins, nil)
			span.End(nil)
			if got := rec.SpanNames(); !slices.Equal(got, tt.wantSpans) {
				t.Fatalf("spans = %q, want %q", got, tt.wantSpans)
			}
			if len(tt.wantSpans) == 0 && !selfobs.Suppressed(ctx) {
				t.Fatal("self-only flush context must be suppressed")
			}
		})
	}
	// nil Tel: no spans, ctx unchanged.
	rec := selfobstest.Install(t)
	var tel *selfobs.Tel
	ctx, span := tel.StartFlush(context.Background(), "g", selfobs.FlushStats{Records: 1})
	if _, ins := selfobs.InsertSpan(ctx, "spans", 1); span != nil || ins != nil || len(rec.Spans.Ended()) != 0 {
		t.Fatal("nil Tel must not trace")
	}
}

func TestIsSelf(t *testing.T) {
	tests := []struct {
		self    uint32
		project uint32
		header  string
		want    bool
	}{
		{self: 2, project: 2, header: "2", want: true},
		{self: 2, project: 12, header: "12"},
		{self: 12, project: 1, header: "1"},
		{self: 0, project: 0, header: "0"},
	}
	for _, tt := range tests {
		tel := selfobs.New(nil, tt.self)
		if got := tel.IsSelf(tt.project); got != tt.want {
			t.Errorf("IsSelf(%d) self=%d = %v", tt.project, tt.self, got)
		}
		if got := tel.IsSelfHeader([]byte(tt.header)); got != tt.want {
			t.Errorf("IsSelfHeader(%q) self=%d = %v", tt.header, tt.self, got)
		}
	}
	var nilTel *selfobs.Tel
	if nilTel.IsSelf(2) || nilTel.IsSelfHeader([]byte("2")) {
		t.Error("nil Tel")
	}
}

func TestLogHandlerLoopGuard(t *testing.T) {
	tests := []struct {
		name     string
		ctx      func() context.Context
		level    slog.Level
		wantOTel bool
	}{
		{name: "normal", ctx: context.Background, level: slog.LevelInfo, wantOTel: true},
		{name: "suppressed", ctx: func() context.Context { return selfobs.Suppress(context.Background()) }, level: slog.LevelError},
		{name: "below local level", ctx: context.Background, level: slog.LevelDebug},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := selfobstest.Install(t)
			var local bytes.Buffer
			log := slog.New(selfobs.LogHandler(slog.NewJSONHandler(&local, nil), "tt-test")).With("service", "x")
			log.Log(tt.ctx(), tt.level, "hello")
			wantLocal := tt.level >= slog.LevelInfo
			if got := strings.Contains(local.String(), `"msg":"hello"`); got != wantLocal {
				t.Fatalf("local = %q, want written %v", local.String(), wantLocal)
			}
			if strings.Contains(local.String(), "5e1f5e1f") {
				t.Fatalf("suppression marker leaked into local log: %s", local.String())
			}
			if got := slices.Contains(rec.Logs.Bodies(), "hello"); got != tt.wantOTel {
				t.Fatalf("otel logs = %q, want exported %v", rec.Logs.Bodies(), tt.wantOTel)
			}
		})
	}
}

func TestSelfProjectFromEnv(t *testing.T) {
	for in, want := range map[string]uint32{"": 0, "2": 2, " 17 ": 17, "x": 0, "-1": 0} {
		t.Setenv("TT_SELF_PROJECT_ID", in)
		if got := selfobs.SelfProjectFromEnv(); got != want {
			t.Errorf("%q → %d, want %d", in, got, want)
		}
	}
}

func TestConfigured(t *testing.T) {
	tests := []struct {
		dsn, otel, enabled string
		want               bool
	}{
		{},
		{dsn: "http://k@localhost:4318/2", want: true},
		{otel: "http://localhost:4318", want: true},
		{dsn: "http://k@localhost:4318/2", enabled: "false"},
	}
	for _, tt := range tests {
		t.Setenv("TROOPER_DSN", tt.dsn)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", tt.otel)
		t.Setenv("TROOPER_ENABLED", tt.enabled)
		if got := selfobs.Configured(); got != tt.want {
			t.Errorf("%+v → %v", tt, got)
		}
	}
}
