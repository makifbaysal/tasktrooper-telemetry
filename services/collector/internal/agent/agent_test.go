package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/tasktrooper/telemetry/services/collector/internal/config"
	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
	"github.com/tasktrooper/telemetry/services/collector/internal/otlptest"
	"github.com/tasktrooper/telemetry/services/collector/internal/scrape"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type fakeScraper struct {
	fn     func() (emit.Result, error)
	closed atomic.Bool
}

func (f *fakeScraper) Scrape(context.Context) (emit.Result, error) { return f.fn() }
func (f *fakeScraper) Close() error                                { f.closed.Store(true); return nil }

func target(id, kind string, fn func() (emit.Result, error)) Target {
	_, key, _ := strings.Cut(id, "/")
	return Target{ID: id, Key: key, Kind: kind, Interval: time.Second, Timeout: time.Second, Spec: "v1",
		New: func() (scrape.Scraper, error) { return &fakeScraper{fn: fn}, nil }}
}

func newEmitter(t *testing.T, srv *otlptest.Server) *emit.Emitter {
	t.Helper()
	d, err := config.ParseIngestDSN("http://tt_test_key@" + srv.Listener.Addr().String() + "/1")
	if err != nil {
		t.Fatal(err)
	}
	em, err := emit.New(context.Background(), emit.Options{MetricsURL: d.MetricsURL, Key: d.Key, Interval: time.Hour, Version: "test", Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = em.Shutdown(context.Background()) })
	return em
}

func TestEndToEndOTLP(t *testing.T) {
	srv := otlptest.New()
	defer srv.Close()
	em := newEmitter(t, srv)
	ctx := context.Background()

	dbAttrs := emit.DBAttrs("orders-db", "orders-db", "postgresql", "orders-db-1", "primary")
	qAttrs := []attribute.KeyValue{
		attribute.String(emit.AttrDBID, "orders-db"),
		attribute.String(emit.AttrQueryHash, "12345"),
		attribute.String(emit.AttrQueryText, "SELECT * FROM orders WHERE id = $1"),
	}
	ok := &Runner{T: target("db/orders-db", "postgres", func() (emit.Result, error) {
		var r emit.Result
		r.Gauge(emit.DBConnections, 42, dbAttrs...)
		r.Gauge(emit.DBHitRatio, 0.99, dbAttrs...)
		r.Count(emit.DBQueryCalls, 30, qAttrs...)
		r.Count(emit.DBQueryTotalMs, 90, qAttrs...)
		return r, nil
	}), Pub: em, Log: quiet}
	failing := &Runner{T: target("mq/kafka-main", "kafka", func() (emit.Result, error) {
		return emit.Result{}, errors.New("dial tcp: connection refused")
	}), Pub: em, Log: quiet}
	panicking := &Runner{T: target("db/cache", "redis", func() (emit.Result, error) { panic("boom") }), Pub: em, Log: quiet}
	broken := &Runner{T: Target{ID: "db/x", Key: "x", Kind: "mysql", Timeout: time.Second,
		New: func() (scrape.Scraper, error) { return nil, errors.New("bad dsn") }}, Pub: em, Log: quiet}

	if !ok.Once(ctx) || failing.Once(ctx) || panicking.Once(ctx) || broken.Once(ctx) {
		t.Fatal("unexpected Once results")
	}
	if err := em.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	if keys := srv.Keys(); len(keys) == 0 || keys[0] != "tt_test_key" {
		t.Fatalf("x-trooper-key: %v", keys)
	}
	conn := srv.Find(emit.DBConnections, map[string]string{
		"trooper.db.id": "orders-db", "trooper.db.name": "orders-db", "db.system": "postgresql",
		"trooper.db.node": "orders-db-1", "trooper.db.role": "primary",
	})
	if len(conn) != 1 || conn[0].Value != 42 || conn[0].Kind != "gauge" || conn[0].ServiceName != "trooper-agent" {
		t.Fatalf("connections: %+v", conn)
	}
	calls := srv.Find(emit.DBQueryCalls, map[string]string{"trooper.db.query.hash": "12345", "db.query.text": "SELECT * FROM orders WHERE id = $1"})
	if len(calls) != 1 || calls[0].Value != 30 || calls[0].Kind != "sum" || !calls[0].Delta || !calls[0].Monotonic {
		t.Fatalf("query calls: %+v", calls)
	}
	for id, want := range map[string]float64{"orders-db": 1, "kafka-main": 0, "cache": 0, "x": 0} {
		up := srv.Find(emit.CollectorUp, map[string]string{"trooper.collector.target": id})
		if len(up) != 1 || up[0].Value != want {
			t.Errorf("up %s: %+v want %v", id, up, want)
		}
	}

	// Second export: deltas were consumed (no new calls), gauges are re-reported; removing a
	// target drops its gauges.
	em.Remove("mq/kafka-main")
	before := len(srv.Points())
	if err := em.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	second := srv.Points()[before:]
	for _, p := range second {
		if p.Metric == emit.DBQueryCalls && p.Value != 0 {
			t.Errorf("delta re-sent: %+v", p)
		}
		if p.Attrs["trooper.collector.target"] == "kafka-main" {
			t.Errorf("removed target still exported: %+v", p)
		}
	}
	var sawConn bool
	for _, p := range second {
		sawConn = sawConn || p.Metric == emit.DBConnections
	}
	if !sawConn {
		t.Error("gauge snapshot not re-exported")
	}
}

type memPub struct {
	n       atomic.Int64
	removed atomic.Int64
}

func (m *memPub) Publish(context.Context, string, emit.Result) { m.n.Add(1) }
func (m *memPub) Remove(string)                                { m.removed.Add(1) }

func TestApplyReconciles(t *testing.T) {
	pub := &memPub{}
	a := New(pub, quiet)
	ctx := context.Background()
	fn := func() (emit.Result, error) { return emit.Result{}, nil }
	a.Apply(ctx, []Target{target("db/a", "redis", fn), target("db/b", "redis", fn)})
	if got := a.IDs(); len(got) != 2 {
		t.Fatalf("ids %v", got)
	}
	changed := target("db/a", "redis", fn)
	changed.Spec = "v2"
	a.Apply(ctx, []Target{changed})
	if got := a.IDs(); len(got) != 1 || got[0] != "db/a" {
		t.Fatalf("ids %v", got)
	}
	if pub.removed.Load() != 1 {
		t.Fatalf("removed=%d", pub.removed.Load())
	}
	a.Stop()
	if len(a.IDs()) != 0 {
		t.Fatal("stop")
	}
}

func TestTargetsModes(t *testing.T) {
	c, err := config.Parse([]byte(`
databases:
  - {id: a, engine: redis, host: h, collector: agent}
  - {id: b, engine: postgres, host: h, collector: direct}
  - {id: c, engine: postgres, collector: cloud, cloudResourceId: arn}
brokerClusters:
  - {id: k, kind: kafka, endpoint: 'x:9092'}
`))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		modes []string
		want  []string
	}{
		{nil, []string{"db/a", "db/b", "db/c", "mq/k"}},
		{[]string{"agent"}, []string{"db/a", "mq/k"}},
		{[]string{"direct", "cloud"}, []string{"db/b", "db/c"}},
	}
	for _, tt := range tests {
		got := Targets(c, Options{Modes: tt.modes}, quiet)
		if len(got) != len(tt.want) {
			t.Fatalf("modes %v: %d targets", tt.modes, len(got))
		}
		for i := range got {
			if got[i].ID != tt.want[i] {
				t.Fatalf("modes %v: got %s want %s", tt.modes, got[i].ID, tt.want[i])
			}
		}
	}
	// spec hash is stable and changes with the target
	a1 := Targets(c, Options{}, quiet)[0].Spec
	a2 := Targets(c, Options{}, quiet)[0].Spec
	c.Databases[0].Port = 7000
	a3 := Targets(c, Options{}, quiet)[0].Spec
	if a1 != a2 || a1 == a3 {
		t.Fatalf("spec hash %s %s %s", a1, a2, a3)
	}
}
