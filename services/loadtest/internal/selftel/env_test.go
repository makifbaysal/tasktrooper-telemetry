package selftel

import (
	"context"
	"maps"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestRunnerEnv(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want map[string]string
	}{
		{"off without DSN", map[string]string{"TROOPER_PROFILING": "true", "TT_ENV": "production"}, map[string]string{}},
		{"dsn + resolved env from TT_ENV",
			map[string]string{"TROOPER_DSN": "http://k@gw:4318/2", "TT_ENV": "production", "TROOPER_SERVICE": "tt-loadtest-ctl", "TROOPER_PROFILING": "true"},
			map[string]string{"TROOPER_DSN": "http://k@gw:4318/2", "TROOPER_ENV": "production", "TROOPER_PROFILING": "true"}},
		{"TROOPER_ENV wins, default development",
			map[string]string{"TROOPER_DSN": "d", "TROOPER_SAMPLE_RATE": "0.5"},
			map[string]string{"TROOPER_DSN": "d", "TROOPER_ENV": "development", "TROOPER_SAMPLE_RATE": "0.5"}},
		{"explicit TROOPER_ENV",
			map[string]string{"TROOPER_DSN": "d", "TROOPER_ENV": "staging", "TT_ENV": "production", "TROOPER_VERSION": "v1"},
			map[string]string{"TROOPER_DSN": "d", "TROOPER_ENV": "staging", "TROOPER_VERSION": "v1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RunnerEnv(func(k string) string { return c.env[k] })
			if !maps.Equal(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestTraceEnvRoundTrip(t *testing.T) {
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ts, _ := trace.ParseTraceState("tt=r:0.5")
	cases := []struct {
		name    string
		sc      trace.SpanContext
		wantEnv int
	}{
		{"sampled with tracestate", trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled, TraceState: ts}), 2},
		{"sampled", trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}), 1},
		{"no span", trace.SpanContext{}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := TraceEnv(trace.ContextWithSpanContext(context.Background(), c.sc))
			if len(env) != c.wantEnv {
				t.Fatalf("env %v, want %d keys", env, c.wantEnv)
			}
			got := trace.SpanContextFromContext(FromEnv(context.Background(), func(k string) string { return env[k] }))
			if c.wantEnv == 0 {
				if got.IsValid() {
					t.Fatalf("unexpected parent %v", got)
				}
				return
			}
			if got.TraceID() != tid || got.SpanID() != sid || !got.IsSampled() || !got.IsRemote() {
				t.Fatalf("round trip lost the parent: %v", got)
			}
			if got.TraceState().String() != c.sc.TraceState().String() {
				t.Fatalf("tracestate %q, want %q", got.TraceState(), c.sc.TraceState())
			}
		})
	}
}

func TestFromEnvInvalid(t *testing.T) {
	for _, tp := range []string{"", "garbage", "00-00000000000000000000000000000000-00f067aa0ba902b7-01"} {
		ctx := FromEnv(context.Background(), func(k string) string {
			if k == EnvTraceparent {
				return tp
			}
			return ""
		})
		if trace.SpanContextFromContext(ctx).IsValid() {
			t.Fatalf("%q: expected no parent", tp)
		}
	}
}

func TestUntraced(t *testing.T) {
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}))
	if trace.SpanFromContext(Untraced(ctx)).SpanContext().IsValid() {
		t.Fatal("Untraced kept the span")
	}
}
