// Package observe matches the code inventory with observed telemetry (ClickHouse, read-only): `red_1m` routes of
// the last 7 days and `consumer_1m` processes → observed; nothing in 30 days (`red_1h`) → dead; traffic without
// code → shadow endpoints. It also marks items that already have a monitor (contracts §13).
package observe

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/route"
)

// Window is the "observed" window; DeadWindow the "no traffic" window.
const (
	Window     = 7 * 24 * time.Hour
	DeadWindow = 30 * 24 * time.Hour
)

// Endpoint is one observed (service, route, method) over Window.
type Endpoint struct {
	Service, Route, Method string
	Count, Errors, P95     float64
}

// Consumer is one observed (service, process, topic) over Window.
type Consumer struct {
	Service, Process, Topic string
	Count, Failed, P95      float64
}

// Traffic is what a project's telemetry says.
type Traffic struct {
	Endpoints []Endpoint
	Consumers []Consumer
	// Seen30d holds service \x00 canon(route) and service \x00 process keys with traffic in DeadWindow.
	Seen30d map[string]bool
	// Services30d are services with any traffic in DeadWindow (dead is only claimed for instrumented services).
	Services30d map[string]bool
}

// Source loads traffic for a tenant.
type Source interface {
	Traffic(ctx context.Context, orgID, projectID int64) (*Traffic, error)
}

// ClickHouse is the production Source.
type ClickHouse struct{ Conn driver.Conn }

// Open connects to ClickHouse (clickhouse://user:pass@host:9000/db).
func Open(ctx context.Context, url string) (*ClickHouse, error) {
	opts, err := clickhouse.ParseDSN(url)
	if err != nil {
		return nil, fmt.Errorf("clickhouse url: %w", err)
	}
	opts.MaxOpenConns = 4
	opts.DialTimeout = 5 * time.Second
	opts.Settings = clickhouse.Settings{"max_execution_time": 60, "readonly": 1}
	c, err := clickhouse.Open(opts)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.Ping(pctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return &ClickHouse{Conn: c}, nil
}

// Traffic runs the matching queries. Every query filters org_id + project_id.
func (c *ClickHouse) Traffic(ctx context.Context, orgID, projectID int64) (*Traffic, error) {
	t := &Traffic{Seen30d: map[string]bool{}, Services30d: map[string]bool{}}
	org, proj := uint32(orgID), uint32(projectID)
	rows, err := c.Conn.Query(ctx, `SELECT service, route, method, sum(count), sum(errors),
			toFloat64(quantilesTDigestWeightedMerge(0.5, 0.75, 0.95, 0.99)(duration)[3])
		FROM telemetry.red_1m WHERE org_id = ? AND project_id = ? AND minute >= now() - INTERVAL 7 DAY
		GROUP BY service, route, method`, org, proj)
	if err != nil {
		return nil, fmt.Errorf("red_1m: %w", err)
	}
	for rows.Next() {
		var e Endpoint
		if err := rows.Scan(&e.Service, &e.Route, &e.Method, &e.Count, &e.Errors, &e.P95); err != nil {
			rows.Close()
			return nil, err
		}
		t.Endpoints = append(t.Endpoints, e)
	}
	rows.Close()

	rows, err = c.Conn.Query(ctx, `SELECT service, process, topic, sum(processed), sum(failed),
			toFloat64(quantilesTDigestWeightedMerge(0.5, 0.95, 0.99)(duration)[2])
		FROM telemetry.consumer_1m WHERE org_id = ? AND project_id = ? AND minute >= now() - INTERVAL 7 DAY
		GROUP BY service, process, topic`, org, proj)
	if err != nil {
		return nil, fmt.Errorf("consumer_1m: %w", err)
	}
	for rows.Next() {
		var co Consumer
		if err := rows.Scan(&co.Service, &co.Process, &co.Topic, &co.Count, &co.Failed, &co.P95); err != nil {
			rows.Close()
			return nil, err
		}
		t.Consumers = append(t.Consumers, co)
	}
	rows.Close()

	// 30-day presence: red_1h (13-month TTL) when present, else red_1m (30-day TTL).
	if err := c.seen(ctx, t, "red_1h", org, proj); err != nil {
		if err := c.seen(ctx, t, "red_1m", org, proj); err != nil {
			return nil, err
		}
	}
	rows, err = c.Conn.Query(ctx, `SELECT DISTINCT service, process FROM telemetry.consumer_1m
		WHERE org_id = ? AND project_id = ? AND minute >= now() - INTERVAL 30 DAY AND processed > 0`, org, proj)
	if err != nil {
		return nil, fmt.Errorf("consumer_1m 30d: %w", err)
	}
	for rows.Next() {
		var s, p string
		if err := rows.Scan(&s, &p); err != nil {
			rows.Close()
			return nil, err
		}
		t.Seen30d[s+"\x00p\x00"+strings.ToLower(p)] = true
		t.Services30d[s] = true
	}
	rows.Close()
	return t, nil
}

func (c *ClickHouse) seen(ctx context.Context, t *Traffic, table string, org, proj uint32) error {
	rows, err := c.Conn.Query(ctx, `SELECT service, route, method FROM telemetry.`+table+`
		WHERE org_id = ? AND project_id = ? AND minute >= now() - INTERVAL 30 DAY
		GROUP BY service, route, method HAVING sum(count) > 0`, org, proj)
	if err != nil {
		return fmt.Errorf("%s 30d: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var s, r, m string
		if err := rows.Scan(&s, &r, &m); err != nil {
			return err
		}
		t.Seen30d[s+"\x00r\x00"+route.Canon(r)] = true
		t.Services30d[s] = true
	}
	return rows.Err()
}

// Match sets Observed / Dead / Stats on items and returns shadow endpoints: observed routes of the given services
// that no code item explains. Items of other services never produce shadows.
func Match(items []*model.Item, t *Traffic, services []string) []model.Item {
	if t == nil {
		return nil
	}
	minutes := Window.Minutes()
	type epKey struct{ service, canon string }
	byRoute := map[epKey][]int{}
	for i, e := range t.Endpoints {
		k := epKey{e.Service, route.Canon(e.Route)}
		byRoute[k] = append(byRoute[k], i)
	}
	used := make([]bool, len(t.Endpoints))
	for _, it := range items {
		it.Observed, it.Dead = false, false
		it.Stats = nil
		switch it.Kind {
		case model.KindEndpoint:
			method, p := route.SplitKey(it.Key)
			canon := route.Canon(p)
			var st model.Stats
			for _, i := range byRoute[epKey{it.Service, canon}] {
				e := t.Endpoints[i]
				if !route.MethodMatches(method, e.Method) {
					continue
				}
				used[i] = true
				st.Count += e.Count
				st.Errors += e.Errors
				st.P95 = max(st.P95, e.P95)
				if st.Route == "" {
					st.Route, st.Method = e.Route, e.Method
				}
			}
			if st.Count > 0 {
				it.Observed = true
				st.RPM = st.Count / minutes
				st.ErrorRate = st.Errors / st.Count
				it.Stats = &st
			} else if t.Services30d[it.Service] && !t.Seen30d[it.Service+"\x00r\x00"+canon] {
				it.Dead = true
			}
		case model.KindConsumer, model.KindCron:
			key := strings.ToLower(it.Key)
			sources := map[string]bool{}
			for _, s := range strings.Split(strings.ToLower(it.Source), ",") {
				if s != "" {
					sources[s] = true
				}
			}
			var st model.Stats
			for _, c := range t.Consumers {
				if c.Service != it.Service {
					continue
				}
				if strings.ToLower(c.Process) == key || (it.Kind == model.KindConsumer && sources[strings.ToLower(c.Topic)]) {
					st.Count += c.Count
					st.Errors += c.Failed
					st.P95 = max(st.P95, c.P95)
					if st.Route == "" {
						st.Route = c.Process
					}
				}
			}
			if st.Count > 0 {
				it.Observed = true
				st.RPM = st.Count / minutes
				st.ErrorRate = st.Errors / st.Count
				it.Stats = &st
			} else if it.Kind == model.KindConsumer && t.Services30d[it.Service] && !t.Seen30d[it.Service+"\x00p\x00"+key] {
				it.Dead = true
			}
		}
	}
	inRepo := map[string]bool{}
	for _, s := range services {
		inRepo[s] = true
	}
	agg := map[string]*model.Item{}
	for i, e := range t.Endpoints {
		if used[i] || !inRepo[e.Service] || !strings.HasPrefix(e.Route, "/") || e.Count <= 0 {
			continue
		}
		method := strings.ToUpper(e.Method)
		if method == "" {
			method = "ANY"
		}
		key := method + " " + route.Observed(e.Route)
		id := e.Service + "\x00" + key
		s, ok := agg[id]
		if !ok {
			s = &model.Item{Kind: model.KindEndpoint, Key: key, Service: e.Service, Observed: true, Shadow: true,
				Criticality: model.Low, Stats: &model.Stats{Route: e.Route, Method: e.Method}, Meta: map[string]any{}}
			agg[id] = s
		}
		s.Stats.Count += e.Count
		s.Stats.Errors += e.Errors
		s.Stats.P95 = max(s.Stats.P95, e.P95)
	}
	out := make([]model.Item, 0, len(agg))
	for _, s := range agg {
		s.Stats.RPM = s.Stats.Count / minutes
		s.Stats.ErrorRate = s.Stats.Errors / s.Stats.Count
		s.Rationale = "observed traffic without a code handler"
		s.ComputeHash()
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service+out[i].Key < out[j].Service+out[j].Key })
	return out
}

// MonitorRef is the part of a monitor that says what it watches (monitors.target / query).
type MonitorRef struct {
	Service, Endpoint, Process, Route string
}

// MarkMonitored sets Monitored on items covered by any monitor: same service and the same endpoint (canonical
// route, compatible method) or process.
func MarkMonitored(items []*model.Item, mons []MonitorRef) {
	for _, it := range items {
		it.Monitored = false
		method, p := route.SplitKey(it.Key)
		canon := route.Canon(p)
		for _, m := range mons {
			if m.Service != "" && m.Service != it.Service {
				continue
			}
			if m.Service == "" && m.Endpoint == "" && m.Process == "" && m.Route == "" {
				continue
			}
			switch it.Kind {
			case model.KindEndpoint:
				for _, ref := range []string{m.Endpoint, m.Route} {
					if ref == "" {
						continue
					}
					mm, mp := route.SplitKey(ref)
					if route.Canon(mp) == canon && (mm == "ANY" || route.MethodMatches(method, mm)) {
						it.Monitored = true
					}
				}
			case model.KindConsumer, model.KindCron:
				if strings.EqualFold(m.Process, it.Key) || strings.EqualFold(m.Endpoint, it.Key) {
					it.Monitored = true
				}
			case model.KindView:
				if strings.EqualFold(m.Endpoint, it.Key) || strings.EqualFold(m.Route, it.Key) {
					it.Monitored = true
				}
			}
			if it.Monitored {
				break
			}
		}
	}
}
