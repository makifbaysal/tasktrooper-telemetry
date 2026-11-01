package selftel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/tasktrooper/telemetry-go/troopertest"
)

func TestDisabledIsNoop(t *testing.T) {
	t.Setenv("TROOPER_DSN", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	before := otel.GetTracerProvider()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	Start(context.Background(), "v1", log)()
	if otel.GetTracerProvider() != before {
		t.Error("Start without DSN replaced the global TracerProvider")
	}
	if Logger(log) != log {
		t.Error("Logger without DSN must return the logger unchanged")
	}
	if Transport(http.DefaultTransport) != http.DefaultTransport {
		t.Error("Transport without DSN must return base unchanged")
	}
}

func TestToolMiddleware(t *testing.T) {
	h := troopertest.New(t, "tt-mcp")
	tests := []struct {
		name, method, tool string
		res                mcp.Result
		err                error
		wantSpan           bool
		wantStatus         codes.Code
	}{
		{name: "tools/list untraced", method: "tools/list"},
		{name: "ok call", method: "tools/call", tool: "tt_list_services", res: &mcp.CallToolResult{}, wantSpan: true, wantStatus: codes.Unset},
		{name: "tool error result", method: "tools/call", tool: "tt_search_logs", res: &mcp.CallToolResult{IsError: true}, wantSpan: true, wantStatus: codes.Error},
		{name: "handler error", method: "tools/call", tool: "tt_get_trace", err: errors.New("boom"), wantSpan: true, wantStatus: codes.Error},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h.Recorder.Reset()
			var inner trace.SpanContext
			next := func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
				inner = trace.SpanContextFromContext(ctx)
				return tc.res, tc.err
			}
			req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: tc.tool}}
			_, _ = ToolMiddleware()(next)(context.Background(), tc.method, req)
			s := h.SpanByKind(trace.SpanKindServer)
			if !tc.wantSpan {
				if s != nil {
					t.Fatalf("unexpected span %q", s.Name())
				}
				return
			}
			if s == nil || s.Name() != tc.tool {
				t.Fatalf("span = %v, want %s", s, tc.tool)
			}
			if inner.SpanID() != s.SpanContext().SpanID() {
				t.Error("handler context does not carry the tool span (api calls would not join the trace)")
			}
			if v, _ := troopertest.Attr(s, "trooper.route"); v.AsString() != tc.tool {
				t.Errorf("trooper.route = %q", v.AsString())
			}
			if s.Status().Code != tc.wantStatus {
				t.Errorf("status = %v, want %v", s.Status().Code, tc.wantStatus)
			}
		})
	}
}
