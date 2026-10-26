package trooperchi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
	"github.com/tasktrooper/telemetry-go/troopertest"
)

func newRouter(policy trooper.PanicPolicy) http.Handler {
	r := chi.NewRouter()
	r.Use(Middleware(WithPanicPolicy(policy)))
	r.Route("/v1", func(r chi.Router) {
		r.Get("/orders/{id}", func(w http.ResponseWriter, r *http.Request) {
			_, child := otel.Tracer("x").Start(r.Context(), "child", trace.WithSpanKind(trace.SpanKindClient))
			child.End()
		})
	})
	r.Get("/panic", func(http.ResponseWriter, *http.Request) { panic("boom") })
	return r
}

func TestChi(t *testing.T) {
	tests := []struct {
		name, path string
		policy     trooper.PanicPolicy
		wantRoute  string
		wantStatus int
		wantPanic  bool
	}{
		{name: "subrouter template", path: "/v1/orders/7", wantRoute: "/v1/orders/:id", wantStatus: 200},
		{name: "unmatched", path: "/missing", wantStatus: 404},
		{name: "panic repanic", path: "/panic", wantRoute: "/panic", wantPanic: true},
		{name: "panic recover", path: "/panic", policy: trooper.PanicRecover, wantRoute: "/panic", wantStatus: 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := troopertest.New(t, "svc")
			srv := newRouter(tt.policy)
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			for k, v := range troopertest.CallerHeaders("upstream") {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			panicked := func() (p bool) {
				defer func() { p = recover() != nil }()
				srv.ServeHTTP(rec, req)
				return
			}()
			if panicked != tt.wantPanic {
				t.Fatalf("panicked=%v", panicked)
			}
			if !tt.wantPanic && rec.Code != tt.wantStatus {
				t.Fatalf("status=%d", rec.Code)
			}
			server := h.SpanByKind(trace.SpanKindServer)
			if got := troopertest.AttrString(server, trooper.AttrRoute); got != tt.wantRoute {
				t.Errorf("trooper.route=%q want %q", got, tt.wantRoute)
			}
			if got := troopertest.AttrString(server, trooper.AttrCaller); got != "upstream" {
				t.Errorf("caller=%q", got)
			}
			if child := h.SpanByName("child"); child != nil {
				if got := troopertest.AttrString(child, trooper.AttrEntryRoute); got != tt.wantRoute {
					t.Errorf("child entry route=%q", got)
				}
				if got := troopertest.AttrString(child, trooper.AttrEntryMethod); got != "GET" {
					t.Errorf("child entry method=%q", got)
				}
			} else if tt.wantStatus == 200 {
				t.Error("no child span")
			}
			if strings.HasPrefix(tt.name, "panic") && (!troopertest.HasException(server) || server.Status().Code != codes.Error) {
				t.Error("panic not recorded")
			}
		})
	}
}
