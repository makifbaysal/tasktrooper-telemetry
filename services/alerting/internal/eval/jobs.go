package eval

import (
	"context"
	"math"
	"time"

	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
)

// JobWindow is how late a run may be: intervalMin + graceMin (grace defaults to 10 % of the interval, ≥ 1 min).
// Without intervalMin, condition.window is the whole allowance.
func JobWindow(m *model.Monitor) time.Duration {
	iv, grace := m.Query.IntervalMin, m.Query.GraceMin
	if iv <= 0 {
		w := m.Condition.Window
		if w <= 0 {
			w = time.Hour
		}
		return w
	}
	if grace <= 0 {
		grace = math.Max(1, iv/10)
	}
	return time.Duration((iv + grace) * float64(time.Minute))
}

// jobLookback bounds the "last run" search: 3 allowances, at least a day, at most 7 days.
func jobLookback(allow time.Duration) time.Duration {
	return min(max(3*allow, 24*time.Hour), 7*24*time.Hour)
}

// jobMissed: a cron job or consumer expected to run every intervalMin. With a process (query or target) it
// looks for consumer_1m rows with processed > 0 (honouring messagingSystem / topic / consumerGroup); otherwise
// for entry spans of the route in red_1m. Value = minutes since the last run (lookback when never seen);
// breached when that exceeds intervalMin + graceMin.
func (e *Evaluator) jobMissed(ctx context.Context, m *model.Monitor, now time.Time) (Result, error) {
	allow := JobWindow(m)
	look := jobLookback(allow)
	to := now.Truncate(time.Minute).Add(time.Minute) // include the running minute: a run just happened
	from := to.Add(-look)
	svc, route, method, env := m.Scope()
	process := m.Query.Process
	if process == "" {
		process = m.Target.Process
	}
	var last time.Time
	var n uint64
	source := "consumer_1m"
	if process != "" {
		w := tenantWhere(m).eq("service", svc).eq("env", env).eq("process", process).eq("topic", m.Query.Topic).
			eq("consumer_group", m.Query.ConsumerGroup).eq("messaging_system", m.Query.MessagingSystem).between("minute", from, to).
			add("processed > 0")
		if err := e.CH.QueryRow(ctx, "SELECT max(minute), count() FROM telemetry.consumer_1m WHERE "+w.sql(), w.args...).Scan(&last, &n); err != nil {
			return Result{}, err
		}
	} else {
		source = "red_1m"
		w := tenantWhere(m).eq("service", svc).eq("route", route).eq("method", method).eq("env", env).between("minute", from, to).add("count > 0")
		if err := e.CH.QueryRow(ctx, "SELECT max(minute), count() FROM telemetry.red_1m WHERE "+w.sql(), w.args...).Scan(&last, &n); err != nil {
			return Result{}, err
		}
	}
	since := look
	if n > 0 && !last.IsZero() {
		// a run in minute M ended no earlier than M; measure from the end of that minute (≤ 1 min optimistic)
		since = max(0, now.Sub(last.Add(time.Minute)))
	}
	v := math.Round(since.Minutes()*10) / 10
	th := allow.Minutes()
	return Result{
		Value: v, Breached: v > th,
		Detail: map[string]any{"op": ">", "threshold": th, "source": source, "lastRun": lastRun(n, last), "lookbackMin": look.Minutes()},
	}, nil
}

func lastRun(n uint64, t time.Time) any {
	if n == 0 {
		return nil
	}
	return t.UnixMilli()
}

// errorNew counts error fingerprints of the project (optionally one service) first seen inside the window
// that have no earlier occurrence in errors_1h during query.lookback (default 30d; errors_1h keeps 90d).
func (e *Evaluator) errorNew(ctx context.Context, m *model.Monitor, now time.Time) (Result, error) {
	from, to := window(now, m.Condition.Window)
	to = to.Add(time.Minute) // include the running minute (stream rules fire within seconds)
	look, _ := model.ParseDuration(m.Query.Lookback)
	if look <= 0 {
		look = 30 * 24 * time.Hour
	}
	svc, _, _, _ := m.Scope()
	w := tenantWhere(m).eq("service", svc).between("hour", from.Add(-look).Truncate(time.Hour), to)
	q := `SELECT count(), groupArray(5)(toString(fingerprint)), groupArray(5)(msg) FROM (
		SELECT fingerprint, min(first_seen) AS fs, any(message) AS msg FROM telemetry.errors_1h
		WHERE ` + w.sql() + ` GROUP BY fingerprint HAVING fs >= toDateTime64(?, 6) AND fs < toDateTime64(?, 6))`
	var n uint64
	var fps, msgs []string
	if err := e.CH.QueryRow(ctx, q, append(w.args, from.Unix(), to.Unix())...).Scan(&n, &fps, &msgs); err != nil {
		return Result{}, err
	}
	r := compare(m, float64(n))
	r.Detail["fingerprints"], r.Detail["samples"] = fps, msgs
	return r, nil
}
