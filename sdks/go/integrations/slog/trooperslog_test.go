package trooperslog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	trooperslog "github.com/tasktrooper/telemetry-go/integrations/slog"
)

type memExporter struct {
	mu   sync.Mutex
	recs []sdklog.Record
}

func (e *memExporter) Export(_ context.Context, rs []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range rs {
		e.recs = append(e.recs, r.Clone())
	}
	return nil
}
func (e *memExporter) Shutdown(context.Context) error   { return nil }
func (e *memExporter) ForceFlush(context.Context) error { return nil }

func setup(t *testing.T) *memExporter {
	exp := &memExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	prev := global.GetLoggerProvider()
	global.SetLoggerProvider(lp)
	t.Cleanup(func() { global.SetLoggerProvider(prev) })
	return exp
}

func TestHandler(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("x").Start(context.Background(), "op")
	defer span.End()
	sc := span.SpanContext()

	tests := []struct {
		name      string
		ctx       context.Context
		level     slog.Level
		wantOtel  bool
		wantTrace bool
	}{
		{"info with span", ctx, slog.LevelInfo, true, true},
		{"info without span", context.Background(), slog.LevelInfo, true, false},
		{"debug filtered from otel", ctx, slog.LevelDebug, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exp := setup(t)
			var buf bytes.Buffer
			next := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
			logger := slog.New(trooperslog.NewHandler("test", next)).With("svc", "orders")
			logger.Log(tt.ctx, tt.level, "hello", "k", "v")

			var line map[string]any
			if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
				t.Fatalf("next output: %v (%q)", err, buf.String())
			}
			if line["svc"] != "orders" || line["k"] != "v" {
				t.Errorf("attrs missing in next output: %v", line)
			}
			if tt.wantTrace {
				if line["trace_id"] != sc.TraceID().String() || line["span_id"] != sc.SpanID().String() {
					t.Errorf("trace ids missing: %v", line)
				}
			} else if _, ok := line["trace_id"]; ok {
				t.Errorf("unexpected trace_id: %v", line)
			}

			if got := len(exp.recs); (got == 1) != tt.wantOtel {
				t.Fatalf("otel records = %d, want otel=%v", got, tt.wantOtel)
			}
			if tt.wantOtel {
				r := exp.recs[0]
				if r.Body().Emit() != "hello" {
					t.Errorf("body = %q", r.Body().Emit())
				}
				wantTID := trace.TraceID{}
				if tt.wantTrace {
					wantTID = sc.TraceID()
				}
				if r.TraceID() != wantTID {
					t.Errorf("record trace id = %s, want %s", r.TraceID(), wantTID)
				}
			}
		})
	}
}

func TestNilNext(t *testing.T) {
	exp := setup(t)
	slog.New(trooperslog.NewHandler("test", nil)).Info("x")
	if len(exp.recs) != 1 {
		t.Fatalf("records = %d", len(exp.recs))
	}
}
