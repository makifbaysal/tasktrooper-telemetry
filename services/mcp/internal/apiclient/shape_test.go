package apiclient

import (
	"encoding/json"
	"strings"
	"testing"
)

// bigDoc builds a JSON array of n items each ~size bytes, decoded the way the
// client decodes API responses (into generic values).
func bigDoc(t *testing.T, n, size int) []any {
	t.Helper()
	fill := strings.Repeat("x", size)
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{"id": i, "message": fill}
	}
	return items
}

func TestTruncateSmallDocUnchanged(t *testing.T) {
	doc := map[string]any{"items": []any{map[string]any{"id": 1}}}
	out, err := Truncate(doc, []string{"items"}, "narrow")
	if err != nil {
		t.Fatal(err)
	}
	obj, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("output type %T", out)
	}
	if _, has := obj["truncated"]; has {
		t.Errorf("small doc must not carry truncated marker")
	}
	if _, has := obj["hint"]; has {
		t.Errorf("small doc must not carry hint")
	}
}

func TestTruncateBareArray(t *testing.T) {
	doc := bigDoc(t, 400, 1000) // ~400 KB, way over the 64 KB cap
	out, err := Truncate(doc, []string{"items"}, "narrow with range or env")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if len(b) > MaxResultBytes {
		t.Errorf("result %d bytes exceeds cap %d", len(b), MaxResultBytes)
	}
	obj, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("bare array must be wrapped, got %T", out)
	}
	if obj["truncated"] != true {
		t.Errorf("truncated marker missing: %v", obj["truncated"])
	}
	if obj["total"] != 400 {
		t.Errorf("total = %v, want 400", obj["total"])
	}
	if hint, _ := obj["hint"].(string); hint != "narrow with range or env" {
		t.Errorf("hint = %v", obj["hint"])
	}
	items, _ := obj["items"].([]any)
	if len(items) == 0 || len(items) >= 400 {
		t.Errorf("items kept %d, want trimmed but non-empty", len(items))
	}
}

func TestTruncateNamedList(t *testing.T) {
	// LogPage shape: object with a "lines" list.
	doc := map[string]any{
		"lines":      bigDoc(t, 300, 1000),
		"nextBefore": float64(1730000000000),
	}
	out, err := Truncate(doc, []string{"lines"}, "narrow service= or level=")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if len(b) > MaxResultBytes {
		t.Errorf("result %d bytes exceeds cap %d", len(b), MaxResultBytes)
	}
	obj := out.(map[string]any)
	if obj["truncated"] != true {
		t.Errorf("truncated marker missing")
	}
	if obj["total"] != 300 {
		t.Errorf("total = %v, want 300", obj["total"])
	}
	lines, _ := obj["lines"].([]any)
	if len(lines) == 0 || len(lines) >= 300 {
		t.Errorf("lines kept %d, want trimmed but non-empty", len(lines))
	}
	// The non-list sibling field survives.
	if _, has := obj["nextBefore"]; !has {
		t.Errorf("nextBefore dropped by truncation")
	}
}

func TestTruncateOversizedObjectNoList(t *testing.T) {
	// A single object too big to trim (no list field): must be marked, not fail.
	doc := map[string]any{"blob": strings.Repeat("y", MaxResultBytes+1)}
	out, err := Truncate(doc, []string{"spans"}, "narrow")
	if err != nil {
		t.Fatal(err)
	}
	obj := out.(map[string]any)
	if obj["truncated"] != true {
		t.Errorf("oversized no-list object must carry truncated marker")
	}
	if _, has := obj["hint"]; !has {
		t.Errorf("oversized no-list object must carry hint")
	}
}

func TestTraceSpans(t *testing.T) {
	tests := []struct {
		name      string
		spans     int
		wantTrim  bool
		wantTotal int
	}{
		{"under cap untouched", 50, false, 0},
		{"exactly max untouched", MaxTraceSpans, false, 0},
		{"over cap trimmed", MaxTraceSpans + 50, true, MaxTraceSpans + 50},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spans := make([]any, tc.spans)
			for i := range spans {
				spans[i] = map[string]any{"id": i}
			}
			doc := map[string]any{"id": "0af7651916cd43dd8448eb211c80319c", "spans": spans}
			out, err := TraceSpans(doc, MaxTraceSpans, "narrow the trace query")
			if err != nil {
				t.Fatal(err)
			}
			obj := out.(map[string]any)
			got, _ := obj["spans"].([]any)
			_, marked := obj["truncated"]
			if tc.wantTrim {
				if len(got) != MaxTraceSpans {
					t.Errorf("spans kept %d, want %d", len(got), MaxTraceSpans)
				}
				if !marked {
					t.Errorf("truncated marker missing")
				}
				if obj["total"] != tc.wantTotal {
					t.Errorf("total = %v, want %d", obj["total"], tc.wantTotal)
				}
			} else {
				if len(got) != tc.spans {
					t.Errorf("spans kept %d, want %d", len(got), tc.spans)
				}
				if marked {
					t.Errorf("untouched trace must not be marked truncated")
				}
			}
		})
	}
}

func TestLimitValue(t *testing.T) {
	pi := func(n int) *int { return &n }
	tests := []struct {
		name string
		n    *int
		def  int
		max  int
		want int
	}{
		{"nil uses default", nil, 20, 100, 20},
		{"zero uses default", pi(0), 20, 100, 20},
		{"negative uses default", pi(-3), 20, 100, 20},
		{"in range passes", pi(35), 20, 100, 35},
		{"over max clamps", pi(9999), 20, 100, 100},
		{"default above max clamps", nil, 500, 200, 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := LimitValue(tc.n, tc.def, tc.max); got != tc.want {
				t.Errorf("LimitValue(%v, %d, %d) = %d, want %d", tc.n, tc.def, tc.max, got, tc.want)
			}
		})
	}
}

func TestClampInt(t *testing.T) {
	if got := ClampInt(150, 1, 100); got != 100 {
		t.Errorf("ClampInt(150,1,100) = %d", got)
	}
	if got := ClampInt(0, 1, 100); got != 1 {
		t.Errorf("ClampInt(0,1,100) = %d", got)
	}
	if got := ClampInt(7, 1, 100); got != 7 {
		t.Errorf("ClampInt(7,1,100) = %d", got)
	}
}
