package selftel

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"

	trooper "github.com/makifbaysal/tasktrooper-telemetry/sdks/go"
	"github.com/makifbaysal/tasktrooper-telemetry/sdks/go/troopertest"
)

func TestEntryHandler(t *testing.T) {
	for _, tc := range []struct {
		name, method, route string
		want, notWant       []string
	}{
		{"request entry", "GET", "/api/v1/services/{service}", []string{"trooper.entry.route=/api/v1/services/:service", "trooper.entry.method=GET"}, nil},
		{"no entry", "", "", nil, []string{"trooper.entry.route"}},
	} {
		var buf bytes.Buffer
		log := slog.New(EntryHandler(slog.NewTextHandler(&buf, nil))).With("svc", "tt-api")
		ctx := context.Background()
		if tc.route != "" {
			var e *trooper.Entry
			ctx, e = trooper.BeginEntry(ctx, tc.method, tc.route, nil)
			defer e.Finish("")
		}
		log.InfoContext(ctx, "hello")
		out := buf.String()
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: %q missing %q", tc.name, out, w)
			}
		}
		for _, w := range tc.notWant {
			if strings.Contains(out, w) {
				t.Errorf("%s: %q has %q", tc.name, out, w)
			}
		}
	}
}

func TestEndAndCaptureErrorRecordStack(t *testing.T) {
	h := troopertest.New(t, "tt-api")
	tr := otel.Tracer("test")
	_, span := tr.Start(context.Background(), "end")
	End(span, errors.New("boom"))
	ctx, span2 := tr.Start(context.Background(), "capture")
	CaptureError(ctx, errors.New("bad"))
	span2.End()
	for _, name := range []string{"end", "capture"} {
		s := h.SpanByName(name)
		if s == nil || !troopertest.HasException(s) {
			t.Fatalf("%s: no exception event", name)
		}
		var stack string
		for _, ev := range s.Events() {
			for _, a := range ev.Attributes {
				if a.Key == "exception.stacktrace" {
					stack = a.Value.AsString()
				}
			}
		}
		if stack == "" {
			t.Errorf("%s: exception without stack trace", name)
		}
	}
}
