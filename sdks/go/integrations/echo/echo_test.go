package trooperecho

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
	"github.com/tasktrooper/telemetry-go/troopertest"
)

func TestEcho(t *testing.T) {
	tests := []struct {
		name, path string
		policy     trooper.PanicPolicy
		wantRoute  string
		wantStatus int
	}{
		{name: "route template", path: "/users/42", wantRoute: "/users/:id", wantStatus: 200},
		{name: "unmatched", path: "/missing", wantStatus: 404},
		{name: "panic repanic", path: "/panic", wantRoute: "/panic", wantStatus: 500},
		{name: "panic recover", path: "/panic", policy: trooper.PanicRecover, wantRoute: "/panic", wantStatus: 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := troopertest.New(t, "svc")
			e := echo.New()
			e.Use(middleware.Recover())
			e.Use(Middleware(WithPanicPolicy(tt.policy)))
			e.GET("/users/:id", func(c echo.Context) error {
				_, child := otel.Tracer("x").Start(c.Request().Context(), "child", trace.WithSpanKind(trace.SpanKindClient))
				child.End()
				return c.NoContent(200)
			})
			e.GET("/panic", func(echo.Context) error { panic("boom") })

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			for k, v := range troopertest.CallerHeaders("upstream") {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
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
				t.Errorf("caller=%q", got)
			}
			if tt.wantStatus == 200 {
				child := h.SpanByName("child")
				if got := troopertest.AttrString(child, trooper.AttrEntryRoute); got != tt.wantRoute {
					t.Errorf("child entry route=%q", got)
				}
				if got := troopertest.AttrString(child, trooper.AttrEntryMethod); got != "GET" {
					t.Errorf("child entry method=%q", got)
				}
			}
			if strings.HasPrefix(tt.name, "panic") && (!troopertest.HasException(server) || server.Status().Code != codes.Error) {
				t.Errorf("panic not recorded: %v", server.Status())
			}
		})
	}
}
