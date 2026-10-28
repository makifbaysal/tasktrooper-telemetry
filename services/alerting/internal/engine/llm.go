package engine

import (
	"github.com/tasktrooper/telemetry/services/alerting/internal/eval"
	"github.com/tasktrooper/telemetry/services/alerting/internal/rca"
)

// llmCause turns an llm monitor's detail into the incident's root cause: the summary ("LLM cost $34.20/h above
// $20/h — claude-sonnet-4-5 in storefront-bff 81%") with the top contributor as the root node and its share of the
// value as the confidence. nil without a summary.
func llmCause(res eval.Result) *rca.Result {
	s, _ := res.Detail["summary"].(string)
	if s == "" {
		return nil
	}
	unit := rca.UnitRatio
	if mt, _ := res.Detail["metric"].(string); mt == "p95_ms" || mt == "ttft_p95_ms" {
		unit = rca.UnitMs
	}
	out := &rca.Result{Unit: unit, Path: []rca.Hop{}, Summary: s, Root: &rca.Node{ID: "llm", Kind: "external", Label: "LLM"}}
	if share, ok := res.Detail["topShare"].(float64); ok {
		out.Confidence = share
	}
	if cs, _ := res.Detail["contributors"].([]map[string]any); len(cs) > 0 {
		c := cs[0]
		provider, _ := c["provider"].(string)
		mdl, _ := c["model"].(string)
		svc, _ := c["service"].(string)
		v, _ := c["value"].(float64)
		out.Root = &rca.Node{ID: "llm:" + provider + "/" + mdl, Kind: "external", Label: provider + " · " + mdl, Service: svc, Current: v}
	}
	return out
}
