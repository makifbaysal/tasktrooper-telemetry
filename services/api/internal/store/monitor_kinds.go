package store

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// Monitor kinds infra (ADR-019, contracts §20) and faas (ADR-020, §21): their query shapes are validated here so
// alert-engine never sees a rule it cannot evaluate.

// InfraMetrics are the metrics of each infra monitor entity.
var InfraMetrics = map[string][]string{
	"host":      {"cpu", "memory", "disk", "load1"},
	"container": {"cpu_ratio", "memory_ratio", "restarts", "throttled"},
	"pod":       {"cpu_ratio", "memory_ratio", "restarts", "throttled", "not_ready"},
	"node":      {"not_ready"},
	"workload":  {"unavailable"},
}

// FaasMetrics are the faas monitor metrics.
var FaasMetrics = []string{"invocations", "errors", "error_rate", "throttles", "duration_p95", "duration_p99", "cold_starts", "concurrency", "iterator_age_ms"}

var infraFilterKeys = map[string]bool{"cluster": true, "namespace": true, "host": true, "workload": true, "service": true}
var faasFilterKeys = map[string]bool{"provider": true, "region": true, "service": true}

func strictDecode(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func stringFilter(raw json.RawMessage, allowed map[string]bool) (map[string]string, error) {
	f := map[string]string{}
	if len(raw) == 0 || string(raw) == "null" {
		return f, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, Invalid("query.filter must be an object")
	}
	var keys []string
	for k := range allowed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for k, v := range m {
		if !allowed[k] {
			return nil, Invalid("query.filter." + k + " is not supported (" + strings.Join(keys, ", ") + ")")
		}
		s, ok := v.(string)
		if !ok && v != nil {
			return nil, Invalid("query.filter." + k + " must be a string")
		}
		f[k] = s
	}
	return f, nil
}

// validateInfraQuery checks {entity, metric, filter: {cluster?, namespace?, host?, workload?, service?}, agg: max|avg}.
func validateInfraQuery(raw json.RawMessage) error {
	var q struct {
		Entity string          `json:"entity"`
		Metric string          `json:"metric"`
		Filter json.RawMessage `json:"filter"`
		Agg    string          `json:"agg"`
	}
	if err := strictDecode(raw, &q); err != nil {
		return Invalid("infra query must be {entity, metric, filter?, agg?}: " + err.Error())
	}
	metrics, ok := InfraMetrics[q.Entity]
	if !ok {
		return Invalid("query.entity must be host|container|pod|node|workload")
	}
	valid := false
	for _, m := range metrics {
		valid = valid || m == q.Metric
	}
	if !valid {
		return Invalid("query.metric for " + q.Entity + " must be " + strings.Join(metrics, "|"))
	}
	if q.Agg != "" && q.Agg != "max" && q.Agg != "avg" {
		return Invalid("query.agg must be max|avg")
	}
	_, err := stringFilter(q.Filter, infraFilterKeys)
	return err
}

// validateFaasQuery checks {functionId? | filter: {provider?, region?, service?}, metric, relativeToTimeout?}.
func validateFaasQuery(raw json.RawMessage) error {
	var q struct {
		FunctionID        json.RawMessage `json:"functionId"`
		Filter            json.RawMessage `json:"filter"`
		Metric            string          `json:"metric"`
		RelativeToTimeout *bool           `json:"relativeToTimeout"`
	}
	if err := strictDecode(raw, &q); err != nil {
		return Invalid("faas query must be {functionId? | filter?, metric, relativeToTimeout?}: " + err.Error())
	}
	if q.RelativeToTimeout != nil && *q.RelativeToTimeout && q.Metric != "duration_p95" && q.Metric != "duration_p99" {
		return Invalid("query.relativeToTimeout only applies to duration_p95 | duration_p99")
	}
	valid := false
	for _, m := range FaasMetrics {
		valid = valid || m == q.Metric
	}
	if !valid {
		return Invalid("query.metric must be " + strings.Join(FaasMetrics, "|"))
	}
	hasID := len(q.FunctionID) > 0 && string(q.FunctionID) != "null" && string(q.FunctionID) != `""`
	if hasID {
		var s string
		var n json.Number
		if json.Unmarshal(q.FunctionID, &s) != nil && json.Unmarshal(q.FunctionID, &n) != nil {
			return Invalid("query.functionId must be a string or number")
		}
	}
	f, err := stringFilter(q.Filter, faasFilterKeys)
	if err != nil {
		return err
	}
	if hasID && len(f) > 0 {
		return Invalid("query takes functionId or filter, not both")
	}
	if p := f["provider"]; p != "" && p != "aws" && p != "azure" {
		return Invalid("query.filter.provider must be aws|azure")
	}
	return nil
}
