package runner

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	trooperhttp "github.com/tasktrooper/telemetry-go/integrations/nethttp"
	"github.com/tasktrooper/telemetry-go/troopertest"

	"github.com/tasktrooper/telemetry/services/integrations/internal/model"
)

// httpProvider calls a (fake) cloud API with the step context, like the real providers do.
type httpProvider struct {
	url string
	hc  *http.Client
}

func (p httpProvider) get(ctx context.Context) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if res, err := p.hc.Do(req); err == nil {
		_ = res.Body.Close()
	}
}

func (p httpProvider) Discover(ctx context.Context) (*model.Inventory, error) {
	p.get(ctx)
	return &model.Inventory{}, nil
}

func (p httpProvider) Collect(ctx context.Context, _ *model.Inventory, _ time.Time) (*model.Result, error) {
	p.get(ctx)
	return &model.Result{}, nil
}

// Self telemetry (ADR-027): each discovery / metrics step is a root span carrying the tenant, and provider API calls
// made with the step context are its children.
func TestSelfTelemetryStepSpans(t *testing.T) {
	h := troopertest.New(t, "tt-trooper-cloud")
	api := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer api.Close()
	p := httpProvider{url: api.URL, hc: &http.Client{Transport: trooperhttp.NewTransport(nil)}}
	st := &fakeStore{status: map[int64]string{}, errs: map[int64]string{}, res: map[int64]int{}}
	r := New(Options{Store: st, Sender: &fakeSender{sent: map[int64][]model.Sample{}},
		Factory: func(context.Context, model.Integration) (model.Provider, error) { return p, nil },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err := r.RunOnce(context.Background(), model.Integration{ID: 9, ProjectID: 5, Provider: "neon"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"discover neon", "collect neon"} {
		root := h.SpanByName(name)
		if root == nil {
			t.Fatalf("no %q span", name)
		}
		if root.Parent().IsValid() {
			t.Errorf("%s is not a root span", name)
		}
		if v, _ := troopertest.Attr(root, "tt.project_id"); v.AsInt64() != 5 {
			t.Errorf("%s tt.project_id = %d", name, v.AsInt64())
		}
		children := 0
		for _, s := range h.Spans() {
			if s.SpanKind() == trace.SpanKindClient && s.Parent().SpanID() == root.SpanContext().SpanID() {
				children++
			}
		}
		if children != 1 {
			t.Errorf("%s has %d HTTP client children, want 1", name, children)
		}
	}
}
