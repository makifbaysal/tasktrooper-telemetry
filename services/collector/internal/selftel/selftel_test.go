package selftel

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func TestConfigEnv(t *testing.T) {
	cases := []struct {
		name                          string
		env                           map[string]string
		wantService, wantEnv, wantVer string
	}{
		{"defaults", nil, "tt-trooper-agent", "development", "1.2.3"},
		{"TT_ENV", map[string]string{"TT_ENV": "production"}, "tt-trooper-agent", "production", "1.2.3"},
		{"TROOPER_ENV wins", map[string]string{"TT_ENV": "production", "TROOPER_ENV": "staging"}, "tt-trooper-agent", "staging", "1.2.3"},
		{"TROOPER_SERVICE", map[string]string{"TROOPER_SERVICE": "agent-eu"}, "agent-eu", "development", "1.2.3"},
		{"OTEL_SERVICE_NAME", map[string]string{"OTEL_SERVICE_NAME": "otel-name", "TROOPER_SERVICE": "agent-eu"}, "otel-name", "development", "1.2.3"},
		{"TROOPER_VERSION", map[string]string{"TROOPER_VERSION": "abc"}, "tt-trooper-agent", "development", "abc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, k := range []string{"TT_ENV", "TROOPER_ENV", "TROOPER_SERVICE", "OTEL_SERVICE_NAME", "TROOPER_VERSION"} {
				t.Setenv(k, c.env[k])
			}
			cfg := Config("tt-trooper-agent", "1.2.3")
			if cfg.Service != c.wantService || cfg.Environment != c.wantEnv || cfg.Version != c.wantVer {
				t.Errorf("got %q/%q/%q, want %q/%q/%q", cfg.Service, cfg.Environment, cfg.Version, c.wantService, c.wantEnv, c.wantVer)
			}
		})
	}
}

func TestDisabledWithoutDSN(t *testing.T) {
	t.Setenv("TROOPER_DSN", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	if Enabled() {
		t.Fatal("enabled without DSN")
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if got := Logger(log, "tt-trooper-agent"); got != log {
		t.Error("logger must be unchanged without DSN")
	}
	Start(context.Background(), "tt-trooper-agent", "dev", log)() // no-op, must not panic
}
