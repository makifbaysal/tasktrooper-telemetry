package store

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
)

// GetError returns one backend error issue with the stack of its latest occurrence (raw; the server symbolicates).
func (d *DB) GetError(ctx context.Context, q Q, fingerprint string) (*model.ErrorDetail, error) {
	fp, err := strconv.ParseUint(fingerprint, 10, 64)
	if err != nil {
		return nil, ErrNotFound
	}
	var e model.ErrorDetail
	var first, last time.Time
	var n uint64
	err = d.ch.QueryRow(ctx, `SELECT count(), any(service), any(type), any(message), any(route), min(first_seen), max(last_seen), toFloat64(sum(count)),
	    lower(hex(any(sample_trace)))
	  FROM errors_1h WHERE {{tenant}} AND fingerprint = ? AND hour >= toStartOfHour(toDateTime(?)) AND hour < toDateTime(?)`,
		fp, secs(q.W.From), secs(q.W.To)).Scan(&n, &e.Service, &e.Type, &e.Message, &e.Route, &first, &last, &e.Count, &e.SampleTrace)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrNotFound
	}
	e.Fingerprint, e.FirstSeen, e.LastSeen, e.Status = fingerprint, ms(first), ms(last), "open"
	if strings.Trim(e.SampleTrace, "0") == "" {
		e.SampleTrace = ""
	}
	if st, err := d.errorStatuses(ctx); err == nil {
		if s, ok := st[fingerprint]; ok {
			e.Status = s
		}
	} else {
		return nil, err
	}
	// latest occurrence: stack from the span attribute or its exception event
	rows, err := d.ch.Query(ctx, `SELECT version, resource_attrs['telemetry.sdk.language'],
	    if(attrs['exception.stacktrace'] != '', attrs['exception.stacktrace'],
	       arrayFirst(m -> m['exception.stacktrace'] != '', events.attrs)['exception.stacktrace']), lower(hex(trace_id))
	  FROM spans WHERE {{tenant}} AND service = ? AND error_fingerprint = ? AND ts >= fromUnixTimestamp64Micro(?) AND ts <= fromUnixTimestamp64Micro(?)
	  ORDER BY ts DESC LIMIT 1`, e.Service, fp, micros(first.Add(-time.Second)), micros(last.Add(time.Second)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if rows.Next() {
		var trace string
		if err := rows.Scan(&e.Release, &e.Platform, &e.Stack, &trace); err != nil {
			return nil, err
		}
		if strings.Trim(trace, "0") != "" {
			e.SampleTrace = trace
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	e.Frames = []model.Frame{}
	e.Replay = d.backendErrorReplay(ctx, q, e.Service, fp)
	return &e, nil
}

// GetRumError returns one RUM error / crash of an app with the stack of its latest occurrence.
func (d *DB) GetRumError(ctx context.Context, q Q, app, fingerprint string) (*model.RumErrorDetail, error) {
	fp, err := strconv.ParseUint(fingerprint, 10, 64)
	if err != nil {
		return nil, ErrNotFound
	}
	ec, ea := envCond(q.Env, "env")
	var e model.RumErrorDetail
	var crash uint8
	var first, last time.Time
	var n uint64
	err = d.ch.QueryRow(ctx, `SELECT count(), any(error_type), any(error_message), topK(1)(view_name)[1], toFloat64(count()),
	    toFloat64(uniq(session_id)), max(is_crash), min(ts), max(ts), argMax(app_version, ts), argMax(platform, ts), argMax(error_stack, ts)
	  FROM rum_events WHERE {{tenant}} AND app = ? AND type = 'error' AND error_fingerprint = ?
	    AND ts >= fromUnixTimestamp64Micro(?) AND ts < fromUnixTimestamp64Micro(?)`+ec,
		append([]any{app, fp, micros(q.W.From), micros(q.W.To)}, ea...)...).
		Scan(&n, &e.Type, &e.Message, &e.View, &e.Count, &e.Sessions, &crash, &first, &last, &e.AppVersion, &e.Platform, &e.Stack)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrNotFound
	}
	e.Fingerprint, e.App, e.Crash, e.FirstSeen, e.LastSeen = fingerprint, app, crash == 1, ms(first), ms(last)
	e.Frames = []model.Frame{}
	e.Replay = d.rumErrorReplay(ctx, q, app, fp)
	return &e, nil
}
