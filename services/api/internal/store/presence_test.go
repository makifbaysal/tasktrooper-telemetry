package store

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/timerange"
)

func TestPresenceQueries(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		rng       timerange.Range
		env, only string
		known     []string
		wantRed   string
		wantFrom  time.Time // raw-table lookback start
		wantNotIn bool
	}{
		{name: "list 1h", rng: timerange.R1h, known: []string{"b", "a"}, wantRed: "red_1m", wantFrom: timerange.R1h.Window(now).From, wantNotIn: true},
		{name: "detail with env", rng: timerange.R1h, env: "prod", only: "svc", wantRed: "red_1m", wantFrom: timerange.R1h.Window(now).From},
		{name: "7d is capped to 24h raw", rng: timerange.R7d, wantRed: "red_1h", wantFrom: timerange.R7d.Window(now).To.Add(-24 * time.Hour)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := Q{W: tt.rng.Window(now), Env: tt.env, Now: now}
			var where string
			var args []any
			if tt.only != "" {
				where, args = " AND service = ?", []any{tt.only}
			}
			sql, all := rollupPresenceQuery(q, where, args)
			if n := strings.Count(sql, "?"); n != len(all) {
				t.Errorf("rollup: %d placeholders, %d args\n%s", n, len(all), sql)
			}
			if strings.Count(sql, "{{tenant}}") != 3 || !strings.Contains(sql, "FROM "+tt.wantRed) || !strings.Contains(sql, "messaging_system = 'job'") {
				t.Errorf("rollup sql:\n%s", sql)
			}
			if tt.only != "" && !strings.Contains(sql, "AND app = ?") {
				t.Errorf("rollup: rum part not filtered by app\n%s", sql)
			}

			sql, all = telemetryPresenceQuery(q, where, args, tt.known)
			if n := strings.Count(sql, "?"); n != len(all) {
				t.Errorf("raw: %d placeholders, %d args\n%s", n, len(all), sql)
			}
			if strings.Count(sql, "{{tenant}}") != 2 || strings.Contains(sql, "NOT IN") != tt.wantNotIn {
				t.Errorf("raw sql:\n%s", sql)
			}
			if all[0] != secs(tt.wantFrom) || all[1] != secs(q.W.To) {
				t.Errorf("raw window = %v..%v, want %v..%v", all[0], all[1], secs(tt.wantFrom), secs(q.W.To))
			}
			if !slices.Contains(all, any(HeartbeatMetric)) {
				t.Errorf("heartbeat metric not bound: %v", all)
			}
			if tt.wantNotIn {
				if k, ok := all[len(all)-1].([]string); !ok || !slices.IsSorted(k) || len(k) != len(tt.known) {
					t.Errorf("known services arg = %v", all[len(all)-1])
				}
			}
		})
	}
}

func TestAddRollupPresence(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 30, 0, time.UTC)
	minute := func(m int) int64 { return now.Truncate(time.Minute).Add(time.Duration(m) * time.Minute).Unix() }
	type row struct {
		m int64
		j uint8
	}
	tests := []struct {
		name     string
		rows     []row
		jobsOnly bool
		lastSeen int64
	}{
		{name: "jobs only", rows: []row{{minute(-5), presJobs}}, jobsOnly: true, lastSeen: (minute(-5)+59)*1000 + 999},
		{name: "jobs and messages", rows: []row{{minute(-5), presJobs}, {minute(-2), presMessages}}, lastSeen: (minute(-2)+59)*1000 + 999},
		{name: "http", rows: []row{{minute(-1), presTraffic}}, lastSeen: (minute(-1)+59)*1000 + 999},
		{name: "current minute capped at now", rows: []row{{minute(0), presTraffic}}, lastSeen: now.UnixMilli()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := map[string]*presence{}
			for _, r := range tt.rows {
				addRollupPresence(out, "svc", r.m, r.j, now.UnixMilli())
			}
			p := out["svc"]
			if p == nil || !p.traffic || p.jobsOnly != tt.jobsOnly || p.lastSeen != tt.lastSeen {
				t.Errorf("got %+v, want jobsOnly=%v lastSeen=%d", p, tt.jobsOnly, tt.lastSeen)
			}
		})
	}
}
