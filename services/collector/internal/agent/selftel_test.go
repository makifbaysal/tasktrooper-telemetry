package agent

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/codes"

	"github.com/tasktrooper/telemetry-go/troopertest"

	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
)

type nopPub struct{}

func (nopPub) Publish(context.Context, string, emit.Result) {}
func (nopPub) Remove(string)                                {}

func TestOnceScrapeSpan(t *testing.T) {
	cases := []struct {
		name    string
		fn      func() (emit.Result, error)
		up      bool
		samples int64
		status  codes.Code
	}{
		{"ok", func() (emit.Result, error) {
			var r emit.Result
			r.Gauge("trooper.db.connections", 3)
			return r, nil
		}, true, 2, codes.Unset}, // + trooper.collector.up
		{"error", func() (emit.Result, error) { return emit.Result{}, errors.New("connection refused") }, false, 0, codes.Error},
		{"panic", func() (emit.Result, error) { panic("boom") }, false, 0, codes.Error},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := troopertest.New(t, "tt-trooper-agent")
			r := &Runner{T: target("db/orders", "postgres", c.fn), Pub: nopPub{}, Log: quiet}
			defer r.Close()
			if got := r.Once(context.Background()); got != c.up {
				t.Fatalf("Once = %v, want %v", got, c.up)
			}
			s := h.SpanByName("agent.scrape")
			if s == nil {
				t.Fatalf("no agent.scrape span; got %d spans", len(h.Spans()))
			}
			if s.Parent().IsValid() {
				t.Error("scrape span must be a root span")
			}
			if v := troopertest.AttrString(s, "tt.collector.kind"); v != "postgres" {
				t.Errorf("kind = %q", v)
			}
			if v := troopertest.AttrString(s, "tt.collector.target"); v != "db/orders" {
				t.Errorf("target = %q", v)
			}
			if v, _ := troopertest.Attr(s, "tt.collector.up"); v.AsBool() != c.up {
				t.Errorf("up = %v", v.AsBool())
			}
			if v, _ := troopertest.Attr(s, "tt.collector.samples"); v.AsInt64() != c.samples-boolInt(c.up) {
				t.Errorf("samples = %d, want %d", v.AsInt64(), c.samples-boolInt(c.up))
			}
			if s.Status().Code != c.status {
				t.Errorf("status = %v, want %v", s.Status().Code, c.status)
			}
		})
	}
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
