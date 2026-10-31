package apiclient

import (
	"encoding/json"
	"fmt"
)

// MaxResultBytes caps every tool result (ADR-025, contracts §26). A truncated
// result carries truncated=true, the original item count in total, and a hint
// telling the agent how to narrow the query.
const MaxResultBytes = 64 << 10

// MaxTraceSpans caps the span list of tt_get_trace independently of the byte cap.
const MaxTraceSpans = 200

// Truncate fits a decoded JSON document under MaxResultBytes by trimming the
// named list (the first of fields that exists), then returns it re-encoded.
//
// The original document must already be decoded into a generic value (any). If
// it fits, it is returned unchanged. Otherwise the chosen list is trimmed until
// the encoded form fits, and the top-level object gains:
//
//	truncated: true   total: <original item count>   hint: <how to narrow>
//
// hint is passed by the caller because only the tool knows which query knobs
// exist (e.g. "raise minMs or narrow service="). A bare array document has no
// object to attach markers to, so it is wrapped as
// {"items": [...], "truncated": true, ...}.
func Truncate(doc any, fields []string, hint string) (any, error) {
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	if len(b) <= MaxResultBytes {
		return doc, nil
	}

	obj, isObj := doc.(map[string]any)
	list, key := findList(doc, fields)
	if key == "" && !isObj {
		// Bare array document: wrap it so the markers have somewhere to live.
		obj = map[string]any{"items": doc}
		key = "items"
		list, _ = obj[key].([]any)
	}
	if key == "" || len(list) == 0 {
		// Oversized object with no list to trim (e.g. a huge single trace): keep
		// the document whole but mark it so the agent knows it is not the full
		// payload, rather than failing the call.
		obj["truncated"] = true
		obj["hint"] = hint
		return obj, nil
	}
	total := len(list)

	// Reserve room for the marker keys by targeting a slightly smaller payload.
	target := MaxResultBytes - 256
	for len(list) > 0 {
		obj[key] = list
		obj["truncated"] = true
		obj["total"] = total
		obj["hint"] = hint
		b, err = json.Marshal(obj)
		if err != nil {
			return nil, fmt.Errorf("encode: %w", err)
		}
		if len(b) <= target {
			return obj, nil
		}
		list = list[:len(list)/2]
	}
	obj[key] = []any{}
	return obj, nil
}

// TraceSpans trims a decoded trace document's spans list to at most maxSpan items,
// recording truncation the same way Truncate does.
func TraceSpans(doc any, maxSpan int, hint string) (any, error) {
	obj, ok := doc.(map[string]any)
	if !ok {
		return doc, nil
	}
	spans, ok := obj["spans"].([]any)
	if !ok || len(spans) <= maxSpan {
		return doc, nil
	}
	total := len(spans)
	obj["spans"] = spans[:maxSpan]
	obj["truncated"] = true
	obj["total"] = total
	obj["hint"] = hint
	return obj, nil
}

// findList returns the first of fields that holds a JSON array in doc, plus its key.
func findList(doc any, fields []string) ([]any, string) {
	obj, ok := doc.(map[string]any)
	if !ok {
		return nil, ""
	}
	for _, f := range fields {
		if l, ok := obj[f].([]any); ok {
			return l, f
		}
	}
	return nil, ""
}

// LimitValue returns the effective list limit: def when the caller omitted the
// field (or sent 0), otherwise n, both clamped to [1, max].
func LimitValue(n *int, def, max int) int {
	if n == nil || *n <= 0 {
		return ClampInt(def, 1, max)
	}
	return ClampInt(*n, 1, max)
}
