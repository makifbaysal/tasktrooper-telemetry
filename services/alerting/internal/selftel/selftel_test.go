package selftel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/tasktrooper/telemetry-go/troopertest"
)

func TestConfig(t *testing.T) {
	tests := []struct {
		name                        string
		env                         map[string]string
		wantService, wantEnv, wantV string
	}{
		{"defaults", nil, "tt-alert-engine", "development", "dev"},
		{"tt env", map[string]string{"TT_ENV": "production"}, "tt-alert-engine", "production", "dev"},
		{"trooper env wins", map[string]string{"TT_ENV": "production", "TROOPER_ENV": "staging"}, "tt-alert-engine", "staging", "dev"},
		{"service override", map[string]string{"TROOPER_SERVICE": "alerts-eu"}, "alerts-eu", "development", "dev"},
		{"otel service wins", map[string]string{"TROOPER_SERVICE": "a", "OTEL_SERVICE_NAME": "b"}, "b", "development", "dev"},
		{"version override", map[string]string{"TROOPER_VERSION": "1.2.3"}, "tt-alert-engine", "development", "1.2.3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, k := range []string{"TT_ENV", "TROOPER_ENV", "TROOPER_SERVICE", "OTEL_SERVICE_NAME", "TROOPER_VERSION"} {
				t.Setenv(k, tt.env[k])
			}
			c := Config("tt-alert-engine", "dev")
			if c.Service != tt.wantService || c.Environment != tt.wantEnv || c.Version != tt.wantV {
				t.Fatalf("got %q/%q/%q", c.Service, c.Environment, c.Version)
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
	Start(context.Background(), "tt-alert-engine", "dev", nil)() // no-op, must not panic
}

func TestOperation(t *testing.T) {
	tests := map[string]string{
		"SELECT 1":                             "SELECT",
		"  select count() FROM spans":          "SELECT",
		"WITH x AS (SELECT 1) SELECT * FROM x": "SELECT",
		"INSERT INTO sli_1m VALUES":            "INSERT",
		"-- comment\nSELECT 1":                 "SELECT",
		"/* c */ ALTER TABLE t DELETE WHERE 1": "ALTER",
		"":                                     "",
	}
	for q, want := range tests {
		if got := Operation(q); got != want {
			t.Errorf("Operation(%q) = %q, want %q", q, got, want)
		}
	}
}

// fakeConn implements the methods the wrapper calls.
type fakeConn struct {
	driver.Conn
	err error
}

func (f *fakeConn) Select(context.Context, any, string, ...any) error { return f.err }
func (f *fakeConn) Exec(context.Context, string, ...any) error        { return f.err }

func TestClickHouseSpans(t *testing.T) {
	h := troopertest.New(t, "tt-alert-engine")
	conn := ClickHouse(&fakeConn{}, "telemetry", "clickhouse")

	// no parent → no orphan span
	if err := conn.Select(context.Background(), nil, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if n := len(h.Spans()); n != 0 {
		t.Fatalf("orphan spans: %d", n)
	}

	ctx, root := StartRoot(context.Background(), "alert.cycle")
	if err := conn.Select(ctx, nil, "SELECT count() FROM spans WHERE org_id = 42 AND name = 'secret'"); err != nil {
		t.Fatal(err)
	}
	failing := ClickHouse(&fakeConn{err: errors.New("boom")}, "telemetry", "clickhouse")
	_ = failing.Exec(ctx, "INSERT INTO sli_1m VALUES (1)")
	root.End()

	var sel, ins bool
	for _, s := range h.Spans() {
		if s.SpanKind() != trace.SpanKindClient {
			continue
		}
		if troopertest.AttrString(s, "db.system") != "clickhouse" || troopertest.AttrString(s, "db.namespace") != "telemetry" {
			t.Errorf("%s: attrs %v", s.Name(), s.Attributes())
		}
		q := troopertest.AttrString(s, "db.query.text")
		if strings.Contains(q, "42") || strings.Contains(q, "secret") {
			t.Errorf("unsanitized SQL %q", q)
		}
		if s.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Errorf("%s not a child of the root", s.Name())
		}
		switch s.Name() {
		case "SELECT telemetry":
			sel = true
		case "INSERT telemetry":
			ins = s.Status().Code == codes.Error
		}
	}
	if !sel || !ins {
		t.Fatalf("select=%v insert(error)=%v; spans=%d", sel, ins, len(h.Spans()))
	}
}

func TestRedactedTransport(t *testing.T) {
	h := troopertest.New(t, "tt-notifier")
	var gotTP string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTP = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	hc := &http.Client{Transport: RedactedTransport(nil, "slack")}
	ctx, root := StartRoot(context.Background(), "notify.deliver")
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/services/T000/B000/SECRET?token=x", nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	root.End()
	s := h.SpanByKind(trace.SpanKindClient)
	if s == nil {
		t.Fatal("no client span")
	}
	for _, kv := range s.Attributes() {
		if v := kv.Value.Emit(); strings.Contains(v, "SECRET") || strings.Contains(v, "token") {
			t.Errorf("secret leaked in %s=%s", kv.Key, v)
		}
	}
	if troopertest.AttrString(s, "peer.service") != "slack" || s.Status().Code != codes.Error {
		t.Errorf("attrs %v status %v", s.Attributes(), s.Status())
	}
	if !strings.Contains(gotTP, s.SpanContext().TraceID().String()) {
		t.Errorf("traceparent %q not propagated", gotTP)
	}
}
