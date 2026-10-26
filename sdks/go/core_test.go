package trooper_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
	"github.com/tasktrooper/telemetry-go/troopertest"
)

func TestParseDSN(t *testing.T) {
	tests := []struct {
		name, in               string
		endpoint, key, project string
		wantErr                bool
	}{
		{name: "local", in: "http://tt_dev_ingest_key@localhost:4318/1", endpoint: "http://localhost:4318", key: "tt_dev_ingest_key", project: "1"},
		{name: "https no port", in: "https://abc@ingest.trooper.dev/42", endpoint: "https://ingest.trooper.dev", key: "abc", project: "42"},
		{name: "trailing slash", in: "https://abc@ingest.trooper.dev/42/", endpoint: "https://ingest.trooper.dev", key: "abc", project: "42"},
		{name: "path prefix", in: "https://abc@proxy.example.com/ingest/7", endpoint: "https://proxy.example.com/ingest", key: "abc", project: "7"},
		{name: "no project", in: "https://abc@ingest.trooper.dev", endpoint: "https://ingest.trooper.dev", key: "abc"},
		{name: "whitespace", in: "  http://k@h:1/2 ", endpoint: "http://h:1", key: "k", project: "2"},
		{name: "empty", in: "", wantErr: true},
		{name: "no key", in: "https://ingest.trooper.dev/1", wantErr: true},
		{name: "bad scheme", in: "grpc://k@host/1", wantErr: true},
		{name: "no host", in: "https://k@/1", wantErr: true},
		{name: "garbage", in: "::not a url", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := trooper.ParseDSN(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if d.Endpoint != tt.endpoint || d.Key != tt.key || d.ProjectID != tt.project {
				t.Fatalf("got %+v", d)
			}
		})
	}
}

func TestNormalizeRoute(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/users/:id", "/users/:id"},
		{"/users/{id}", "/users/:id"},
		{"/users/{id}/orders/{oid}", "/users/:id/orders/:oid"},
		{"/users/{id:[0-9]+}", "/users/:id"},
		{"/users/{id:[0-9]{3}}", "/users/:id"},
		{"/files/{path...}", "/files/*path"},
		{"/exact/{$}", "/exact/"},
		{"/pkg.Svc/Method", "/pkg.Svc/Method"},
		{"/broken/{id", "/broken/{id"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := trooper.NormalizeRoute(tt.in); got != tt.want {
			t.Errorf("NormalizeRoute(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSanitizeSQL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"SELECT * FROM users WHERE id = 42", "SELECT * FROM users WHERE id = ?"},
		{"SELECT * FROM users WHERE email = 'a@b.com' AND n = 3.5", "SELECT * FROM users WHERE email = ? AND n = ?"},
		{"SELECT 'it''s'", "SELECT ?"},
		{"SELECT * FROM t WHERE id = $1 AND x = ?", "SELECT * FROM t WHERE id = $1 AND x = ?"},
		{"SELECT * FROM t2 WHERE c IN (1, 2, 3)", "SELECT * FROM t2 WHERE c IN (?, ?, ?)"},
		{"SELECT $$secret$$, $tag$x$tag$", "SELECT ?, ?"},
		{"SELECT 1 -- comment 'pii'\nFROM t", "SELECT ? \nFROM t"},
		{"INSERT INTO t (a) VALUES (0x1F)", "INSERT INTO t (a) VALUES (?)"},
		{"SELECT col1 FROM table_2", "SELECT col1 FROM table_2"},
		{"SELECT 'unterminated", "SELECT ?"},
	}
	for _, tt := range tests {
		if got := trooper.SanitizeSQL(tt.in); got != tt.want {
			t.Errorf("SanitizeSQL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestEntryRoutePropagation verifies that trooper.entry.route/method set at the entry span reach
// every span started under it, including deep descendants, client spans and goroutines.
func TestEntryRoutePropagation(t *testing.T) {
	h := troopertest.New(t, "svc")
	tr := otel.Tracer("test")

	ctx, srv := tr.Start(context.Background(), "GET /orders/:id", trace.WithSpanKind(trace.SpanKindServer))
	ctx, entry := trooper.BeginEntry(ctx, "GET", "/orders/{id}", nil)

	c1ctx, c1 := tr.Start(ctx, "db", trace.WithSpanKind(trace.SpanKindClient))
	_, c2 := tr.Start(c1ctx, "nested", trace.WithSpanKind(trace.SpanKindInternal))
	c2.End()
	c1.End()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, g := tr.Start(ctx, "goroutine", trace.WithSpanKind(trace.SpanKindProducer))
		g.End()
	}()
	wg.Wait()
	entry.Finish("")
	srv.End()
	_, outside := tr.Start(context.Background(), "outside")
	outside.End()

	for _, name := range []string{"GET /orders/:id", "db", "nested", "goroutine"} {
		s := h.SpanByName(name)
		if got := troopertest.AttrString(s, trooper.AttrEntryRoute); got != "/orders/:id" {
			t.Errorf("%s: entry.route = %q", name, got)
		}
		if got := troopertest.AttrString(s, trooper.AttrEntryMethod); got != "GET" {
			t.Errorf("%s: entry.method = %q", name, got)
		}
	}
	if _, ok := troopertest.Attr(h.SpanByName("outside"), trooper.AttrEntryRoute); ok {
		t.Error("span outside the entry must not have entry.route")
	}
}

func TestLazyEntryResolver(t *testing.T) {
	h := troopertest.New(t, "svc")
	tr := otel.Tracer("test")
	var mu sync.Mutex
	route := "" // filled by the "router" after middleware ran
	ctx, srv := tr.Start(context.Background(), "srv", trace.WithSpanKind(trace.SpanKindServer))
	ctx, entry := trooper.BeginEntry(ctx, "POST", "", func() string { mu.Lock(); defer mu.Unlock(); return route })

	_, before := tr.Start(ctx, "before-routing")
	before.End()
	mu.Lock()
	route = "/items/{id}"
	mu.Unlock()
	_, after := tr.Start(ctx, "after-routing")
	after.End()
	entry.Finish("")
	mu.Lock()
	route = "/reused-by-another-request"
	mu.Unlock()
	_, late := tr.Start(ctx, "after-finish")
	late.End()
	srv.End()

	tests := []struct{ span, want string }{
		{"before-routing", ""},
		{"after-routing", "/items/:id"},
		{"after-finish", "/items/:id"}, // frozen: resolver not consulted after Finish
		{"srv", "/items/:id"},
	}
	for _, tt := range tests {
		if got := troopertest.AttrString(h.SpanByName(tt.span), trooper.AttrEntryRoute); got != tt.want {
			t.Errorf("%s: entry.route = %q, want %q", tt.span, got, tt.want)
		}
	}
}

func TestEntryResolverPanicIsSwallowed(t *testing.T) {
	h := troopertest.New(t, "svc")
	ctx, entry := trooper.BeginEntry(context.Background(), "GET", "", func() string { panic("boom") })
	_, s := otel.Tracer("t").Start(ctx, "child")
	s.End()
	entry.Finish("")
	if len(h.Spans()) != 1 {
		t.Fatal("span not recorded")
	}
}

func TestSampleRateOnEntrySpans(t *testing.T) {
	h := troopertest.New(t, "svc")
	tr := otel.Tracer("test")
	remote := trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled, Remote: true,
	}))
	rootCtx, root := tr.Start(context.Background(), "root-server", trace.WithSpanKind(trace.SpanKindServer))
	_, localChildServer := tr.Start(rootCtx, "local-child-server", trace.WithSpanKind(trace.SpanKindServer))
	localChildServer.End()
	root.End()
	_, rs := tr.Start(remote, "remote-consumer", trace.WithSpanKind(trace.SpanKindConsumer))
	rs.End()
	_, internal := tr.Start(context.Background(), "root-internal")
	internal.End()

	tests := []struct {
		span string
		want bool
	}{{"root-server", true}, {"remote-consumer", true}, {"local-child-server", false}, {"root-internal", false}}
	for _, tt := range tests {
		v, ok := troopertest.Attr(h.SpanByName(tt.span), trooper.AttrSampleRate)
		if ok != tt.want || (ok && v.AsFloat64() != 1) {
			t.Errorf("%s: sample_rate present=%v value=%v, want present=%v", tt.span, ok, v.AsFloat64(), tt.want)
		}
	}
}

func TestCallerPropagation(t *testing.T) {
	h := troopertest.New(t, "orders-api")
	// Incoming request from "checkout" with its own baggage.
	in := troopertest.CallerHeaders("checkout")
	in["baggage"] += ",tenant=acme"
	ctx := otel.GetTextMapPropagator().Extract(context.Background(), in)
	if got := trooper.CallerFromContext(ctx); got != "checkout" {
		t.Fatalf("caller = %q", got)
	}
	ctx, srv := otel.Tracer("t").Start(ctx, "srv", trace.WithSpanKind(trace.SpanKindServer))
	ctx, _ = trooper.BeginEntry(ctx, "GET", "/x", nil)
	srv.End()
	if got := troopertest.AttrString(h.SpanByName("srv"), trooper.AttrCaller); got != "checkout" {
		t.Errorf("trooper.caller = %q", got)
	}
	// Outgoing: tt.svc is replaced by our own service, other members kept.
	out := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, out)
	b, err := baggage.Parse(out["baggage"])
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Member("tt.svc").Value(); got != "orders-api" {
		t.Errorf("outgoing tt.svc = %q", got)
	}
	if got := b.Member("tenant").Value(); got != "acme" {
		t.Errorf("outgoing tenant = %q", got)
	}
	if out["traceparent"] == "" {
		t.Error("missing traceparent")
	}
}

func TestCaptureError(t *testing.T) {
	h := troopertest.New(t, "svc")
	ctx, s := otel.Tracer("t").Start(context.Background(), "op")
	trooper.CaptureError(ctx, errors.New("db down"))
	trooper.CaptureError(ctx, nil)
	s.End()
	trooper.CaptureError(context.Background(), errors.New("no span")) // must not panic
	trooper.CaptureError(nil, errors.New("nil ctx"))                  //nolint:staticcheck // must not panic

	sp := h.SpanByName("op")
	if sp.Status().Code != codes.Error || sp.Status().Description != "db down" {
		t.Errorf("status = %+v", sp.Status())
	}
	if !troopertest.HasException(sp) {
		t.Fatal("no exception event")
	}
	found := false
	for _, a := range sp.Events()[0].Attributes {
		if a.Key == "exception.stacktrace" && a.Value.AsString() != "" {
			found = true
		}
	}
	if !found {
		t.Error("no stacktrace")
	}
}

func TestProcessMessage(t *testing.T) {
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
			carrier := troopertest.CallerHeaders("producer-svc")
			m := trooper.Message{System: "kafka", Destination: "orders", ConsumerGroup: "g1", Partition: 3, Attempt: 2}
			var err error
			panicked := func() (p bool) {
				defer func() { p = recover() != nil }()
				err = trooper.ProcessMessage(context.Background(), carrier, m, tt.policy, func(ctx context.Context) error {
					_, c := otel.Tracer("t").Start(ctx, "child", trace.WithSpanKind(trace.SpanKindClient))
					c.End()
					return tt.fn(ctx)
				})
				return false
			}()
			if panicked != tt.wantPanic || (err != nil) != tt.wantErr {
				t.Fatalf("panicked=%v err=%v", panicked, err)
			}
			s := h.SpanByKind(trace.SpanKindConsumer)
			want := map[string]string{
				"messaging.system": "kafka", "messaging.destination.name": "orders", "messaging.consumer.group.name": "g1",
				"messaging.destination.partition.id": "3", "trooper.process": "g1", "trooper.consumer.attempt": "2",
				"trooper.caller": "producer-svc", "trooper.entry.route": "g1", "trooper.sample_rate": "1",
			}
			for k, v := range want {
				if got := troopertest.AttrString(s, attrKey(k)); got != v {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}
			if s.Name() != "process orders" {
				t.Errorf("name = %q", s.Name())
			}
			if s.Parent().TraceID() != (trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}) || !s.Parent().IsRemote() {
				t.Errorf("parent = %v", s.Parent())
			}
			if got := troopertest.AttrString(h.SpanByName("child"), trooper.AttrEntryRoute); got != "g1" {
				t.Errorf("child entry.route = %q", got)
			}
			if (s.Status().Code == codes.Error) != (tt.wantErr || tt.wantPanic) {
				t.Errorf("status = %v", s.Status())
			}
		})
	}
}

func TestStartProducerInjects(t *testing.T) {
	h := troopertest.New(t, "svc")
	c := propagation.MapCarrier{}
	_, s := trooper.StartProducer(context.Background(), c, trooper.Message{System: "rabbitmq", Destination: "jobs", Partition: -1})
	trooper.EndSpan(s, nil)
	if c["traceparent"] == "" || c["baggage"] != "tt.svc=svc" {
		t.Errorf("carrier = %v", c)
	}
	if sp := h.SpanByKind(trace.SpanKindProducer); sp == nil || sp.Name() != "send jobs" {
		t.Errorf("producer span = %v", sp)
	}
}

func TestMarkDLQ(t *testing.T) {
	h := troopertest.New(t, "svc")
	ctx, s := trooper.StartConsumer(context.Background(), nil, trooper.Message{System: "rabbitmq", Destination: "q", Partition: -1})
	trooper.MarkDLQ(ctx)
	s.End()
	if got := troopertest.AttrString(h.SpanByKind(trace.SpanKindConsumer), trooper.AttrConsumerDLQ); got != "true" {
		t.Errorf("dlq = %q", got)
	}
}
