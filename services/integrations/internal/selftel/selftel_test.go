package selftel

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
)

func TestDisabledIsNoop(t *testing.T) {
	t.Setenv("TROOPER_DSN", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("TT_DSN", "http://k@localhost:4318/1") // the cloud-metrics DSN never enables self telemetry
	before := otel.GetTracerProvider()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	Start(context.Background(), "v1", log)()
	if otel.GetTracerProvider() != before {
		t.Error("Start without DSN replaced the global TracerProvider")
	}
	if Logger(log) != log || Transport() != nil || Client(time.Second) != nil {
		t.Error("helpers without DSN must leave defaults untouched")
	}
}

func TestConfig(t *testing.T) {
	tests := []struct {
		name                              string
		env                               map[string]string
		wantService, wantEnv, wantVersion string
	}{
		{"defaults", nil, Service, "development", "dev"},
		{"tt env", map[string]string{"TT_ENV": "production"}, Service, "production", "dev"},
		{"overrides", map[string]string{"TROOPER_SERVICE": "cloud-b", "TROOPER_ENV": "staging", "TROOPER_VERSION": "abc"}, "cloud-b", "staging", "abc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"OTEL_SERVICE_NAME", "TROOPER_SERVICE", "TROOPER_ENV", "TT_ENV", "TROOPER_VERSION"} {
				t.Setenv(k, tc.env[k])
			}
			if c := Config("dev"); c.Service != tc.wantService || c.Environment != tc.wantEnv || c.Version != tc.wantVersion {
				t.Errorf("got %q/%q/%q", c.Service, c.Environment, c.Version)
			}
		})
	}
}
