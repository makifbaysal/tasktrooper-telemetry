// Package obs is a dependency-free Prometheus text-format registry for /metrics.
package obs

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Registry holds counters and gauges keyed by name + labels.
type Registry struct {
	mu     sync.Mutex
	values map[string]float64
	types  map[string]string
}

// New returns an empty registry.
func New() *Registry { return &Registry{values: map[string]float64{}, types: map[string]string{}} }

func key(name string, labels []string) string {
	if len(labels) == 0 {
		return name
	}
	parts := make([]string, 0, len(labels)/2)
	for i := 0; i+1 < len(labels); i += 2 {
		parts = append(parts, fmt.Sprintf("%s=%q", labels[i], labels[i+1]))
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

// Add increments a counter.
func (r *Registry) Add(name string, v float64, labels ...string) {
	r.mu.Lock()
	r.types[name] = "counter"
	r.values[key(name, labels)] += v
	r.mu.Unlock()
}

// Set sets a gauge.
func (r *Registry) Set(name string, v float64, labels ...string) {
	r.mu.Lock()
	r.types[name] = "gauge"
	r.values[key(name, labels)] = v
	r.mu.Unlock()
}

// Get returns a value (tests).
func (r *Registry) Get(name string, labels ...string) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.values[key(name, labels)]
}

// ServeHTTP writes the exposition format.
func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	keys := make([]string, 0, len(r.values))
	for k := range r.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	typed := map[string]bool{}
	for _, k := range keys {
		name, _, _ := strings.Cut(k, "{")
		if !typed[name] {
			fmt.Fprintf(&b, "# TYPE %s %s\n", name, r.types[name])
			typed[name] = true
		}
		fmt.Fprintf(&b, "%s %g\n", k, r.values[k])
	}
	r.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
}
