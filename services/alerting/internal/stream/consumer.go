package stream

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tasktrooper/telemetry/services/pkg/topics"

	"github.com/tasktrooper/telemetry/services/alerting/internal/eval"
	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
	"github.com/tasktrooper/telemetry/services/alerting/internal/obs"
)

// Engine is what the stream needs from the alert engine.
type Engine interface {
	// StreamLeader: this instance consumes the stream (it holds shard 0; with one shard simply the leader).
	StreamLeader() bool
	StreamMonitors() []*model.Monitor
	ApplyStream(ctx context.Context, m *model.Monitor, res eval.Result)
	EvalNow(ctx context.Context, m *model.Monitor)
}

// Group is the consumer group of the stream rules (separate from tt-ingest-writer: both see every record).
const Group = "tt-alert-stream"

// Trigger delays: ingest-writer flushes at least every second; the second check covers a slow flush.
var triggerDelays = []time.Duration{2 * time.Second, 6 * time.Second}

// Processor matches decoded records against stream monitors. Not safe for concurrent use.
type Processor struct {
	Eng Engine
	Reg *obs.Registry
	Log *slog.Logger
	Now func() time.Time

	windows  map[int64]*Window
	triggers map[int64][]time.Time
	byProj   map[int64][]*model.Monitor
	byID     map[int64]*model.Monitor
}

// NewProcessor returns a processor.
func NewProcessor(eng Engine, reg *obs.Registry, log *slog.Logger) *Processor {
	return &Processor{Eng: eng, Reg: reg, Log: log, Now: time.Now, windows: map[int64]*Window{}, triggers: map[int64][]time.Time{}}
}

// Refresh reloads the stream monitors (call once per poll).
func (p *Processor) Refresh() {
	p.byProj, p.byID = map[int64][]*model.Monitor{}, map[int64]*model.Monitor{}
	for _, m := range p.Eng.StreamMonitors() {
		p.byProj[int64(m.ProjectID)] = append(p.byProj[int64(m.ProjectID)], m)
		p.byID[m.ID] = m
	}
	for id := range p.windows {
		if p.byID[id] == nil {
			delete(p.windows, id)
		}
	}
}

// Wants reports whether records of a project can match anything.
func (p *Processor) Wants(projectID int64) bool { return len(p.byProj[projectID]) > 0 }

func (p *Processor) count(name string, labels ...string) {
	if p.Reg != nil {
		p.Reg.Add(name, 1, labels...)
	}
}

// Logs handles one tt.otlp.logs record.
func (p *Processor) Logs(ctx context.Context, projectID int64, req *collogspb.ExportLogsServiceRequest) {
	ms := p.byProj[projectID]
	if len(ms) == 0 {
		return
	}
	now := p.Now()
	touched := map[int64]bool{}
	for _, rl := range req.GetResourceLogs() {
		svc, env := Resource(rl.GetResource().GetAttributes())
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				rec := LogRec{Service: svc, Env: env, Severity: Severity(lr.GetSeverityNumber(), lr.GetSeverityText()), Body: lr.GetBody().GetStringValue()}
				for _, m := range ms {
					if m.Kind == "error_new" {
						continue
					}
					match, exact := LogMatch(m, rec)
					if !match {
						continue
					}
					p.count("tt_alert_stream_matches_total", "kind", m.Kind)
					if !exact {
						p.trigger(m.ID, now)
						continue
					}
					w := p.windows[m.ID]
					if w == nil {
						w = &Window{}
						p.windows[m.ID] = w
					}
					w.Add(now, 1)
					touched[m.ID] = true
				}
			}
		}
	}
	for id := range touched {
		m := p.byID[id]
		c := eval.Condition(m)
		win := c.Window
		if win < time.Minute {
			win = 5 * time.Minute
		}
		n := p.windows[id].Sum(now, win)
		if c.Breached(n) {
			p.Eng.ApplyStream(ctx, m, eval.Result{Value: n, Breached: true, Detail: map[string]any{"op": c.Op, "threshold": c.Threshold, "source": "stream"}})
			continue
		}
		// the in-memory window only knows what arrived since this instance became leader: ask ClickHouse
		p.trigger(id, now)
	}
}

// Traces handles one tt.otlp.traces record: error spans trigger error_new re-evaluations.
func (p *Processor) Traces(projectID int64, req *coltracepb.ExportTraceServiceRequest) {
	ms := p.byProj[projectID]
	if len(ms) == 0 {
		return
	}
	now := p.Now()
	for _, rs := range req.GetResourceSpans() {
		svc, env := Resource(rs.GetResource().GetAttributes())
		var errSeen bool
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				if IsErrorSpan(sp) {
					errSeen = true
					break
				}
			}
		}
		if !errSeen {
			continue
		}
		for _, m := range ms {
			if m.Kind == "error_new" && ErrorNewMatch(m, SpanErr{Service: svc, Env: env}) {
				p.count("tt_alert_stream_matches_total", "kind", m.Kind)
				p.trigger(m.ID, now)
			}
		}
	}
}

// trigger schedules ClickHouse re-evaluations of a monitor (deduplicated: one pending schedule at a time).
func (p *Processor) trigger(id int64, now time.Time) {
	if len(p.triggers[id]) > 0 {
		return
	}
	for _, d := range triggerDelays {
		p.triggers[id] = append(p.triggers[id], now.Add(d))
	}
}

// Due returns the monitors whose re-evaluation is due at now and consumes those schedule entries.
func (p *Processor) Due(now time.Time) []*model.Monitor {
	var out []*model.Monitor
	for id, ts := range p.triggers {
		fired := false
		for len(ts) > 0 && !ts[0].After(now) {
			ts, fired = ts[1:], true
		}
		if len(ts) == 0 {
			delete(p.triggers, id)
		} else {
			p.triggers[id] = ts
		}
		if m := p.byID[id]; fired && m != nil {
			out = append(out, m)
		}
	}
	return out
}

// Config of the Kafka consumer.
type Config struct {
	Brokers []string
	Group   string
}

// Run consumes the ingest topics while the engine is leader (joining the group only then, so standbys do not
// take partitions away from the leader) and feeds matches into the engine.
func Run(ctx context.Context, cfg Config, eng Engine, reg *obs.Registry, log *slog.Logger) error {
	if cfg.Group == "" {
		cfg.Group = Group
	}
	p := NewProcessor(eng, reg, log)
	var cl *kgo.Client
	defer func() {
		if cl != nil {
			cl.Close()
		}
	}()
	var evalWG sync.WaitGroup
	defer evalWG.Wait()
	sem := make(chan struct{}, 4)
	for ctx.Err() == nil {
		leader := eng.StreamLeader()
		switch {
		case leader && cl == nil:
			var err error
			cl, err = kgo.NewClient(
				kgo.SeedBrokers(cfg.Brokers...),
				kgo.ConsumerGroup(cfg.Group),
				kgo.ConsumeTopics(topics.Logs, topics.Traces),
				kgo.ConsumeResetOffset(kgo.NewOffset().AtEnd()), // alerts are about now: never replay history
				kgo.FetchMaxWait(500*time.Millisecond),
			)
			if err != nil {
				log.Error("stream: kafka client", "err", err)
				cl = nil
				sleep(ctx, 5*time.Second)
				continue
			}
			log.Info("stream rules consuming", "group", cfg.Group, "topics", []string{topics.Logs, topics.Traces})
		case !leader && cl != nil:
			cl.Close()
			cl = nil
			log.Info("stream rules stopped (not leader)")
		}
		if cl == nil {
			sleep(ctx, 2*time.Second)
			continue
		}
		p.Refresh()
		pctx, cancel := context.WithTimeout(ctx, time.Second)
		fetches := cl.PollFetches(pctx)
		cancel()
		fetches.EachError(func(t string, part int32, err error) {
			if ctx.Err() == nil && err != context.DeadlineExceeded {
				log.Warn("stream fetch", "topic", t, "partition", part, "err", err)
			}
		})
		fetches.EachRecord(func(r *kgo.Record) {
			p.Handle(ctx, r)
		})
		for _, m := range p.Due(p.Now()) {
			evalWG.Add(1)
			sem <- struct{}{}
			go func(m *model.Monitor) {
				defer func() { <-sem; evalWG.Done() }()
				eng.EvalNow(ctx, m)
			}(m)
		}
	}
	return nil
}

// Handle decodes one Kafka record (project from the project_id header) and processes it.
func (p *Processor) Handle(ctx context.Context, r *kgo.Record) {
	var project int64
	for _, h := range r.Headers {
		if h.Key == topics.HeaderProjectID {
			project, _ = strconv.ParseInt(string(h.Value), 10, 64)
		}
	}
	p.count("tt_alert_stream_records_total", "topic", r.Topic)
	if project == 0 || !p.Wants(project) {
		return
	}
	switch r.Topic {
	case topics.Logs:
		var req collogspb.ExportLogsServiceRequest
		if err := proto.Unmarshal(r.Value, &req); err == nil {
			p.Logs(ctx, project, &req)
		}
	case topics.Traces:
		var req coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(r.Value, &req); err == nil {
			p.Traces(project, &req)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
