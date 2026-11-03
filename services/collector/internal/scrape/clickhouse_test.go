package scrape

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/collector/internal/config"
	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
)

// fakeCH answers the scraper's queries from per-scrape tables.
type fakeCH struct {
	scrape int                             // 0-based index of the current scrape
	pair   []map[string]map[string]float64 // per scrape: query → rows
	errs   map[string]error
	stmts  []chStmt
	args   map[string][]any
}

func (f *fakeCH) pairs(_ context.Context, q string, args ...any) (map[string]float64, error) {
	if f.args == nil {
		f.args = map[string][]any{}
	}
	f.args[q] = args
	if err := f.errs[q]; err != nil {
		return nil, err
	}
	if !strings.HasPrefix(q, chMarker) {
		return nil, errors.New("query without agent marker")
	}
	return f.pair[f.scrape][q], nil
}

func (f *fakeCH) statements(_ context.Context, q string, args ...any) ([]chStmt, error) {
	f.args[q] = args
	if err := f.errs[q]; err != nil {
		return nil, err
	}
	return f.stmts, nil
}

func (f *fakeCH) Close() error { return nil }

func chScrape(query, failed, hits, misses, now float64) map[string]map[string]float64 {
	return map[string]map[string]float64{
		chqMetrics:  {"TCPConnection": 3, "HTTPConnection": 2, "InterserverConnection": 1},
		chqEvents:   {"Query": query, "FailedQuery": failed, "MarkCacheHits": hits, "MarkCacheMisses": misses},
		chqAsync:    {"OSUserTimeNormalized": 0.2, "OSSystemTimeNormalized": 0.05, "MemoryResident": 2e9, "OSMemoryTotal": 16e9, "CGroupMemoryTotal": 8e9},
		chqMaxConn:  {"max_connections": 4096},
		chqDisks:    {"total": 100, "free": 75},
		chqReplicas: {"replicas": 0, "delay": 0},
		chqNow:      {"now": now},
		chqWindow:   {"p95": 120, "calls": 150, "slow": 2},
	}
}

func TestClickHouseScrape(t *testing.T) {
	stmts := []chStmt{
		{hash: "111", text: chMarker + "SELECT  count() FROM spans WHERE org_id = ?", calls: 100, totalMs: 900, rowsN: 100, p95Ms: 20},
		{hash: "222", text: "INSERT INTO spans FORMAT Native", calls: 40, totalMs: 4000, rowsN: 80000, p95Ms: 150},
		{hash: "333", text: "SELECT 1", calls: 10, totalMs: 1, rowsN: 10, p95Ms: 0},
	}
	tests := []struct {
		name    string
		mutate  func(f *fakeCH)
		want    map[string]float64 // gauges on the second scrape
		absent  []string
		queries int // top query rows (calls counters)
	}{
		{
			name: "all sources",
			want: map[string]float64{
				emit.DBConnections: 6, emit.DBMaxConnections: 4096, emit.DBOps: 10, emit.DBErrors: 12,
				emit.DBHitRatio: 0.9, emit.DBCPU: 0.25, emit.DBMemory: 0.25, emit.DBDisk: 0.25,
				emit.DBSlow: 8, emit.DBLatencyP95: 120,
			},
			absent:  []string{emit.DBReplLag},
			queries: 2,
		},
		{
			name: "replicated tables report lag",
			mutate: func(f *fakeCH) {
				f.pair[1][chqReplicas] = map[string]float64{"replicas": 4, "delay": 3}
			},
			want:    map[string]float64{emit.DBReplLag: 3000},
			queries: 2,
		},
		{
			name: "query_log disabled keeps the server metrics",
			mutate: func(f *fakeCH) {
				f.errs = map[string]error{chqWindow: errors.New("Table system.query_log does not exist")}
			},
			want:    map[string]float64{emit.DBConnections: 6, emit.DBOps: 10},
			absent:  []string{emit.DBSlow, emit.DBLatencyP95},
			queries: 0,
		},
		{
			name: "old server without server_settings",
			mutate: func(f *fakeCH) {
				f.errs = map[string]error{chqMaxConn: errors.New("Unknown table system.server_settings")}
			},
			want:    map[string]float64{emit.DBConnections: 6},
			absent:  []string{emit.DBMaxConnections},
			queries: 2,
		},
		{
			name: "idle window has no p95",
			mutate: func(f *fakeCH) {
				f.pair[1][chqWindow] = map[string]float64{"p95": math.NaN(), "calls": 0, "slow": 0}
				f.stmts = nil
			},
			want:    map[string]float64{emit.DBSlow: 0},
			absent:  []string{emit.DBLatencyP95},
			queries: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeCH{
				pair:  []map[string]map[string]float64{chScrape(1000, 0, 90, 10, 1000), chScrape(1150, 3, 180, 20, 1015)},
				stmts: stmts,
			}
			if tt.mutate != nil {
				tt.mutate(f)
			}
			db := config.Database{Slug: "tt-clickhouse", Name: "telemetry", Engine: "clickhouse", Host: "clickhouse",
				Interval: config.Duration(15 * time.Second), TopQueries: 2, SlowQueryMs: 500}
			c := newClickHouse(db, f, slog.New(slog.DiscardHandler))
			t0 := time.Unix(1_000_000, 0)
			c.now = func() time.Time { return t0 }

			r1, err := c.Scrape(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := find(r1, emit.DBOps, nil); ok {
				t.Error("first scrape must not report rates")
			}
			if len(r1.Counts) != 0 {
				t.Error("first scrape must only set the query_log cursor")
			}

			f.scrape = 1
			c.now = func() time.Time { return t0.Add(15 * time.Second) }
			r2, err := c.Scrape(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for name, want := range tt.want {
				got, ok := find(r2, name, map[string]string{emit.AttrDBSystem: "clickhouse", emit.AttrDBID: "tt-clickhouse", emit.AttrDBRole: "primary"})
				if !ok || math.Abs(got-want) > 1e-9 {
					t.Errorf("%s = %v (present %v), want %v", name, got, ok, want)
				}
			}
			for _, name := range tt.absent {
				if _, ok := find(r2, name, nil); ok {
					t.Errorf("%s must not be reported", name)
				}
			}
			calls := 0
			for _, s := range r2.Counts {
				if s.Name == emit.DBQueryCalls {
					calls++
				}
			}
			if calls != tt.queries {
				t.Errorf("top query rows = %d, want %d", calls, tt.queries)
			}
		})
	}
}

func TestClickHouseQueryWindow(t *testing.T) {
	f := &fakeCH{
		pair: []map[string]map[string]float64{chScrape(1, 0, 0, 0, 1000), chScrape(2, 0, 0, 0, 1015), chScrape(3, 0, 0, 0, 5000)},
		stmts: []chStmt{
			{hash: "18446744073709551615", text: "/* trooper-agent */ SELECT  a\n FROM   t", calls: 2, totalMs: 10, rowsN: 1, p95Ms: 6},
		},
	}
	db := config.Database{Slug: "ch", Engine: "clickhouse", Interval: config.Duration(15 * time.Second), TopQueries: 50, SlowQueryMs: 250}
	c := newClickHouse(db, f, slog.New(slog.DiscardHandler))
	if _, err := c.Scrape(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.cursor != 990 {
		t.Fatalf("cursor after first scrape = %d, want now − %d = 990", c.cursor, chFlushLag)
	}
	f.scrape = 1
	r, err := c.Scrape(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []any{int64(990), int64(990), int64(1005), int64(990), int64(990), int64(1005), 250.0, int64(990), int64(990), int64(1005)}
	if got := f.args[chqWindow]; len(got) != len(wantArgs) {
		t.Fatalf("window args = %v, want %v", got, wantArgs)
	} else {
		for i := range got {
			if got[i] != wantArgs[i] {
				t.Fatalf("window args = %v, want %v", got, wantArgs)
			}
		}
	}
	if strings.Count(chqWindow, "?") != len(wantArgs) || strings.Count(chqTop, "?") != 3 {
		t.Fatal("placeholder count drifted from the args")
	}
	var text, hash string
	for _, s := range r.Counts {
		if s.Name == emit.DBQueryCalls {
			v, _ := s.Attrs.Value(emit.AttrQueryText)
			h, _ := s.Attrs.Value(emit.AttrQueryHash)
			text, hash = v.AsString(), h.AsString()
		}
	}
	if text != "SELECT a FROM t" || hash != "18446744073709551615" {
		t.Errorf("query attrs = %q / %q", text, hash)
	}

	// after a long gap (agent paused) the window is capped to one interval
	f.scrape = 2
	if _, err := c.Scrape(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.args[chqWindow]; got[0] != int64(4990-15) || got[2] != int64(4990) || c.cursor != 4990 {
		t.Errorf("gap window = %v, cursor %d", got, c.cursor)
	}
}

func TestNewDatabaseClickHouse(t *testing.T) {
	s, err := NewDatabase(config.Database{Engine: "clickhouse", Host: "h", Port: 9000, Timeout: config.Duration(time.Second)}, HostOptions{}, slog.Default())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if _, ok := s.(*ClickHouse); !ok {
		t.Fatalf("got %T", s)
	}
	_ = s.Close()
}
