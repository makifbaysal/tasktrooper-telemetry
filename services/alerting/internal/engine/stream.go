package engine

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tasktrooper/telemetry/services/alerting/internal/eval"
	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
	"github.com/tasktrooper/telemetry/services/alerting/internal/store"
)

// Stream hooks (internal/stream) and the multi-shard hand-off.
//
// One instance consumes the stream: the holder of shard 0 (StreamLeader). It evaluates every stream rule of every
// shard. A hit on a monitor of a held shard is applied in-process; a hit on another shard's monitor is written to
// alert_stream_hits + NOTIFY tt_stream_hit, and the owning replica (RunStreamHandoff: LISTEN + 1 s poll) claims and
// applies it — escalate only, like local stream hits. Recovery always stays with the periodic loop.

const (
	streamHitMaxAge   = 5 * time.Minute // older unclaimed hits are dropped (alerts are about now)
	streamHitPoll     = time.Second
	streamHitBatch    = 200
	streamPurgeEvery  = time.Minute
	streamHitsMetric  = "tt_alert_stream_handoffs_total"
	streamHitSentDir  = "sent"
	streamHitRecvDir  = "received"
	streamHitDropDir  = "dropped"
	streamHitErrorDir = "error"
)

// Leader reports whether this instance currently holds at least one shard.
func (e *Engine) Leader() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.held) > 0
}

// StreamLeader reports whether this instance consumes the stream (holds shard 0).
func (e *Engine) StreamLeader() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Contains(e.held, 0)
}

func (e *Engine) owns(id int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return store.Shards{N: e.elector.Shards, Held: e.held}.Owns(id)
}

// StreamMonitors returns the stream rules this instance evaluates: its own and, on the stream leader with more than
// one shard, those of every other shard (their hits are handed off).
func (e *Engine) StreamMonitors() []*model.Monitor {
	var out []*model.Monitor
	for _, m := range e.snapshot() {
		if m.Condition.Stream && eval.StreamKind(m.Kind) {
			out = append(out, m)
		}
	}
	e.mu.Lock()
	others := e.streamOthers
	e.mu.Unlock()
	return append(out, others...)
}

// loadStreamOthers refreshes the other shards' stream rules (stream leader, N > 1 only).
func (e *Engine) loadStreamOthers(ctx context.Context) error {
	var others []*model.Monitor
	if e.elector.Shards > 1 && e.StreamLeader() {
		all, err := e.st.LoadStreamMonitors(ctx)
		if err != nil {
			return err
		}
		for _, m := range all {
			if e.inScope(m.ProjectID) && !e.owns(m.ID) {
				others = append(others, m)
			}
		}
	}
	e.mu.Lock()
	e.streamOthers = others
	e.mu.Unlock()
	return nil
}

// ApplyStream feeds a stream-computed breach into the state machine (escalate only), or hands it to the owner.
func (e *Engine) ApplyStream(ctx context.Context, m *model.Monitor, res eval.Result) {
	if !e.owns(m.ID) {
		e.handOff(ctx, m, "breach", res.Value, res.Detail)
		return
	}
	e.apply(ctx, m, res, e.Now(), true)
}

// EvalNow re-evaluates m against ClickHouse right away (stream trigger; escalate only), or hands it to the owner.
func (e *Engine) EvalNow(ctx context.Context, m *model.Monitor) {
	if !e.owns(m.ID) {
		e.handOff(ctx, m, "eval", 0, nil)
		return
	}
	e.evalMonitor(ctx, m, e.Now(), true)
}

func (e *Engine) handOff(ctx context.Context, m *model.Monitor, kind string, value float64, detail map[string]any) {
	if err := e.st.HandOffStreamHit(ctx, m.ID, kind, value, detail); err != nil {
		e.reg.Add(streamHitsMetric, 1, "dir", streamHitErrorDir)
		e.log.Error("stream hand-off failed", "monitor", m.ID, "kind", kind, "err", err)
		return
	}
	e.reg.Add(streamHitsMetric, 1, "dir", streamHitSentDir)
	e.log.Debug("stream hit handed off", "monitor", m.ID, "kind", kind)
}

// RunStreamHandoff claims stream hits for the held shards until ctx ends: LISTEN tt_stream_hit wakes it up right
// away, a 1 s poll covers lost notifications. Only needed with more than one shard.
func (e *Engine) RunStreamHandoff(ctx context.Context, pool *pgxpool.Pool) {
	var conn *pgxpool.Conn
	defer func() {
		if conn != nil {
			conn.Hijack().Close(context.Background()) // drop the LISTEN session
		}
	}()
	lastPurge := time.Time{}
	for ctx.Err() == nil {
		if conn == nil {
			c, err := pool.Acquire(ctx)
			if err == nil {
				if _, err = c.Exec(ctx, "LISTEN "+store.StreamHitChannel); err != nil {
					c.Release()
				}
			}
			if err != nil {
				if ctx.Err() == nil {
					e.log.Warn("stream hand-off: listen failed, polling", "err", err)
				}
			} else {
				conn = c
			}
		}
		if conn != nil {
			wctx, cancel := context.WithTimeout(ctx, streamHitPoll)
			_, err := conn.Conn().WaitForNotification(wctx)
			cancel()
			if err != nil && ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) {
				e.log.Warn("stream hand-off: listen connection lost", "err", err)
				conn.Hijack().Close(context.Background())
				conn = nil
			}
		} else {
			sleepCtx(ctx, streamHitPoll)
		}
		if ctx.Err() != nil {
			return
		}
		e.ClaimStreamHits(ctx)
		if time.Since(lastPurge) >= streamPurgeEvery && e.StreamLeader() {
			lastPurge = time.Now()
			if n, err := e.st.PurgeStreamHits(ctx, streamHitMaxAge); err == nil && n > 0 {
				e.reg.Add(streamHitsMetric, float64(n), "dir", streamHitDropDir)
			}
		}
	}
}

// ClaimStreamHits applies pending hand-offs for the held shards once (exported for tests).
func (e *Engine) ClaimStreamHits(ctx context.Context) int {
	e.mu.Lock()
	held := slices.Clone(e.held)
	e.mu.Unlock()
	if len(held) == 0 {
		return 0
	}
	hits, err := e.st.ClaimStreamHits(ctx, store.Shards{N: e.elector.Shards, Held: held}, streamHitMaxAge, streamHitBatch)
	if err != nil {
		if ctx.Err() == nil {
			e.log.Error("stream hand-off: claim failed", "err", err)
		}
		return 0
	}
	if len(hits) == 0 {
		return 0
	}
	byID := map[int64]*model.Monitor{}
	for _, m := range e.snapshot() {
		byID[m.ID] = m
	}
	// collapse per monitor: the largest breach wins; an eval runs once and only without a breach
	breach := map[int64]store.StreamHit{}
	evals := map[int64]bool{}
	for _, h := range hits {
		switch h.Kind {
		case "breach":
			if b, ok := breach[h.MonitorID]; !ok || h.Value > b.Value {
				breach[h.MonitorID] = h
			}
		default:
			evals[h.MonitorID] = true
		}
	}
	applied := 0
	for id, h := range breach {
		m := byID[id]
		if m == nil {
			e.reg.Add(streamHitsMetric, 1, "dir", streamHitDropDir)
			continue
		}
		delete(evals, id)
		e.apply(ctx, m, eval.Result{Value: h.Value, Breached: true, Detail: h.Detail}, e.Now(), true)
		applied++
	}
	for id := range evals {
		m := byID[id]
		if m == nil {
			e.reg.Add(streamHitsMetric, 1, "dir", streamHitDropDir)
			continue
		}
		e.evalMonitor(ctx, m, e.Now(), true)
		applied++
	}
	e.reg.Add(streamHitsMetric, float64(len(hits)), "dir", streamHitRecvDir)
	return applied
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
