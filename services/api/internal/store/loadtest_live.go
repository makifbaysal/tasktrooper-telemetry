package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/timerange"
)

// Runner metrics (contracts §14): OTLP from loadtest-runner, service.name = loadtest, attr trooper.loadtest.run.
// Sums are expected with delta temporality (each point = what happened since the previous export).
const (
	ltRequests = "trooper.loadtest.requests"
	ltErrors   = "trooper.loadtest.errors"
	ltDuration = "trooper.loadtest.duration_ms"
	ltVus      = "trooper.loadtest.vus"
	maxPoints  = 2000
)

// runWindow returns the run's time span [from, to): started_at (or created_at) → finished_at (or now).
func (d *DB) runWindow(ctx context.Context, id string, now time.Time) (from, to time.Time, live []byte, started bool, err error) {
	n, err := parseID(id)
	if err != nil {
		return from, to, nil, false, err
	}
	var start, fin *time.Time
	var created time.Time
	err = d.pg.QueryRow(ctx, `SELECT r.started_at, r.finished_at, r.created_at, r.live FROM loadtest_runs r
	  JOIN loadtest_scenarios s ON s.id = r.scenario_id WHERE {{tenant:s}} AND r.id = $1`, n).Scan(&start, &fin, &created, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return from, to, nil, false, ErrNotFound
	}
	if err != nil {
		return from, to, nil, false, err
	}
	from, to = created, now
	if start != nil {
		from, started = *start, true
	}
	if fin != nil {
		to = *fin
	}
	if to.Before(from) {
		to = from
	}
	return from.UTC(), to.UTC(), live, started, nil
}

// RunSeries is GET /loadtests/runs/{id}/series: the runner's own view (VUs, requests/s, p95, error rate) per step
// from the metrics table, or the latest live snapshot when no metric has arrived (yet).
func (d *DB) RunSeries(ctx context.Context, id string, step time.Duration, now time.Time) (model.RunSeries, error) {
	out := model.RunSeries{Ts: []int64{}, Vus: []float64{}, Rps: []float64{}, P95: []float64{}, ErrorRate: []float64{}, Source: "metrics"}
	if step < time.Second || step > time.Hour {
		return out, Invalid("step must be between 1s and 1h")
	}
	from, to, live, started, err := d.runWindow(ctx, id, now)
	if err != nil {
		return out, err
	}
	n := int(math.Ceil(float64(to.Sub(from)) / float64(step)))
	for n > maxPoints {
		step *= 2
		n = int(math.Ceil(float64(to.Sub(from)) / float64(step)))
	}
	n = max(n, 1)
	out.StepMs = step.Milliseconds()
	w := timerange.Window{Step: step, Points: n, From: from.Truncate(time.Second), To: from.Truncate(time.Second).Add(time.Duration(n) * step)}
	got := false
	if started {
		b := func(col string) string { // bucket expression at sub-minute resolution (metrics.ts is DateTime64(3))
			return "intDiv(toInt64(toUnixTimestamp64Milli(" + col + ")) - " + strconv.FormatInt(w.From.UnixMilli(), 10) + ", " +
				strconv.FormatInt(step.Milliseconds(), 10) + ")"
		}
		reqs, errs, vus := make([]float64, n), make([]float64, n), make([]float64, n)
		hist := make([]map[float64]uint64, n)
		rows, err := d.ch.Query(ctx, `SELECT b, name, toFloat64(sum(v)) FROM (
		    SELECT `+b("ts")+` AS b, name, attrs_hash, if(name = ?, max(value), sum(value)) AS v FROM metrics
		    WHERE {{tenant}} AND service = 'loadtest' AND name IN (?, ?, ?) AND attrs['trooper.loadtest.run'] = ?
		      AND ts >= fromUnixTimestamp64Milli(?) AND ts < fromUnixTimestamp64Milli(?)
		    GROUP BY b, name, attrs_hash) GROUP BY b, name`,
			ltVus, ltRequests, ltErrors, ltVus, id, w.From.UnixMilli(), w.To.UnixMilli())
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var bk int64
			var name string
			var v float64
			if err := rows.Scan(&bk, &name, &v); err != nil {
				rows.Close()
				return out, err
			}
			if bk < 0 || bk >= int64(n) {
				continue
			}
			got = true
			switch name {
			case ltRequests:
				reqs[bk] = v
			case ltErrors:
				errs[bk] = v
			case ltVus:
				vus[bk] = v
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return out, err
		}
		hrows, err := d.ch.Query(ctx, `SELECT `+b("ts")+` AS b, bucket_bounds, sumForEach(bucket_counts) FROM metrics
		  WHERE {{tenant}} AND service = 'loadtest' AND name = ? AND type = 'histogram' AND attrs['trooper.loadtest.run'] = ?
		    AND ts >= fromUnixTimestamp64Milli(?) AND ts < fromUnixTimestamp64Milli(?)
		  GROUP BY b, bucket_bounds`, ltDuration, id, w.From.UnixMilli(), w.To.UnixMilli())
		if err != nil {
			return out, err
		}
		for hrows.Next() {
			var bk int64
			var bounds []float64
			var counts []uint64
			if err := hrows.Scan(&bk, &bounds, &counts); err != nil {
				hrows.Close()
				return out, err
			}
			if bk < 0 || bk >= int64(n) {
				continue
			}
			got = true
			if hist[bk] == nil {
				hist[bk] = map[float64]uint64{}
			}
			for i, c := range counts {
				ub := math.Inf(1) // the last bucket is (last bound, +Inf)
				if i < len(bounds) {
					ub = bounds[i]
				}
				hist[bk][ub] += c
			}
		}
		hrows.Close()
		if err := hrows.Err(); err != nil {
			return out, err
		}
		if got {
			sec := step.Seconds()
			for i := 0; i < n; i++ {
				out.Ts = append(out.Ts, w.From.Add(time.Duration(i)*step).UnixMilli())
				out.Vus = append(out.Vus, vus[i])
				out.Rps = append(out.Rps, reqs[i]/sec)
				out.ErrorRate = append(out.ErrorRate, div(errs[i], reqs[i]))
				out.P95 = append(out.P95, HistogramQuantile(hist[i], 0.95))
			}
			return out, nil
		}
	}
	out.Source = "live"
	var l model.RunLive
	if len(live) > 0 && json.Unmarshal(live, &l) == nil && l.Ts > 0 {
		out.Ts, out.Vus, out.Rps, out.P95, out.ErrorRate = []int64{l.Ts}, []float64{l.Vus}, []float64{l.Rps}, []float64{l.P95}, []float64{l.ErrorRate}
	}
	return out, nil
}

// HistogramQuantile estimates quantile q from explicit-bucket counts keyed by upper bound (linear interpolation
// inside the bucket; the +Inf bucket reports its lower bound).
func HistogramQuantile(h map[float64]uint64, q float64) float64 {
	if len(h) == 0 {
		return 0
	}
	bounds := make([]float64, 0, len(h))
	var total uint64
	for b, c := range h {
		bounds = append(bounds, b)
		total += c
	}
	if total == 0 {
		return 0
	}
	sort.Float64s(bounds)
	rank := q * float64(total)
	var cum float64
	lower := 0.0
	for _, ub := range bounds {
		c := float64(h[ub])
		if cum+c >= rank && c > 0 {
			if math.IsInf(ub, 1) {
				return lower
			}
			return lower + (ub-lower)*(rank-cum)/c
		}
		cum += c
		if !math.IsInf(ub, 1) {
			lower = ub
		}
	}
	return lower
}

// RunTarget is GET /loadtests/runs/{id}/target: the target service's RED per minute over the run window, total
// (red_1m) vs the run's own traffic (callers_1m, caller = loadtest:<id>). service = "" picks the service that
// received the most loadtest:<id> requests.
func (d *DB) RunTarget(ctx context.Context, id, service string, now time.Time) (model.RunTarget, error) {
	out := model.RunTarget{Service: service, Caller: "loadtest:" + id}
	from, to, _, _, err := d.runWindow(ctx, id, now)
	if err != nil {
		return out, err
	}
	first := from.Truncate(time.Minute)
	n := int(to.Sub(first)/time.Minute) + 1
	if n > 24*60 {
		n = 24 * 60
	}
	w := timerange.Window{Step: time.Minute, Points: n, From: first, To: first.Add(time.Duration(n) * time.Minute)}
	out.From, out.To = w.From.UnixMilli(), w.To.UnixMilli()
	if out.Service == "" {
		err := d.ch.QueryRow(ctx, `SELECT service FROM callers_1m WHERE {{tenant}} AND caller = ? AND minute >= toDateTime(?) AND minute < toDateTime(?)
		  GROUP BY service ORDER BY sum(count) DESC LIMIT 1`, out.Caller, secs(w.From), secs(w.To)).Scan(&out.Service)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
	}
	b := timerange.NewBuilder(w, "rpm", "rpmLoadtest", "errorRate", "errorRateLoadtest", "p95", "p95Loadtest")
	if out.Service == "" {
		out.SeriesSet = labels(b, targetLabels)
		return out, nil
	}
	rows, err := d.red(ctx, w, "", nil, " AND service = ?", []any{out.Service}, true)
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		b.Set("rpm", r.bucket, r.count)
		b.Set("errorRate", r.bucket, div(r.errors, r.count))
		b.Set("p95", r.bucket, qt(r.q, 2))
	}
	tot, err := d.red(ctx, w, "", nil, " AND service = ?", []any{out.Service}, false)
	if err != nil {
		return out, err
	}
	if len(tot) > 0 {
		out.Total = model.RedTotals{Requests: tot[0].count, ErrorRate: div(tot[0].errors, tot[0].count), P95: qt(tot[0].q, 2)}
	}
	callerSQL := func(sel, group string) string {
		return `SELECT ` + sel + `toFloat64(sum(count)), toFloat64(sum(errors)), arrayMap(x -> toFloat64(x), quantilesTDigestWeightedMerge(0.5, 0.95)(duration))
		  FROM callers_1m WHERE {{tenant}} AND service = ? AND caller = ? AND minute >= toDateTime(?) AND minute < toDateTime(?)` + group
	}
	crow, err := d.ch.Query(ctx, callerSQL(w.BucketExpr("minute")+" AS b, ", " GROUP BY b"), out.Service, out.Caller, secs(w.From), secs(w.To))
	if err != nil {
		return out, err
	}
	defer crow.Close()
	for crow.Next() {
		var bk int64
		var c, e float64
		var q []float64
		if err := crow.Scan(&bk, &c, &e, &q); err != nil {
			return out, err
		}
		b.Set("rpmLoadtest", int(bk), c)
		b.Set("errorRateLoadtest", int(bk), div(e, c))
		b.Set("p95Loadtest", int(bk), qt(q, 1))
	}
	if err := crow.Err(); err != nil {
		return out, err
	}
	var tc, te float64
	var tq []float64
	if err := d.ch.QueryRow(ctx, callerSQL("", ""), out.Service, out.Caller, secs(w.From), secs(w.To)).Scan(&tc, &te, &tq); err != nil {
		return out, err
	}
	if tc > 0 {
		out.Loadtest = model.RedTotals{Requests: tc, ErrorRate: div(te, tc), P95: qt(tq, 1)}
	}
	out.Other = model.RedTotals{Requests: math.Max(0, out.Total.Requests-out.Loadtest.Requests)}
	out.SeriesSet = labels(b, targetLabels)
	return out, nil
}

var targetLabels = map[string]string{"rpm": "Requests / min (all)", "rpmLoadtest": "Requests / min (load test)", "errorRate": "Error rate (all)",
	"errorRateLoadtest": "Error rate (load test)", "p95": "p95 (all)", "p95Loadtest": "p95 (load test)"}

// ---------------------------------------------------------------- domains

const domainSelect = `SELECT id, host, token, coalesce(method, ''), verified_at, verify_requested_at, created_at FROM loadtest_domains`

func scanDomain(row pgx.Row) (model.LoadTestDomain, error) {
	var dm model.LoadTestDomain
	var id int64
	var verified, requested *time.Time
	var created time.Time
	if err := row.Scan(&id, &dm.Host, &dm.Token, &dm.Method, &verified, &requested, &created); err != nil {
		return dm, err
	}
	dm.ID, dm.CreatedAt = idStr(id), ms(created)
	if verified != nil {
		dm.VerifiedAt, dm.Verified = ms(*verified), true
	}
	if requested != nil {
		dm.VerifyRequestedAt = ms(*requested)
	}
	dm.DNSRecord = "trooper-verify=" + dm.Token
	dm.HTTPURL = "https://" + dm.Host + "/.well-known/trooper-verify"
	return dm, nil
}

// ListDomains returns the project's load test target domains.
func (d *DB) ListDomains(ctx context.Context) ([]model.LoadTestDomain, error) {
	rows, err := d.pg.Query(ctx, domainSelect+` WHERE {{tenant}} ORDER BY host`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.LoadTestDomain{}
	for rows.Next() {
		dm, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dm)
	}
	return out, rows.Err()
}

func (d *DB) getDomain(ctx context.Context, n int64) (*model.LoadTestDomain, error) {
	dm, err := scanDomain(d.pg.QueryRow(ctx, domainSelect+` WHERE {{tenant}} AND id = $1`, n))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &dm, err
}

// NormalizeHost reduces "https://Shop.example.com:443/x" to "shop.example.com" (ports are not part of ownership).
func NormalizeHost(raw string) (string, error) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", Invalid("host is not a valid URL or hostname")
		}
		raw = u.Host
	}
	if h, _, err := net.SplitHostPort(raw); err == nil {
		raw = h
	}
	raw = strings.TrimSuffix(raw, ".")
	if raw == "" || len(raw) > 253 || strings.ContainsAny(raw, "/ @*") {
		return "", Invalid("host must be a hostname like shop.example.com")
	}
	return raw, nil
}

// CreateDomain registers a host and generates its verification token.
func (d *DB) CreateDomain(ctx context.Context, in model.LoadTestDomainInput) (*model.LoadTestDomain, error) {
	host, err := NormalizeHost(in.Host)
	if err != nil {
		return nil, err
	}
	if in.Method == "" {
		in.Method = "dns"
	}
	if in.Method != "dns" && in.Method != "http" {
		return nil, Invalid("method must be dns|http")
	}
	var id int64
	err = d.pg.QueryRow(ctx, `INSERT INTO loadtest_domains (project_id, host, token, method) VALUES ({{project}}, $1, $2, $3) RETURNING id`,
		host, "tt-"+strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(randomToken(18))), in.Method).Scan(&id)
	if isUnique(err) {
		return nil, Conflict("domain already registered")
	}
	if err != nil {
		return nil, err
	}
	return d.getDomain(ctx, id)
}

// DeleteDomain removes a domain.
func (d *DB) DeleteDomain(ctx context.Context, id string) error {
	n, err := parseID(id)
	if err != nil {
		return err
	}
	tag, err := d.pg.Exec(ctx, `DELETE FROM loadtest_domains WHERE {{tenant}} AND id = $1`, n)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RequestDomainVerify asks loadtest-ctl to (re)check the domain (verify_requested_at); method switches dns/http.
func (d *DB) RequestDomainVerify(ctx context.Context, id, method string) (*model.LoadTestDomain, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	if method != "" && method != "dns" && method != "http" {
		return nil, Invalid("method must be dns|http")
	}
	tag, err := d.pg.Exec(ctx, `UPDATE loadtest_domains SET verify_requested_at = now(), method = coalesce(nullif($2, ''), method)
	  WHERE {{tenant}} AND id = $1`, n, method)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return d.getDomain(ctx, n)
}
