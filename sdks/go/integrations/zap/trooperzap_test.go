package trooperzap_test

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	trooperzap "github.com/tasktrooper/telemetry-go/integrations/zap"
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

func TestTee(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	spanCtx, span := tp.Tracer("x").Start(context.Background(), "op")
	defer span.End()
	sc := span.SpanContext()

	tests := []struct {
		name      string
		fields    []zap.Field
		wantTrace bool
	}{
		{"with context", []zap.Field{trooperzap.Context(spanCtx), zap.String("k", "v")}, true},
		{"explicit trace fields", append(trooperzap.TraceFields(spanCtx), zap.String("k", "v")), true},
		{"no context", []zap.Field{zap.String("k", "v")}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exp := &memExporter{}
			prev := global.GetLoggerProvider()
			global.SetLoggerProvider(sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp))))
			t.Cleanup(func() { global.SetLoggerProvider(prev) })

			var buf bytes.Buffer
			local := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(&buf), zap.InfoLevel)
			logger := zap.New(trooperzap.Tee(local, "test"))
			logger.Info("hello", tt.fields...)

			var line map[string]any
			if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
				t.Fatalf("local output: %v (%q)", err, buf.String())
			}
			if _, ok := line["context"]; ok {
				t.Errorf("raw context field leaked to local output: %v", line)
			}
			if tt.wantTrace != (line["trace_id"] == sc.TraceID().String() && line["span_id"] == sc.SpanID().String()) {
				t.Errorf("trace fields: %v", line)
			}
			if len(exp.recs) != 1 {
				t.Fatalf("otel records = %d", len(exp.recs))
			}
			if tt.name == "with context" && exp.recs[0].TraceID() != sc.TraceID() {
				t.Errorf("otel record trace id = %s", exp.recs[0].TraceID())
			}
		})
	}
	if trooperzap.TraceFields(context.Background()) != nil {
		t.Error("TraceFields without span should be nil")
	}
}
