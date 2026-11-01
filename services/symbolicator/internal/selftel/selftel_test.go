package selftel

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel"
)

func TestDisabledIsNoop(t *testing.T) {
	t.Setenv("TROOPER_DSN", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	before := otel.GetTracerProvider()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	Start(context.Background(), "v1", log)()
	if otel.GetTracerProvider() != before {
		t.Error("Start without DSN replaced the global TracerProvider")
	}
	if Logger(log) != log {
		t.Error("Logger without DSN must return the logger unchanged")
	}
	if Client("s3") != nil {
		t.Error("Client without DSN must be nil (SDK default client)")
	}
}

func TestConfig(t *testing.T) {
	tests := []struct {
		name                              string
		env                               map[string]string
		wantService, wantEnv, wantVersion string
	}{
		{"defaults", nil, Service, "development", "build"},
		{"tt env", map[string]string{"TT_ENV": "production"}, Service, "production", "build"},
		{"overrides", map[string]string{"TROOPER_SERVICE": "sym-b", "TROOPER_ENV": "staging", "TROOPER_VERSION": "abc"}, "sym-b", "staging", "abc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"OTEL_SERVICE_NAME", "TROOPER_SERVICE", "TROOPER_ENV", "TT_ENV", "TROOPER_VERSION"} {
				t.Setenv(k, tc.env[k])
			}
			if c := Config("build"); c.Service != tc.wantService || c.Environment != tc.wantEnv || c.Version != tc.wantVersion {
				t.Errorf("got %q/%q/%q", c.Service, c.Environment, c.Version)
			}
		})
	}
}
