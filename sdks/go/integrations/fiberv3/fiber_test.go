package trooperfiber3

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
	"github.com/tasktrooper/telemetry-go/troopertest"
)

func TestFiber(t *testing.T) {
	tests := []struct {
		name, path string
		policy     trooper.PanicPolicy
		wantRoute  string
		wantStatus int
	}{
		{name: "route template", path: "/users/42", wantRoute: "/users/:id", wantStatus: 200},
		{name: "handler 404 keeps route", path: "/users/0", wantRoute: "/users/:id", wantStatus: 404},
		{name: "unmatched", path: "/missing", wantStatus: 404},
		{name: "panic repanic", path: "/panic", wantRoute: "/panic", wantStatus: 500},
		{name: "panic recover", path: "/panic", policy: trooper.PanicRecover, wantRoute: "/panic", wantStatus: 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := troopertest.New(t, "svc")
			app := fiber.New()
			app.Use(recover.New())
			Use(app, WithPanicPolicy(tt.policy))
			app.Use(func(c fiber.Ctx) error { return c.Next() }) // another middleware
			app.Get("/users/:id", func(c fiber.Ctx) error {
				_, child := otel.Tracer("x").Start(c.Context(), "child", trace.WithSpanKind(trace.SpanKindClient))
				child.End()
				if c.Params("id") == "0" {
					return fiber.ErrNotFound
				}
				return c.SendStatus(200)
			})
			app.Get("/panic", func(fiber.Ctx) error { panic("boom") })

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			for k, v := range troopertest.CallerHeaders("upstream") {
				req.Header.Set(k, v)
			}
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status=%d want %d", resp.StatusCode, tt.wantStatus)
			}
			if resp.Header.Get("baggage") != "" || resp.Header.Get("traceparent") != "" {
				t.Error("trace headers leaked into response")
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
			if child := h.SpanByName("child"); child != nil {
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
