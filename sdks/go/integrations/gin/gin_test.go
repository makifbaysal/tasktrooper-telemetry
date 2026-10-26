package troopergin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
	"github.com/tasktrooper/telemetry-go/troopertest"
)

func TestGin(t *testing.T) {
	gin.SetMode(gin.TestMode)
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
			r := gin.New()
			r.Use(gin.Recovery())
			Use(r, WithPanicPolicy(tt.policy))
			r.GET("/users/:id", func(c *gin.Context) {
				_, child := otel.Tracer("x").Start(c.Request.Context(), "child", trace.WithSpanKind(trace.SpanKindClient))
				child.End()
				c.Status(200)
			})
			r.GET("/panic", func(*gin.Context) { panic("boom") })

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			for k, v := range troopertest.CallerHeaders("upstream") {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
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
			if got := troopertest.AttrString(server, trooper.AttrEntryRoute); got != tt.wantRoute {
				t.Errorf("server entry route=%q", got)
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
