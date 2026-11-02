package assistant

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// WidgetTool is the assistant-only tool that renders a widget in the chat (.ai/assistant.md § Widgets).
const WidgetTool = "show_widget"

var widgetKinds = map[string]bool{
	"service": true, "endpoint": true, "incident": true, "kpi": true, "monitor": true, "trace": true,
	"loadtest_run": true, "logs": true, "chart": true, "table": true, "stats": true,
}

// WidgetDef is the show_widget tool definition.
func WidgetDef() ToolDef {
	str := map[string]any{"type": "string"}
	return ToolDef{
		Name: WidgetTool,
		Description: "Show a widget in the chat next to your answer; the user can click it to open the screen. " +
			"Entity kinds take references (the UI loads live data): service {service}, endpoint {service, endpoint: \"GET /route\"}, " +
			"incident {id}, kpi {id}, monitor {id, service?}, trace {traceId}, loadtest_run {id}, logs {service?, query?, level?}. " +
			"Data kinds carry their data: chart {title, unit: ms|percent (values as ratios 0..1)|count|rpm|bytes, series: [{label, points: [[unixMs, value]]}]} (max 4 series, single y-axis), " +
			"table {title, columns, rows: [[cell...]], links?: [dashboard path per row]} (max 50 rows), " +
			"stats {title?, items: [{label, value, status?: ok|warning|critical}]} (max 6). " +
			"link is an optional dashboard path starting with /. Use widgets for the items your answer is about; do not repeat their numbers at length in text.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"kind":     map[string]any{"type": "string", "enum": []string{"service", "endpoint", "incident", "kpi", "monitor", "trace", "loadtest_run", "logs", "chart", "table", "stats"}},
				"title":    str,
				"service":  str,
				"endpoint": str,
				"id":       str,
				"traceId":  str,
				"query":    str,
				"level":    str,
				"unit":     str,
				"link":     str,
				"series": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
					"label":  str,
					"points": map[string]any{"type": "array", "items": map[string]any{"type": "array", "items": map[string]any{"type": "number"}}},
				}}},
				"columns": map[string]any{"type": "array", "items": str},
				"rows":    map[string]any{"type": "array", "items": map[string]any{"type": "array", "items": str}},
				"links":   map[string]any{"type": "array", "items": str},
				"items": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
					"label": str, "value": str, "status": map[string]any{"type": "string", "enum": []string{"ok", "warning", "critical"}},
				}}},
			},
			"required": []any{"kind"},
		},
	}
}

// internalPath accepts dashboard paths only ("/x", not "//host" or a URL).
func internalPath(s string) bool {
	return strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "//") && !strings.ContainsAny(s, "\\\r\n")
}

// SanitizeWidget validates a show_widget input and returns the JSON stored and sent to the UI.
func SanitizeWidget(raw json.RawMessage) (json.RawMessage, error) {
	var w map[string]any
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, errors.New("widget input must be a JSON object")
	}
	kind, _ := w["kind"].(string)
	if !widgetKinds[kind] {
		return nil, fmt.Errorf("unknown widget kind %q", kind)
	}
	if l, ok := w["link"].(string); ok && !internalPath(l) {
		delete(w, "link")
	}
	need := func(fields ...string) error {
		for _, f := range fields {
			if s, _ := w[f].(string); strings.TrimSpace(s) == "" {
				return fmt.Errorf("widget %s needs %s", kind, f)
			}
		}
		return nil
	}
	var err error
	switch kind {
	case "service":
		err = need("service")
	case "endpoint":
		err = need("service", "endpoint")
	case "incident", "kpi", "monitor", "loadtest_run":
		err = need("id")
	case "trace":
		err = need("traceId")
	case "chart":
		series, _ := w["series"].([]any)
		if len(series) == 0 {
			return nil, errors.New("chart needs series")
		}
		if len(series) > 4 {
			w["series"] = series[:4]
		}
		for _, s := range series[:min(len(series), 4)] {
			if m, ok := s.(map[string]any); ok {
				if pts, ok := m["points"].([]any); ok && len(pts) > 500 {
					m["points"] = pts[len(pts)-500:]
				}
			}
		}
	case "table":
		rows, _ := w["rows"].([]any)
		if len(rows) > 50 {
			w["rows"] = rows[:50]
		}
		if links, ok := w["links"].([]any); ok {
			for i, l := range links {
				if s, _ := l.(string); !internalPath(s) {
					links[i] = ""
				}
			}
		}
	case "stats":
		items, _ := w["items"].([]any)
		if len(items) == 0 {
			return nil, errors.New("stats needs items")
		}
		if len(items) > 6 {
			w["items"] = items[:6]
		}
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(w)
}
