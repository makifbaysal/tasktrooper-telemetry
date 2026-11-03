package selftel

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	trooper "github.com/makifbaysal/tasktrooper-telemetry/sdks/go"
)

func TestEntryHandler(t *testing.T) {
	tests := []struct {
		name               string
		method, route      string
		wantRoute, wantMet string
	}{
		{"http entry", "GET", "/v1/items/{id}", "/v1/items/:id", "GET"},
		{"job entry", trooper.JobMethod, "sync", "sync", trooper.JobMethod},
		{"no entry", "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.route != "" {
				var e *trooper.Entry
				ctx, e = trooper.BeginEntry(ctx, tt.method, tt.route, nil)
				defer e.Finish("")
			}
			var buf bytes.Buffer
			slog.New(entryHandler{slog.NewJSONHandler(&buf, nil)}).With("k", "v").InfoContext(ctx, "hello")
			var line map[string]any
			if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
				t.Fatalf("output: %v (%q)", err, buf.String())
			}
			if got, _ := line["trooper.entry.route"].(string); got != tt.wantRoute {
				t.Errorf("route = %q, want %q", got, tt.wantRoute)
			}
			if got, _ := line["trooper.entry.method"].(string); got != tt.wantMet {
				t.Errorf("method = %q, want %q", got, tt.wantMet)
			}
			if line["k"] != "v" {
				t.Errorf("WithAttrs lost: %v", line)
			}
		})
	}
}
