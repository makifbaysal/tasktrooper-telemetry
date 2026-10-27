// Package ascode reads alert-as-code monitors from `.trooper/monitors/*.yaml` (alerting.md § Kural şeması) and
// translates them to the monitors columns the api writes (services/api/internal/store/alerting.go normalizeMonitor):
// target {service, endpoint, process, view, env}, query (kind specific), condition {op, threshold, forMin, window,
// minRequests, renotifyMin}, severity, channels, status.
package ascode

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Dir is where monitors live in a repository.
const Dir = ".trooper/monitors"

// Kinds the api accepts (services/api/internal/store/alerting.go MonitorKinds).
var Kinds = map[string]bool{
	"metric_threshold": true, "latency": true, "throughput_change": true, "anomaly": true, "log_match": true,
	"log_pattern_new": true, "error_new": true, "consumer_lag": true, "job_missed": true, "rum_vital": true, "kpi_burn": true,
	"error_rate": true, "throughput": true, "apdex": true, "log_count": true, "log_pattern": true, "dlq": true,
	"consumer_failures": true, "crash_rate": true, "composite": true,
	"infra": true, "faas": true, "llm": true,
}

var lagMetrics = map[string]bool{"lag": true, "time_behind": true, "partition_lag": true, "growing": true, "not_drained": true,
	"rebalances": true, "oldest_age": true}

// Monitor is one monitor ready to be written (jsonb columns as Go values).
type Monitor struct {
	Path      string // repo-relative file
	Name      string
	Kind      string
	Target    map[string]string
	Query     map[string]any
	Condition map[string]any
	Severity  string
	Channels  []string
	Status    string // active | muted | draft
}

// Def is the YAML shape.
type Def struct {
	Name      string            `yaml:"name"`
	Kind      string            `yaml:"kind"`
	Query     map[string]any    `yaml:"query"`
	Condition map[string]any    `yaml:"condition"`
	Severity  string            `yaml:"severity"`
	Notify    []string          `yaml:"notify"`
	Labels    map[string]string `yaml:"labels"`
	Status    string            `yaml:"status"`
	Enabled   *bool             `yaml:"enabled"`
}

// FileError is a file that failed to parse; its monitors are kept as they are (not deleted).
type FileError struct {
	Path string
	Err  error
}

func (e FileError) Error() string { return e.Path + ": " + e.Err.Error() }

// Load reads every *.yaml/*.yml under root/.trooper/monitors. Broken files are returned as errors and skipped.
func Load(root string) (mons []Monitor, broken []FileError) {
	dir := filepath.Join(root, Dir)
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if ext := filepath.Ext(p); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		b, err := os.ReadFile(p)
		if err != nil {
			broken = append(broken, FileError{rel, err})
			return nil
		}
		m, err := Parse(rel, b)
		if err != nil {
			broken = append(broken, FileError{rel, err})
			return nil
		}
		mons = append(mons, m...)
		return nil
	})
	sort.Slice(mons, func(i, j int) bool { return mons[i].Path+mons[i].Name < mons[j].Path+mons[j].Name })
	return mons, broken
}

// Parse reads one file: a single monitor, a YAML list, `monitors: [...]`, or several `---` documents.
func Parse(path string, b []byte) ([]Monitor, error) {
	var defs []Def
	dec := yaml.NewDecoder(bytes.NewReader(b))
	for {
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if len(node.Content) == 0 {
			continue
		}
		root := node.Content[0]
		switch root.Kind {
		case yaml.SequenceNode:
			var ds []Def
			if err := root.Decode(&ds); err != nil {
				return nil, err
			}
			defs = append(defs, ds...)
		case yaml.MappingNode:
			var wrap struct {
				Monitors []Def `yaml:"monitors"`
			}
			if err := root.Decode(&wrap); err == nil && len(wrap.Monitors) > 0 {
				defs = append(defs, wrap.Monitors...)
				continue
			}
			var d Def
			if err := root.Decode(&d); err != nil {
				return nil, err
			}
			defs = append(defs, d)
		}
	}
	out := make([]Monitor, 0, len(defs))
	seen := map[string]bool{}
	for i, d := range defs {
		m, err := Translate(path, d)
		if err != nil {
			return nil, fmt.Errorf("monitor #%d: %w", i+1, err)
		}
		if seen[m.Name] {
			return nil, fmt.Errorf("duplicate monitor name %q", m.Name)
		}
		seen[m.Name] = true
		out = append(out, m)
	}
	return out, nil
}

// Translate maps the YAML rule to monitor columns.
func Translate(path string, d Def) (Monitor, error) {
	m := Monitor{Path: path, Name: strings.TrimSpace(d.Name), Kind: d.Kind, Target: map[string]string{}, Query: map[string]any{},
		Condition: map[string]any{}, Severity: d.Severity, Channels: d.Notify, Status: "active"}
	if m.Name == "" {
		return m, errors.New("name is required")
	}
	if !Kinds[m.Kind] {
		return m, fmt.Errorf("unknown kind %q", m.Kind)
	}
	if m.Severity == "" {
		m.Severity = "warning"
	}
	if m.Severity != "critical" && m.Severity != "warning" && m.Severity != "info" {
		return m, fmt.Errorf("severity must be critical|warning|info, got %q", m.Severity)
	}
	if m.Channels == nil {
		m.Channels = []string{}
	}
	switch {
	case d.Enabled != nil && !*d.Enabled:
		m.Status = "draft"
	case d.Status != "":
		if d.Status != "active" && d.Status != "muted" && d.Status != "draft" {
			return m, fmt.Errorf("status must be active|muted|draft")
		}
		m.Status = d.Status
	}
	for k, v := range d.Query {
		switch k {
		case "source": // red | logs | consumers … is implied by kind
		case "service", "env", "process", "view":
			m.Target[k] = fmt.Sprint(v)
			if k == "process" {
				m.Query[k] = v
			}
		case "route", "endpoint":
			m.Target["endpoint"] = fmt.Sprint(v)
			if k == "route" {
				m.Query["route"] = v // job_missed reads query.route
			}
		default:
			m.Query[k] = v
		}
	}
	if len(d.Labels) > 0 {
		m.Query["labels"] = d.Labels
	}
	c := d.Condition
	op, _ := c["op"].(string)
	if op == "" {
		op = ">"
	}
	switch op {
	case ">", "<", ">=", "<=":
	default:
		return m, fmt.Errorf("condition.op must be > < >= <=")
	}
	m.Condition["op"] = op
	if v, ok := num(c["threshold"]); ok {
		m.Condition["threshold"] = v
	} else {
		m.Condition["threshold"] = 0.0
	}
	forMin := 0.0
	if v, ok := minutes(c["for"]); ok {
		forMin = v
	}
	if v, ok := minutes(c["forMin"]); ok {
		forMin = v
	}
	m.Condition["forMin"] = forMin
	if w, ok := c["window"]; ok {
		m.Condition["window"] = w
	}
	for _, k := range []string{"min_requests", "minRequests"} {
		if v, ok := num(c[k]); ok {
			m.Condition["minRequests"] = v
		}
	}
	for _, k := range []string{"renotify", "renotifyMin", "renotify_min"} {
		if v, ok := minutes(c[k]); ok {
			m.Condition["renotifyMin"] = v
		}
	}
	if s, ok := c["stream"].(bool); ok {
		m.Condition["stream"] = s
	}
	if m.Kind == "consumer_lag" {
		// QueueRule {clusterId, group, topic?, metric, threshold, forMin}; condition mirrors it (api normalizeMonitor).
		cluster, _ := m.Query["clusterId"].(string)
		group, _ := m.Query["group"].(string)
		metric, _ := m.Query["metric"].(string)
		if cluster == "" || group == "" || !lagMetrics[metric] {
			return m, errors.New("consumer_lag needs query.clusterId, query.group and query.metric lag|time_behind|partition_lag|growing|not_drained|rebalances|oldest_age")
		}
		th, ok := num(m.Query["threshold"])
		if !ok {
			th = m.Condition["threshold"].(float64)
		}
		fm, ok := minutes(m.Query["forMin"])
		if !ok {
			fm = forMin
		}
		if metric == "growing" {
			th = 0
		}
		m.Query["threshold"], m.Query["forMin"] = th, fm
		m.Condition = map[string]any{"op": ">", "threshold": th, "forMin": fm}
	}
	return m, nil
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case string:
		s := strings.TrimSpace(x)
		if p, ok := strings.CutSuffix(s, "%"); ok {
			f, err := strconv.ParseFloat(p, 64)
			return f / 100, err == nil
		}
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	}
	return 0, false
}

// minutes reads "5m" / "1h30m" / "90s" / a number of minutes.
func minutes(v any) (float64, bool) {
	if s, ok := v.(string); ok {
		if d, err := time.ParseDuration(strings.TrimSpace(s)); err == nil {
			return d.Minutes(), true
		}
	}
	return num(v)
}
