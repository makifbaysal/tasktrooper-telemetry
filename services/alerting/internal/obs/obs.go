// Package obs exposes /healthz and a Prometheus text-format /metrics without external dependencies.
package obs

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Registry holds counters and gauges keyed by name + labels.
type Registry struct {
	mu     sync.Mutex
	values map[string]float64
	types  map[string]string
	help   map[string]string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{values: map[string]float64{}, types: map[string]string{}, help: map[string]string{}}
}

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

// Describe sets the type ("counter"|"gauge") and help of a metric.
func (r *Registry) Describe(name, typ, help string) {
	r.mu.Lock()
	r.types[name], r.help[name] = typ, help
	r.mu.Unlock()
}

// Add increments a counter; labels are key, value pairs.
func (r *Registry) Add(name string, v float64, labels ...string) {
	r.mu.Lock()
	r.values[key(name, labels)] += v
	r.mu.Unlock()
}

// Set sets a gauge.
func (r *Registry) Set(name string, v float64, labels ...string) {
	r.mu.Lock()
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
	described := map[string]bool{}
	for _, k := range keys {
		name := k
		if i := strings.IndexByte(k, '{'); i >= 0 {
			name = k[:i]
		}
		if !described[name] {
			described[name] = true
			if h := r.help[name]; h != "" {
				fmt.Fprintf(&b, "# HELP %s %s\n", name, h)
			}
			if t := r.types[name]; t != "" {
				fmt.Fprintf(&b, "# TYPE %s %s\n", name, t)
			}
		}
		fmt.Fprintf(&b, "%s %g\n", k, r.values[k])
	}
	r.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
}

// Serve runs the health/metrics server until ctx ends. health returns nil when healthy.
func Serve(ctx context.Context, addr string, reg *Registry, health func(context.Context) error, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		c, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := health(c); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /metrics", reg)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("http server", "err", err)
	}
}
