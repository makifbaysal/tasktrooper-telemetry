package toolset

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestConnectForwardsAuth(t *testing.T) {
	var gotCookie, gotAuth string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie, gotAuth = r.Header.Get("Cookie"), r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"orders-api","name":"orders-api","type":"http","rpm":10}]`))
	}))
	defer api.Close()

	for _, write := range []bool{false, true} {
		cs, err := Connect(context.Background(), Options{BaseURL: api.URL, AllowWrite: write,
			Auth: func(h http.Header) { h.Set("Cookie", "tt_session=abc") }})
		if err != nil {
			t.Fatal(err)
		}
		tools, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(tools.Tools) != ToolCount(write) {
			t.Fatalf("write=%v: %d tools, want %d", write, len(tools.Tools), ToolCount(write))
		}
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "tt_list_services", Arguments: map[string]any{}})
		if err != nil || res.IsError {
			t.Fatalf("call: %v %+v", err, res)
		}
		if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "orders-api") {
			t.Fatalf("result %s", text)
		}
		if gotCookie != "tt_session=abc" || gotAuth != "" {
			t.Fatalf("cookie %q auth %q", gotCookie, gotAuth)
		}
		_ = cs.Close()
	}
}

func TestNeedsApproval(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{`{}`, false},
		{`{"confirm":false}`, false},
		{`{"confirm":true,"name":"x"}`, true},
		{`not json`, false},
	} {
		if got := NeedsApproval(json.RawMessage(tc.in)); got != tc.want {
			t.Errorf("%s: %v", tc.in, got)
		}
	}
}
