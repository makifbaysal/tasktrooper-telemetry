package trooperhttp

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"runtime/pprof"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
	"github.com/tasktrooper/telemetry-go/troopertest"
)

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, child := otel.Tracer("x").Start(r.Context(), "child", trace.WithSpanKind(trace.SpanKindClient))
		child.End()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /panic", func(http.ResponseWriter, *http.Request) { panic("boom") })
	return mux
}

func TestServer(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		policy     trooper.PanicPolicy
		wantRoute  string
		wantStatus int
		wantPanic  bool
		wantChild  bool
	}{
		{name: "route template", path: "/users/42", wantRoute: "/users/:id", wantStatus: 200, wantChild: true},
		{name: "unmatched", path: "/nope", wantRoute: "", wantStatus: 404},
		{name: "panic repanic", path: "/panic", policy: trooper.PanicRepanic, wantRoute: "/panic", wantPanic: true},
		{name: "panic recover", path: "/panic", policy: trooper.PanicRecover, wantRoute: "/panic", wantStatus: 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := troopertest.New(t, "svc")
			srv := Handler(newMux(), WithPanicPolicy(tt.policy))
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			for k, v := range troopertest.CallerHeaders("upstream") {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			panicked := func() (p bool) {
				defer func() { p = recover() != nil }()
				srv.ServeHTTP(rec, req)
				return false
			}()
			if panicked != tt.wantPanic {
				t.Fatalf("panicked=%v want %v", panicked, tt.wantPanic)
			}
			if !tt.wantPanic && rec.Code != tt.wantStatus {
				t.Fatalf("status=%d want %d", rec.Code, tt.wantStatus)
			}
			server := h.SpanByKind(trace.SpanKindServer)
			if server == nil {
				t.Fatal("no server span")
			}
			if got := troopertest.AttrString(server, trooper.AttrRoute); got != tt.wantRoute {
				t.Errorf("trooper.route=%q want %q", got, tt.wantRoute)
			}
			if got := troopertest.AttrString(server, trooper.AttrCaller); got != "upstream" {
				t.Errorf("trooper.caller=%q", got)
			}
			if tt.wantRoute != "" {
				if got := troopertest.AttrString(server, trooper.AttrEntryRoute); got != tt.wantRoute {
					t.Errorf("server entry route=%q", got)
				}
			}
			if tt.wantChild {
				child := h.SpanByName("child")
				if got := troopertest.AttrString(child, trooper.AttrEntryRoute); got != tt.wantRoute {
					t.Errorf("child entry route=%q want %q", got, tt.wantRoute)
				}
				if got := troopertest.AttrString(child, trooper.AttrEntryMethod); got != "GET" {
					t.Errorf("child entry method=%q", got)
				}
				if !strings.Contains(server.Name(), "/users/") {
					t.Errorf("span name=%q", server.Name())
				}
			}
			if strings.HasPrefix(tt.name, "panic") {
				if !troopertest.HasException(server) || server.Status().Code != codes.Error {
					t.Errorf("panic not recorded: status=%v events=%d", server.Status(), len(server.Events()))
				}
			}
		})
	}
}

func TestStripPattern(t *testing.T) {
	for in, want := range map[string]string{
		"":                   "",
		"/a/{id}":            "/a/{id}",
		"GET /a/{id}":        "/a/{id}",
		"POST example.com/a": "/a",
		"example.com/":       "/",
		"GET  /x":            "/x",
	} {
		if got := StripPattern(in); got != want {
			t.Errorf("StripPattern(%q)=%q want %q", in, got, want)
		}
	}
}

func TestClientTransport(t *testing.T) {
	h := troopertest.New(t, "svc")
	var gotBaggage, gotTP string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBaggage, gotTP = r.Header.Get("baggage"), r.Header.Get("traceparent")
	}))
	defer up.Close()
	ctx, parent := otel.Tracer("x").Start(t.Context(), "parent")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, up.URL, nil)
	resp, err := NewClient(WithPeerService("billing")).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	parent.End()
	if !strings.Contains(gotBaggage, "tt.svc=svc") {
		t.Errorf("baggage=%q", gotBaggage)
	}
	if gotTP == "" {
		t.Error("no traceparent")
	}
	client := h.SpanByKind(trace.SpanKindClient)
	if got := troopertest.AttrString(client, "peer.service"); got != "billing" {
		t.Errorf("peer.service=%q", got)
	}
}

func TestProfileLabels(t *testing.T) {
	troopertest.New(t, "svc")
	trooper.EnableProfileLabels(true)
	t.Cleanup(func() { trooper.EnableProfileLabels(false) })

	var route, method string
	var dump bytes.Buffer
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		route, _ = pprof.Label(r.Context(), trooper.ProfileLabelRoute)
		method, _ = pprof.Label(r.Context(), trooper.ProfileLabelMethod)
		_ = pprof.Lookup("goroutine").WriteTo(&dump, 1)
	})
	Handler(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/orders/7", nil))
	if route != "/orders/:id" || method != "GET" {
		t.Errorf("labels = %q %q", route, method)
	}
	if !strings.Contains(dump.String(), `"trooper.route":"/orders/:id"`) {
		t.Error("request goroutine not labelled")
	}
	var after bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&after, 1)
	if strings.Contains(after.String(), `"trooper.route":"/orders/:id"`) {
		t.Error("labels not restored after the request")
	}
}
