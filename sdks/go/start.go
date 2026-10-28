package trooper

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// ShutdownFunc flushes and stops all providers. It is safe to call more than once.
type ShutdownFunc func(context.Context) error

// Start configures OpenTelemetry for TaskTrooper and installs the global TracerProvider,
// MeterProvider, LoggerProvider and propagator. Call the returned function on exit to flush.
//
// Start never panics. On error a non-nil no-op shutdown function is returned and the application
// keeps running without exporting telemetry.
//
// Options (e.g. WithProfiling) are applied on top of cfg:
//
//	shutdown, err := trooper.Start(ctx, trooper.Config{Service: "orders"}, trooper.WithProfiling())
func Start(ctx context.Context, cfg Config, opts ...Option) (shutdown ShutdownFunc, err error) {
	noop := func(context.Context) error { return nil }
	defer func() {
		if r := recover(); r != nil {
			shutdown, err = noop, fmt.Errorf("trooper: start panicked: %v", r)
		}
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	cfg = cfg.withDefaults()
	if cfg.Service == "" {
		cfg.Service = "unknown_service:" + filepath.Base(os.Args[0])
	}

	otel.SetErrorHandler(&errorHandler{debug: cfg.Debug})
	otel.SetTextMapPropagator(NewPropagator(cfg.Service))

	if cfg.Disabled {
		return noop, nil
	}

	endpoint, key := "", ""
	if cfg.DSN != "" {
		d, perr := ParseDSN(cfg.DSN)
		if perr != nil {
			return noop, perr
		}
		endpoint, key = d.Endpoint, d.Key
	}
	// An explicitly set OTEL endpoint wins (contract §2).
	if e := strings.TrimRight(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"), "/"); e != "" {
		endpoint = e
	}

	res, rerr := buildResource(ctx, cfg)
	if rerr != nil && res == nil {
		return noop, rerr
	}

	tpOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(NewSampler(cfg.SampleRate)),
		sdktrace.WithSpanProcessor(NewSpanProcessor(cfg.SampleRate)),
	}
	if endpoint == "" {
		if cfg.Debug {
			fmt.Fprintln(os.Stderr, "trooper: no DSN or OTEL_EXPORTER_OTLP_ENDPOINT; telemetry is not exported")
		}
		tp := sdktrace.NewTracerProvider(tpOpts...)
		otel.SetTracerProvider(tp)
		return tp.Shutdown, nil
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{}
	} else {
		c := *client
		client = &c
	}
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	client.Transport = exportTransport{base: base}
	headers := map[string]string{}
	if key != "" {
		headers["x-trooper-key"] = key
	}

	var shutdowns []func(context.Context) error
	shutdownAll := func(ctx context.Context) error {
		var errs []error
		for i := len(shutdowns) - 1; i >= 0; i-- {
			errs = append(errs, shutdowns[i](ctx))
		}
		return errors.Join(errs...)
	}

	// Metrics first so self-metrics can be registered.
	if !cfg.DisableMetrics {
		mopts := []otlpmetrichttp.Option{
			otlpmetrichttp.WithEndpointURL(endpoint + "/v1/metrics"),
			otlpmetrichttp.WithCompression(otlpmetrichttp.GzipCompression),
			otlpmetrichttp.WithHTTPClient(client),
			otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig(retryConfig(cfg))),
		}
		if len(headers) > 0 {
			mopts = append(mopts, otlpmetrichttp.WithHeaders(headers))
		}
		mexp, merr := otlpmetrichttp.New(ctx, mopts...)
		if merr != nil {
			return noop, merr
		}
		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(res),
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(countingMetricExporter{mexp},
				sdkmetric.WithInterval(cfg.MetricInterval), sdkmetric.WithTimeout(cfg.ExportTimeout))),
		)
		otel.SetMeterProvider(mp)
		shutdowns = append(shutdowns, mp.Shutdown)
		self.register(mp.Meter(MeterName))
		if !cfg.DisableRuntimeMetrics {
			_ = runtime.Start(runtime.WithMeterProvider(mp))
		}
	}

	if !cfg.DisableTraces {
		topts := []otlptracehttp.Option{
			otlptracehttp.WithEndpointURL(endpoint + "/v1/traces"),
			otlptracehttp.WithCompression(otlptracehttp.GzipCompression),
			otlptracehttp.WithHTTPClient(client),
			otlptracehttp.WithRetry(otlptracehttp.RetryConfig(retryConfig(cfg))),
		}
		if len(headers) > 0 {
			topts = append(topts, otlptracehttp.WithHeaders(headers))
		}
		texp, terr := otlptracehttp.New(ctx, topts...)
		if terr != nil {
			_ = shutdownAll(ctx)
			return noop, terr
		}
		tp := sdktrace.NewTracerProvider(append(tpOpts, sdktrace.WithBatcher(countingSpanExporter{texp},
			sdktrace.WithMaxQueueSize(cfg.MaxQueueSize),
			sdktrace.WithBatchTimeout(cfg.BatchTimeout),
			sdktrace.WithExportTimeout(cfg.ExportTimeout),
		))...)
		otel.SetTracerProvider(tp)
		shutdowns = append(shutdowns, tp.Shutdown)
	}

	if !cfg.DisableLogs {
		lopts := []otlploghttp.Option{
			otlploghttp.WithEndpointURL(endpoint + "/v1/logs"),
			otlploghttp.WithCompression(otlploghttp.GzipCompression),
			otlploghttp.WithHTTPClient(client),
			otlploghttp.WithRetry(otlploghttp.RetryConfig(retryConfig(cfg))),
		}
		if len(headers) > 0 {
			lopts = append(lopts, otlploghttp.WithHeaders(headers))
		}
		lexp, lerr := otlploghttp.New(ctx, lopts...)
		if lerr != nil {
			_ = shutdownAll(ctx)
			return noop, lerr
		}
		lp := sdklog.NewLoggerProvider(
			sdklog.WithResource(res),
			sdklog.WithProcessor(sdklog.NewBatchProcessor(countingLogExporter{lexp},
				sdklog.WithMaxQueueSize(cfg.MaxQueueSize),
				sdklog.WithExportInterval(cfg.BatchTimeout),
				sdklog.WithExportTimeout(cfg.ExportTimeout),
			)),
		)
		global.SetLoggerProvider(lp)
		shutdowns = append(shutdowns, lp.Shutdown)
	}

	if cfg.Profiling.Enabled {
		if p := startProfiler(cfg, client, endpoint, key); p != nil {
			shutdowns = append(shutdowns, p.shutdown)
		}
	}

	done := false
	return func(ctx context.Context) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("trooper: shutdown panicked: %v", r)
			}
		}()
		if done {
			return nil
		}
		done = true
		return shutdownAll(ctx)
	}, nil
}

type retryCfg struct {
	Enabled         bool
	InitialInterval time.Duration
	MaxInterval     time.Duration
	MaxElapsedTime  time.Duration
}

// retryConfig implements the contract backoff: exponential 1s → 60s with jitter, bounded by the
// export timeout so the batch processor never blocks for long.
func retryConfig(cfg Config) retryCfg {
	return retryCfg{Enabled: true, InitialInterval: time.Second, MaxInterval: 60 * time.Second, MaxElapsedTime: cfg.ExportTimeout}
}

func buildResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{
		attribute.String("service.name", cfg.Service),
		attribute.String("telemetry.distro.name", DistroName),
		attribute.String("telemetry.distro.version", Version),
	}
	if cfg.Version != "" {
		attrs = append(attrs, attribute.String("service.version", cfg.Version))
	}
	if cfg.Environment != "" {
		attrs = append(attrs, attribute.String("deployment.environment.name", cfg.Environment))
	}
	for k, v := range cfg.ResourceAttributes {
		attrs = append(attrs, attribute.String(k, v))
	}
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
		resource.WithOS(),
		resource.WithContainer(),
		// No command args / owner: they may contain secrets or PII.
		resource.WithProcessPID(),
		resource.WithProcessExecutableName(),
		resource.WithProcessRuntimeName(),
		resource.WithProcessRuntimeVersion(),
		resource.WithProcessRuntimeDescription(),
		resource.WithAttributes(attrs...),
	)
	if res == nil {
		res = resource.NewSchemaless(attrs...)
	}
	if errors.Is(err, resource.ErrPartialResource) {
		err = nil
	}
	return res, err
}
