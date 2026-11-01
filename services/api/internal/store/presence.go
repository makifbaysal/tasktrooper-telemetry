package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// presenceLookback bounds the raw-table part of presence (spans, metrics): at most the last 24 h of the window.
const presenceLookback = 24 * time.Hour

// HeartbeatMetric is the SDK presence gauge (contracts §4). Other trooper.* metrics (collector health, loadtest
// runner, cloud integrations) carry the collector's / platform's service name, not the customer service's, so they
// do not make a service present.
const HeartbeatMetric = "trooper.sdk.heartbeat"

// JobSystem is consumer_1m.messaging_system of background job entries (contracts §4; written by ingest-writer).
const JobSystem = "job"

// presence is what the services list knows about a service besides its traffic aggregates.
type presence struct {
	lastSeen int64 // unix ms of the latest telemetry seen in the window (0 = unknown)
	jobsOnly bool  // every consumer_1m row of the service is a background job entry (messaging_system = 'job')
	traffic  bool  // has entry traffic (red / consumer / rum rollups) in the window
}

// rollupPresence reads, per service, the last minute with entry traffic in the window from the rollups (cheap) and
// whether its consumer traffic is only background job entries (contracts §4).
func (d *DB) rollupPresence(ctx context.Context, q Q, where string, args []any) (map[string]*presence, error) {
	sql, all := rollupPresenceQuery(q, where, args)
	rows, err := d.ch.Query(ctx, sql, all...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*presence{}
	now := q.Now.UnixMilli()
	for rows.Next() {
		var s string
		var m int64
		var j uint8
		if err := rows.Scan(&s, &m, &j); err != nil {
			return nil, err
		}
		addRollupPresence(out, s, m, j, now)
	}
	return out, rows.Err()
}

// Rollup row kinds (column j of rollupPresenceQuery).
const (
	presTraffic  = 0 // HTTP / RUM traffic
	presMessages = 1 // consumer traffic with messages
	presJobs     = 2 // consumer traffic made only of job entries
)

// addRollupPresence merges one rollup row (service s, last bucket start m in unix seconds, kind j) into out.
func addRollupPresence(out map[string]*presence, s string, m int64, j uint8, nowMs int64) {
	p := out[s]
	if p == nil {
		p = &presence{jobsOnly: true}
		out[s] = p
	}
	p.traffic = true
	// a minute bucket covers up to its end: the last request may be ≤ 60 s after its start
	p.lastSeen = max(p.lastSeen, min(m*1000+59_999, nowMs))
	p.jobsOnly = p.jobsOnly && j == presJobs
}

// rollupPresenceQuery: last bucket per service over the window from red (1m/1h), consumer_1m and rum_views_1m.
func rollupPresenceQuery(q Q, where string, args []any) (string, []any) {
	ec, ea := envCond(q.Env, "env")
	appWhere := ""
	if where != "" {
		appWhere = strings.Replace(where, "service", "app", 1)
	}
	part := func(table, col, j, w string) string {
		return fmt.Sprintf(`SELECT %s AS s, toInt64(toUnixTimestamp(max(minute))) AS m, %s AS j FROM %s
	    WHERE {{tenant}} AND minute >= toDateTime(?) AND minute < toDateTime(?)%s%s GROUP BY s`, col, j, table, ec, w)
	}
	sql := part(redTable(q.W), "service", fmt.Sprintf("toUInt8(%d)", presTraffic), where) + "\n  UNION ALL " +
		part("consumer_1m", "service", fmt.Sprintf("toUInt8(%d + min(messaging_system = '%s'))", presMessages, JobSystem), where) + "\n  UNION ALL " +
		part("rum_views_1m", "app", fmt.Sprintf("toUInt8(%d)", presTraffic), appWhere)
	var all []any
	for range 3 {
		all = append(all, secs(q.W.From), secs(q.W.To))
		all = append(all, ea...)
		all = append(all, args...)
	}
	return sql, all
}

// telemetryPresence returns the last time (unix ms) each service sent any span or SDK metric in the window (at most
// its last presenceLookback), skipping the services in known (already present through rollups): a service with an
// SDK but no entry traffic — an idle API, a background worker — is still listed. The service column follows the
// tenant in both tables' ORDER BY, so excluding the known (busy) services prunes most granules, and the time filter
// prunes the rest per (service, route) / (name, service, series) run.
func (d *DB) telemetryPresence(ctx context.Context, q Q, where string, args []any, known []string) (map[string]int64, error) {
	sql, all := telemetryPresenceQuery(q, where, args, known)
	rows, err := d.ch.Query(ctx, sql, all...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var s string
		var ms int64
		if err := rows.Scan(&s, &ms); err != nil {
			return nil, err
		}
		out[s] = ms
	}
	return out, rows.Err()
}

// telemetryPresenceQuery: latest span / SDK metric per service over the window's last presenceLookback, except known.
func telemetryPresenceQuery(q Q, where string, args []any, known []string) (string, []any) {
	w := q.W
	from := w.From
	if f := w.To.Add(-presenceLookback); f.After(from) {
		from = f
	}
	ec, ea := envCond(q.Env, "env")
	cond := ec + where + " AND service != ''"
	cargs := append(append([]any{}, ea...), args...)
	if len(known) > 0 {
		known = slices.Sorted(slices.Values(known))
		cond += " AND service NOT IN (?)"
		cargs = append(cargs, known)
	}
	sql := `SELECT service, toInt64(max(ms)) FROM (
	    SELECT service, toUnixTimestamp64Milli(max(ts)) AS ms FROM spans
	    WHERE {{tenant}} AND ts >= toDateTime64(?, 6) AND ts < toDateTime64(?, 6)` + cond + ` GROUP BY service
	  UNION ALL
	    SELECT service, toUnixTimestamp64Milli(max(ts)) AS ms FROM metrics
	    WHERE {{tenant}} AND ts >= toDateTime64(?, 3) AND ts < toDateTime64(?, 3)
	      AND (name = ? OR NOT startsWith(name, 'trooper.'))` + cond + ` GROUP BY service)
	  GROUP BY service`
	all := append([]any{secs(from), secs(w.To)}, cargs...)
	all = append(append(all, secs(from), secs(w.To), HeartbeatMetric), cargs...)
	return sql, all
}
