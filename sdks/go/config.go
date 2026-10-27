package trooper

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config configures Start. Zero values are filled from the environment, then defaults.
//
// Precedence: explicit field > OTEL_* env (when set) > TROOPER_* env > default (.ai/contracts.md §2).
type Config struct {
	// DSN: https://<ingest-key>@<host>[:port]/<project_id>. Env: TROOPER_DSN.
	// OTEL_EXPORTER_OTLP_ENDPOINT, when set, overrides the DSN endpoint (the DSN key is still sent);
	// without a DSN, OTEL_EXPORTER_OTLP_HEADERS supplies auth.
	// If both are empty the SDK runs without exporting (propagation still works).
	DSN string
	// Service is service.name. Env: OTEL_SERVICE_NAME, TROOPER_SERVICE.
	Service string
	// Environment is deployment.environment.name. Env: TROOPER_ENV.
	Environment string
	// Version is service.version (deploy marker). Env: TROOPER_VERSION.
	Version string
	// SampleRate is the head sampling probability in (0, 1]. 0 means "unset" (env or 1.0).
	// Env: OTEL_TRACES_SAMPLER_ARG, TROOPER_SAMPLE_RATE.
	SampleRate float64
	// Debug logs every internal SDK error to stderr. Env: TROOPER_DEBUG=true.
	Debug bool
	// Disabled turns the SDK off: no providers are installed (propagation still works).
	// Env: TROOPER_ENABLED=false.
	Disabled bool

	// ResourceAttributes are extra resource attributes (low cardinality only).
	ResourceAttributes map[string]string

	DisableTraces         bool
	DisableMetrics        bool
	DisableLogs           bool
	DisableRuntimeMetrics bool

	// MetricInterval is the metric export interval. Default 30s.
	MetricInterval time.Duration
	// ExportTimeout bounds a single export call including retries. Default 30s.
	ExportTimeout time.Duration
	// BatchTimeout is the maximum delay before a span/log batch is exported. Default 5s.
	BatchTimeout time.Duration
	// MaxQueueSize bounds the span/log queues; items beyond it are dropped (never blocks). Default 2048.
	MaxQueueSize int

	// HTTPClient overrides the HTTP client used by exporters (its Transport is wrapped).
	HTTPClient *http.Client

	// Profiling configures continuous profiling (off by default). See WithProfiling.
	Profiling ProfilingConfig
}

// Option modifies the Config passed to Start (e.g. WithProfiling). Options are applied after the
// Config fields, before environment defaults.
type Option func(*Config)

func (c Config) withDefaults() Config {
	c.DSN = first(c.DSN, os.Getenv("TROOPER_DSN"))
	// Contract: explicitly set OTEL_* variables win over TROOPER_*.
	c.Service = first(c.Service, os.Getenv("OTEL_SERVICE_NAME"), os.Getenv("TROOPER_SERVICE"))
	c.Environment = first(c.Environment, os.Getenv("TROOPER_ENV"))
	c.Version = first(c.Version, os.Getenv("TROOPER_VERSION"))
	if !c.Debug {
		c.Debug, _ = strconv.ParseBool(os.Getenv("TROOPER_DEBUG"))
	}
	if c.SampleRate <= 0 {
		c.SampleRate = parseRate(os.Getenv("OTEL_TRACES_SAMPLER_ARG"))
	}
	if c.SampleRate <= 0 {
		c.SampleRate = parseRate(os.Getenv("TROOPER_SAMPLE_RATE"))
	}
	if !c.Disabled {
		if v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("TROOPER_ENABLED"))); err == nil && !v {
			c.Disabled = true
		}
	}
	if c.SampleRate <= 0 || c.SampleRate > 1 {
		c.SampleRate = 1
	}
	if c.MetricInterval <= 0 {
		c.MetricInterval = 30 * time.Second
	}
	if c.ExportTimeout <= 0 {
		c.ExportTimeout = 30 * time.Second
	}
	if c.BatchTimeout <= 0 {
		c.BatchTimeout = 5 * time.Second
	}
	if c.MaxQueueSize <= 0 {
		c.MaxQueueSize = 2048
	}
	c.Profiling = c.Profiling.withDefaults()
	return c
}

func parseRate(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f <= 0 || f > 1 {
		return 0
	}
	return f
}

func first(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
