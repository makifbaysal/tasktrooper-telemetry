package emit

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
)

// LogOptions configure the OTLP log exporter (cluster mode Warning events).
type LogOptions struct {
	// LogsURL is the full OTLP/HTTP endpoint (…/v1/logs).
	LogsURL string
	Key     string
	// Resource attributes (service.name is set by the caller: k8s).
	Resource []attribute.KeyValue
	// Exporter overrides the OTLP exporter (tests); records are then exported synchronously.
	Exporter sdklog.Exporter
}

// LogRecord is one log line.
type LogRecord struct {
	Time  time.Time
	Body  string
	Attrs []attribute.KeyValue
}

// LogEmitter ships log records as OTLP logs (batched, async).
type LogEmitter struct {
	provider *sdklog.LoggerProvider
	logger   otellog.Logger
}

// NewLogEmitter builds the logger provider.
func NewLogEmitter(ctx context.Context, o LogOptions) (*LogEmitter, error) {
	var proc sdklog.Processor
	if o.Exporter != nil {
		proc = sdklog.NewSimpleProcessor(o.Exporter)
	} else {
		exp, err := otlploghttp.New(ctx,
			otlploghttp.WithEndpointURL(o.LogsURL),
			otlploghttp.WithHeaders(map[string]string{"x-trooper-key": o.Key}),
			otlploghttp.WithCompression(otlploghttp.GzipCompression),
			otlploghttp.WithRetry(otlploghttp.RetryConfig{
				Enabled: true, InitialInterval: time.Second, MaxInterval: 60 * time.Second, MaxElapsedTime: 2 * time.Minute,
			}),
		)
		if err != nil {
			return nil, fmt.Errorf("otlp log exporter: %w", err)
		}
		proc = sdklog.NewBatchProcessor(exp, sdklog.WithExportInterval(5*time.Second))
	}
	res := resource.NewSchemaless(o.Resource...)
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(proc), sdklog.WithResource(res))
	return &LogEmitter{provider: lp, logger: lp.Logger("github.com/tasktrooper/telemetry/services/collector")}, nil
}

// Warn emits a warn-level record.
func (l *LogEmitter) Warn(ctx context.Context, r LogRecord) {
	var rec otellog.Record
	rec.SetTimestamp(r.Time)
	rec.SetObservedTimestamp(time.Now())
	rec.SetSeverity(otellog.SeverityWarn)
	rec.SetSeverityText("warn")
	rec.SetBody(attribute.StringValue(r.Body))
	rec.AddAttributes(r.Attrs...)
	l.logger.Emit(ctx, rec)
}

// Flush exports pending records.
func (l *LogEmitter) Flush(ctx context.Context) error { return l.provider.ForceFlush(ctx) }

// Shutdown flushes and stops.
func (l *LogEmitter) Shutdown(ctx context.Context) error { return l.provider.Shutdown(ctx) }
