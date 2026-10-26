package scrape

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/tasktrooper/telemetry/services/collector/internal/config"
	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
)

// ErrDeferred marks engines the collector does not support yet.
var ErrDeferred = errors.New("not supported yet")

// HostOptions control the optional host scraper.
type HostOptions struct {
	// Enabled reports host CPU/memory/disk for agent-mode databases (--host-metrics).
	Enabled  bool
	DiskPath string
}

// NewDatabase builds the scraper for a database target.
func NewDatabase(t config.Database, host HostOptions, log *slog.Logger) (Scraper, error) {
	var s Scraper
	var err error
	switch t.Engine {
	case "postgres":
		s, err = NewPostgres(t, log)
	case "mysql":
		s, err = NewMySQL(t, log)
	case "redis":
		s, err = NewRedis(t, log)
	case "mongodb":
		s, err = NewMongoDB(t, log)
	default:
		return nil, fmt.Errorf("unknown engine %q", t.Engine)
	}
	if err != nil {
		return nil, err
	}
	useHost := host.Enabled && t.Collector == "agent"
	if t.HostMetrics != nil {
		useHost = *t.HostMetrics
	}
	if !useHost {
		return s, nil
	}
	w := &withHost{db: s, role: "primary"}
	w.host = NewHost(t, host.DiskPath, w.lastRole, log)
	return w, nil
}

// NewBroker builds the scraper for a broker cluster.
func NewBroker(t config.Broker, log *slog.Logger) (Scraper, error) {
	switch t.Kind {
	case "kafka":
		return NewKafka(t, log)
	case "rabbitmq":
		return NewRabbitMQ(t, log)
	}
	return nil, fmt.Errorf("unknown broker kind %q", t.Kind)
}

// withHost runs the database scraper, then the host scraper with the role it reported.
type withHost struct {
	db   Scraper
	host *Host
	mu   sync.Mutex
	role string
}

func (w *withHost) lastRole() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.role
}

func (w *withHost) Scrape(ctx context.Context) (emit.Result, error) {
	r, err := w.db.Scrape(ctx)
	if err == nil {
		for _, g := range r.Gauges {
			if v, ok := g.Attrs.Value(emit.AttrDBRole); ok && g.Name == emit.DBConnections {
				w.mu.Lock()
				w.role = v.AsString()
				w.mu.Unlock()
				break
			}
		}
	}
	hr, herr := w.host.Scrape(ctx)
	// the engine's own value wins (Redis reports memory against maxmemory)
	have := map[string]bool{}
	for _, g := range r.Gauges {
		have[g.Name] = true
	}
	for _, g := range hr.Gauges {
		if !have[g.Name] {
			r.Gauges = append(r.Gauges, g)
		}
	}
	if err != nil {
		return r, err
	}
	return r, herr
}

func (w *withHost) Close() error { return w.db.Close() }
