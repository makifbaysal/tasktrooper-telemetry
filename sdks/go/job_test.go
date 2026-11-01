package trooper_test

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	trooper "github.com/tasktrooper/telemetry-go"
	"github.com/tasktrooper/telemetry-go/troopertest"
)

func TestStartJob(t *testing.T) {
	remote := troopertest.CallerHeaders("loadtest-ctl")
	tests := []struct {
		name string
		// parent returns the context the job starts from (the harness is installed).
		parent     func(t *testing.T) (context.Context, trace.SpanContext)
		wantRemote bool // parent kept (remote)
		wantLink   bool // local parent: new root + link
	}{
		{name: "no parent", parent: func(*testing.T) (context.Context, trace.SpanContext) {
			return context.Background(), trace.SpanContext{}
		}},
		{name: "nil context", parent: func(*testing.T) (context.Context, trace.SpanContext) {
			return nil, trace.SpanContext{} //nolint:staticcheck // nil context must be tolerated
		}},
		{name: "local parent becomes a link", wantLink: true, parent: func(t *testing.T) (context.Context, trace.SpanContext) {
			ctx, sp := otel.Tracer("t").Start(context.Background(), "outer")
			t.Cleanup(func() { sp.End() })
			return ctx, sp.SpanContext()
		}},
		{name: "remote parent kept", wantRemote: true, parent: func(*testing.T) (context.Context, trace.SpanContext) {
			ctx := otel.GetTextMapPropagator().Extract(context.Background(), propagation.MapCarrier(remote))
			return ctx, trace.SpanContextFromContext(ctx)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := troopertest.New(t, "worker")
			pctx, psc := tt.parent(t)
			ctx, span := trooper.StartJob(pctx, "billing.sync")
			_, c := otel.Tracer("t").Start(ctx, "child", trace.WithSpanKind(trace.SpanKindClient))
			c.End()
			trooper.EndSpan(span, nil)

			s := h.SpanByName("billing.sync")
			if s == nil || s.SpanKind() != trace.SpanKindConsumer {
				t.Fatalf("job span = %v", s)
			}
			want := map[string]string{
				"trooper.process": "billing.sync", "trooper.entry.route": "billing.sync", "trooper.entry.method": "JOB",
				"trooper.sample_rate": "1", "messaging.system": "",
			}
			for k, v := range want {
				if got := troopertest.AttrString(s, attrKey(k)); got != v {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}
			switch {
			case tt.wantRemote:
				if s.Parent().SpanID() != psc.SpanID() || !s.Parent().IsRemote() {
					t.Errorf("parent = %v, want remote %v", s.Parent(), psc)
				}
			default:
				if s.Parent().IsValid() {
					t.Errorf("job has a parent: %v", s.Parent())
				}
			}
			if tt.wantLink != (len(s.Links()) == 1 && s.Links()[0].SpanContext.SpanID() == psc.SpanID()) {
				t.Errorf("links = %v", s.Links())
			}
			child := h.SpanByName("child")
			if troopertest.AttrString(child, trooper.AttrEntryRoute) != "billing.sync" || troopertest.AttrString(child, trooper.AttrEntryMethod) != "JOB" {
				t.Errorf("child entry attrs = %v", child.Attributes())
			}
			if child.Parent().SpanID() != s.SpanContext().SpanID() {
				t.Errorf("child parent = %v", child.Parent())
			}
		})
	}
}

func TestStartJobNoProvider(t *testing.T) {
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(noop.NewTracerProvider())
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	ctx, span := trooper.StartJob(context.Background(), "idle")
	if span == nil || ctx == nil || span.IsRecording() {
		t.Fatalf("span = %v", span)
	}
	trooper.EndSpan(span, errors.New("x")) // must not panic
}

func TestRunJob(t *testing.T) {
	tests := []struct {
		name      string
		policy    trooper.PanicPolicy
		fn        func(ctx context.Context) error
		wantErr   bool
		wantPanic bool
	}{
		{name: "ok", fn: func(context.Context) error { return nil }},
		{name: "error", fn: func(context.Context) error { return errors.New("bad") }, wantErr: true},
		{name: "panic recover", policy: trooper.PanicRecover, fn: func(context.Context) error { panic("boom") }, wantErr: true},
		{name: "panic repanic", policy: trooper.PanicRepanic, fn: func(context.Context) error { panic("boom") }, wantPanic: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := troopertest.New(t, "worker")
			var err error
			panicked := func() (p bool) {
				defer func() { p = recover() != nil }()
				err = trooper.RunJob(context.Background(), "report.build", tt.policy, tt.fn)
				return false
			}()
			if panicked != tt.wantPanic || (err != nil) != tt.wantErr {
				t.Fatalf("panicked=%v err=%v", panicked, err)
			}
			s := h.SpanByName("report.build")
			if s == nil || s.SpanKind() != trace.SpanKindConsumer {
				t.Fatalf("span = %v", s)
			}
			if (s.Status().Code == codes.Error) != (tt.wantErr || tt.wantPanic) {
				t.Errorf("status = %v", s.Status())
			}
		})
	}
}
