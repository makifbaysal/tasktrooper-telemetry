package scrape

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/connstring"
	"go.opentelemetry.io/otel/attribute"

	"github.com/tasktrooper/telemetry/services/collector/internal/config"
	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
	"github.com/tasktrooper/telemetry/services/collector/internal/rate"
)

// MongoDB scrapes one mongod with a user holding the built-in `clusterMonitor` role (serverStatus,
// replSetGetStatus, hostInfo, listDatabases, $queryStats on 7.0.12+/8.0). The driver connects
// directly to the given host (no topology discovery) so metrics are per node; a multi-host or
// mongodb+srv URI is used as is (primary).
//
//   - connections: connections.current; max_connections = current + available
//   - ops: Δ Σ opcounters (insert, query, update, delete, getmore, command) / s
//   - hit_ratio: 1 − Δ"pages read into cache" / Δ"pages requested from the cache" (WiredTiger)
//   - evictions: Δ(unmodified + modified pages evicted) / min
//   - memory: mem.resident / hostInfo memSizeMB
//   - latency.p95: Δ opLatencies histograms (reads + writes; commands are left out because
//     awaitable hello / getMore long-polls would dominate) → bucket p95, linear inside the
//     power-of-two-ish bucket; without histograms the ops-weighted interval mean
//   - role / replication_lag: replSetGetStatus. Secondary: primary optime − own optime; primary:
//     per secondary (trooper.db.node = member host); standalone: role primary, no lag
//   - top queries: $queryStats (TopQueriesSource auto|querystats), Δ execCount / totalExecMicros /
//     docsReturned per key, merged per queryShapeHash; p95 = mean + 2σ from sumOfSquares
//     (cumulative, like Postgres). Fallback (auto|profiler): system.profile of every database with
//     profiling on, entries since the last scrape grouped by normalized shape (only the operations
//     the profiler kept, i.e. ≥ slowms at level 1); slow = profiled ops ≥ slowQueryMs / min.
type MongoDB struct {
	t   config.Database
	cl  *mongo.Client
	log *slog.Logger
	c   *rate.Counters

	memSizeMB   float64
	hostInfoAt  time.Time
	lastQS      time.Time
	noQS        bool
	warnedQS    bool
	profSince   time.Time
	warnedProf  bool
	lastProfDBs time.Time
	profDBs     []string
}

// NewMongoDB builds the client (connections are lazy).
func NewMongoDB(t config.Database, log *slog.Logger) (*MongoDB, error) {
	uri, err := t.ConnString()
	if err != nil {
		return nil, err
	}
	cs, err := connstring.ParseAndValidate(uri)
	if err != nil {
		return nil, fmt.Errorf("mongodb uri: %w", err)
	}
	opts := options.Client().ApplyURI(uri).SetAppName("trooper-agent").
		SetServerSelectionTimeout(t.Timeout.D()).SetConnectTimeout(t.Timeout.D()).
		SetMaxPoolSize(2).SetReadPreference(readpref.PrimaryPreferred())
	if len(cs.Hosts) == 1 && cs.ReplicaSet == "" && cs.Scheme == connstring.SchemeMongoDB && !cs.DirectConnectionSet {
		opts.SetDirect(true)
	}
	cl, err := mongo.Connect(opts)
	if err != nil {
		return nil, err
	}
	return &MongoDB{t: t, cl: cl, log: log, c: rate.NewCounters()}, nil
}

// Close disconnects.
func (m *MongoDB) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return m.cl.Disconnect(ctx)
}

// Num converts any BSON number (int32, int64, float64, Decimal128) to float64.
func Num(v any) (float64, bool) {
	switch x := v.(type) {
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case int:
		return float64(x), true
	case bson.Decimal128:
		f, err := strconv.ParseFloat(x.String(), 64)
		return f, err == nil
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// Path walks nested documents (bson.M / bson.D) and returns the value at keys.
func Path(v any, keys ...string) (any, bool) {
	for _, k := range keys {
		switch d := v.(type) {
		case bson.M:
			x, ok := d[k]
			if !ok {
				return nil, false
			}
			v = x
		case bson.D:
			found := false
			for _, e := range d {
				if e.Key == k {
					v, found = e.Value, true
					break
				}
			}
			if !found {
				return nil, false
			}
		default:
			return nil, false
		}
	}
	return v, true
}

// PathNum is Path + Num.
func PathNum(v any, keys ...string) (float64, bool) {
	x, ok := Path(v, keys...)
	if !ok {
		return 0, false
	}
	return Num(x)
}

// Scrape implements Scraper.
func (m *MongoDB) Scrape(ctx context.Context) (emit.Result, error) {
	var r emit.Result
	now := time.Now()
	admin := m.cl.Database("admin")
	var ss bson.M
	cmd := bson.D{{Key: "serverStatus", Value: 1}, {Key: "opLatencies", Value: bson.D{{Key: "histograms", Value: true}}}}
	if err := admin.RunCommand(ctx, cmd).Decode(&ss); err != nil {
		return r, fmt.Errorf("serverStatus: %w", err)
	}
	role, lags := m.replication(ctx)
	base := emit.DBAttrs(m.t.Key(), m.t.DisplayName(), "mongodb", m.t.NodeName(), role)
	if cur, ok := PathNum(ss, "connections", "current"); ok {
		r.Gauge(emit.DBConnections, cur, base...)
		if avail, ok := PathNum(ss, "connections", "available"); ok {
			r.Gauge(emit.DBMaxConnections, cur+avail, base...)
		}
	}
	var ops float64
	for _, k := range []string{"insert", "query", "update", "delete", "getmore", "command"} {
		v, _ := PathNum(ss, "opcounters", k)
		ops += v
	}
	if v, ok := m.c.Rate("ops", ops, now); ok {
		r.Gauge(emit.DBOps, v, base...)
	}
	if req, ok := PathNum(ss, "wiredTiger", "cache", "pages requested from the cache"); ok {
		miss, _ := PathNum(ss, "wiredTiger", "cache", "pages read into cache")
		dReq, _, ok1 := m.c.Delta("wt_req", req, now)
		dMiss, _, ok2 := m.c.Delta("wt_miss", miss, now)
		if hr, ok := rate.IntervalHitRatio(dReq-dMiss, dMiss, ok1 && ok2, req-miss, miss); ok {
			r.Gauge(emit.DBHitRatio, rate.Clamp01(hr), base...)
		}
		un, _ := PathNum(ss, "wiredTiger", "cache", "unmodified pages evicted")
		mod, _ := PathNum(ss, "wiredTiger", "cache", "modified pages evicted")
		if d, dt, ok := m.c.Delta("wt_evict", un+mod, now); ok {
			r.Gauge(emit.DBEvictions, rate.PerMinute(d, dt), base...)
		}
	}
	if res, ok := PathNum(ss, "mem", "resident"); ok {
		if total := m.hostMemMB(ctx, now); total > 0 {
			r.Gauge(emit.DBMemory, rate.Clamp01(res/total), base...)
		}
	}
	if p95, ok := m.latencyP95(ss, now); ok {
		r.Gauge(emit.DBLatencyP95, p95, base...)
	}
	for _, l := range lags {
		a := base
		if l.node != "" {
			a = emit.DBAttrs(m.t.Key(), m.t.DisplayName(), "mongodb", l.node, "replica")
		}
		r.Gauge(emit.DBReplLag, l.ms, a...)
	}
	m.topQueries(ctx, now, base, &r)
	m.c.Prune(now.Add(-10 * m.t.Interval.D()))
	return r, nil
}

func (m *MongoDB) hostMemMB(ctx context.Context, now time.Time) float64 {
	if m.memSizeMB > 0 || (!m.hostInfoAt.IsZero() && now.Sub(m.hostInfoAt) < 10*time.Minute) {
		return m.memSizeMB
	}
	m.hostInfoAt = now
	var hi bson.M
	if err := m.cl.Database("admin").RunCommand(ctx, bson.D{{Key: "hostInfo", Value: 1}}).Decode(&hi); err == nil {
		m.memSizeMB, _ = PathNum(hi, "system", "memSizeMB")
	}
	return m.memSizeMB
}

type mongoLag struct {
	node string // "" = this node
	ms   float64
}

// replication returns this node's role and replication lags.
func (m *MongoDB) replication(ctx context.Context) (string, []mongoLag) {
	var st bson.M
	err := m.cl.Database("admin").RunCommand(ctx, bson.D{{Key: "replSetGetStatus", Value: 1}}).Decode(&st)
	if err != nil {
		return "primary", nil // standalone (NoReplicationEnabled) or no privilege
	}
	return ReplStatus(st)
}

// ReplStatus derives role and lags from a replSetGetStatus document.
func ReplStatus(st bson.M) (string, []mongoLag) {
	myState, _ := PathNum(st, "myState")
	members, _ := Path(st, "members")
	var list []any
	switch x := members.(type) {
	case bson.A:
		list = x
	case []any:
		list = x
	}
	var primary, self time.Time
	type mem struct {
		name  string
		state float64
		op    time.Time
		self  bool
	}
	var ms []mem
	for _, raw := range list {
		var e mem
		e.name, _ = func() (string, bool) { v, ok := Path(raw, "name"); s, _ := v.(string); return s, ok }()
		e.state, _ = PathNum(raw, "state")
		if v, ok := Path(raw, "optimeDate"); ok {
			e.op = asTime(v)
		}
		if v, ok := Path(raw, "self"); ok {
			e.self, _ = v.(bool)
		}
		if e.state == 1 {
			primary = e.op
		}
		if e.self {
			self = e.op
		}
		ms = append(ms, e)
	}
	switch myState {
	case 1:
		var lags []mongoLag
		for _, e := range ms {
			if e.state == 2 && !primary.IsZero() && !e.op.IsZero() {
				lags = append(lags, mongoLag{node: e.name, ms: max(float64(primary.Sub(e.op).Milliseconds()), 0)})
			}
		}
		return "primary", lags
	case 2:
		if !primary.IsZero() && !self.IsZero() {
			return "replica", []mongoLag{{ms: max(float64(primary.Sub(self).Milliseconds()), 0)}}
		}
		return "replica", nil
	case 7:
		return "arbiter", nil
	}
	return "replica", nil
}

func asTime(v any) time.Time {
	switch x := v.(type) {
	case bson.DateTime:
		return x.Time()
	case time.Time:
		return x
	}
	return time.Time{}
}

// HistogramQuantile returns the q-quantile (in the bucket's unit) of a histogram given each bucket's
// lower bound and count (MongoDB opLatencies: bucket i covers [lower[i], lower[i+1])). The value is
// interpolated linearly inside the bucket; the last bucket returns its lower bound.
func HistogramQuantile(lower, counts []float64, q float64) (float64, bool) {
	var total float64
	for _, c := range counts {
		total += c
	}
	if total <= 0 || len(lower) != len(counts) {
		return 0, false
	}
	target := q * total
	var cum float64
	for i, c := range counts {
		if c <= 0 {
			continue
		}
		if cum+c >= target {
			if i+1 >= len(lower) {
				return lower[i], true
			}
			frac := (target - cum) / c
			return lower[i] + frac*(lower[i+1]-lower[i]), true
		}
		cum += c
	}
	return lower[len(lower)-1], true
}

func (m *MongoDB) latencyP95(ss bson.M, now time.Time) (float64, bool) {
	merged := map[float64]float64{} // bucket lower bound (µs) → Δcount
	haveHist := false
	var weighted []rate.Weighted
	for _, class := range []string{"reads", "writes"} {
		if lat, ok := PathNum(ss, "opLatencies", class, "latency"); ok {
			n, _ := PathNum(ss, "opLatencies", class, "ops")
			dl, _, ok1 := m.c.Delta("lat/"+class, lat, now)
			dn, _, ok2 := m.c.Delta("latops/"+class, n, now)
			if ok1 && ok2 && dn > 0 {
				weighted = append(weighted, rate.Weighted{Value: dl / dn / 1000, Weight: dn})
			}
		}
		h, ok := Path(ss, "opLatencies", class, "histogram")
		if !ok {
			continue
		}
		var buckets []any
		switch x := h.(type) {
		case bson.A:
			buckets = x
		case []any:
			buckets = x
		}
		for _, b := range buckets {
			lo, ok1 := PathNum(b, "micros")
			c, ok2 := PathNum(b, "count")
			if !ok1 || !ok2 {
				continue
			}
			if d, _, ok := m.c.Delta(fmt.Sprintf("hist/%s/%g", class, lo), c, now); ok {
				merged[lo] += d
				haveHist = true
			}
		}
	}
	if haveHist {
		lows := make([]float64, 0, len(merged))
		for lo := range merged {
			lows = append(lows, lo)
		}
		sort.Float64s(lows)
		counts := make([]float64, len(lows))
		for i, lo := range lows {
			counts[i] = merged[lo]
		}
		if v, ok := HistogramQuantile(lows, counts, 0.95); ok {
			return v / 1000, true
		}
	}
	return rate.WeightedQuantile(weighted, 0.95)
}

// mongoQuery is one normalized query shape's interval totals.
type mongoQuery struct {
	hash, text            string
	calls, totalMs, rowsN float64
	p95                   float64
	samples               []float64 // profiler: per-op ms
}

func (m *MongoDB) topQueries(ctx context.Context, now time.Time, base []attribute.KeyValue, r *emit.Result) {
	src := m.t.TopQueriesSource
	if src == "none" {
		return
	}
	var qs []*mongoQuery
	used := false
	if (src == "auto" || src == "querystats") && !m.noQS {
		var err error
		qs, err = m.queryStats(ctx, now)
		if err != nil {
			if !m.warnedQS {
				m.warnedQS = true
				m.log.Warn("mongodb $queryStats unavailable (needs 7.0.12+/8.0 and queryStatsRead/clusterMonitor; self-managed: setParameter internalQueryStatsRateLimit=-1)",
					"target", m.t.Key(), "err", err)
			}
			if src == "auto" {
				m.noQS = true
			}
		} else if len(qs) > 0 || src == "querystats" {
			used = true
		}
	}
	if !used && (src == "auto" || src == "profiler") {
		pq, slow, ok := m.profiler(ctx, now)
		if ok {
			qs = pq
			r.Gauge(emit.DBSlow, slow, base...)
		}
	}
	rank := make([]float64, len(qs))
	for i, q := range qs {
		rank[i] = q.totalMs + 1e-9*q.calls
	}
	for _, i := range TopN(rank, m.t.TopQueries) {
		q := qs[i]
		a := []attribute.KeyValue{
			attribute.String(emit.AttrDBID, m.t.Key()),
			attribute.String(emit.AttrQueryHash, q.hash),
			attribute.String(emit.AttrQueryText, NormalizeQueryText(q.text)),
		}
		r.Count(emit.DBQueryCalls, q.calls, a...)
		r.Count(emit.DBQueryTotalMs, q.totalMs, a...)
		r.Count(emit.DBQueryRows, q.rowsN, a...)
		r.Gauge(emit.DBQueryP95Ms, q.p95, a...)
	}
}

type qsEntry struct {
	Key            bson.Raw `bson:"key"`
	KeyHash        string   `bson:"keyHash"`
	QueryShapeHash string   `bson:"queryShapeHash"`
	Metrics        bson.M   `bson:"metrics"`
}

var internalDBs = map[string]bool{"admin": true, "local": true, "config": true}

// queryStats reads $queryStats and returns the interval deltas per query shape.
func (m *MongoDB) queryStats(ctx context.Context, now time.Time) ([]*mongoQuery, error) {
	cur, err := m.cl.Database("admin").Aggregate(ctx, mongo.Pipeline{{{Key: "$queryStats", Value: bson.D{}}}})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	since := m.lastQS
	m.lastQS = now
	byShape := map[string]*mongoQuery{}
	var order []string
	for cur.Next(ctx) {
		var e qsEntry
		if err := cur.Decode(&e); err != nil {
			continue
		}
		shape, err := e.Key.LookupErr("queryShape")
		if err != nil {
			continue
		}
		sd, ok := shape.DocumentOK()
		if !ok {
			continue
		}
		db, _ := sd.Lookup("cmdNs", "db").StringValueOK()
		if internalDBs[db] {
			continue
		}
		calls, _ := PathNum(e.Metrics, "execCount")
		sum, _ := PathNum(e.Metrics, "totalExecMicros", "sum")
		sumSq, _ := PathNum(e.Metrics, "totalExecMicros", "sumOfSquares")
		docs, _ := PathNum(e.Metrics, "docsReturned", "sum")
		dc, ok := m.c.NewKeyDelta("qs/calls/"+e.KeyHash, calls, now, since)
		dt, _ := m.c.NewKeyDelta("qs/time/"+e.KeyHash, sum, now, since)
		dr, _ := m.c.NewKeyDelta("qs/docs/"+e.KeyHash, docs, now, since)
		if !ok || dc <= 0 {
			continue
		}
		id := e.QueryShapeHash
		if id == "" {
			id = e.KeyHash
		}
		q := byShape[id]
		if q == nil {
			hash := HexDigestToUint64(e.QueryShapeHash)
			if e.QueryShapeHash == "" {
				hash = HashText(e.KeyHash)
			}
			q = &mongoQuery{hash: hash, text: ShapeText(sd)}
			byShape[id] = q
			order = append(order, id)
		}
		q.calls += dc
		q.totalMs += dt / 1000
		q.rowsN += dr
		if calls > 0 {
			mean := sum / calls
			variance := math.Max(sumSq/calls-mean*mean, 0)
			q.p95 = max(q.p95, rate.ApproxP95(mean, math.Sqrt(variance))/1000)
		}
	}
	if err := cur.Err(); err != nil {
		return nil, err
	}
	out := make([]*mongoQuery, 0, len(order))
	for _, id := range order {
		out = append(out, byShape[id])
	}
	return out, nil
}

// ShapeText renders a $queryStats queryShape as "<command> <db>.<coll> <rest as relaxed JSON>",
// e.g. `find shop.orders {"filter":{"status":{"$eq":"?string"}}}`. Literals are already replaced
// by the server ("?string", "?number").
func ShapeText(shape bson.Raw) string {
	cmd, _ := shape.Lookup("command").StringValueOK()
	db, _ := shape.Lookup("cmdNs", "db").StringValueOK()
	coll, _ := shape.Lookup("cmdNs", "coll").StringValueOK()
	rest := bson.D{}
	elems, _ := shape.Elements()
	for _, el := range elems {
		k := el.Key()
		if k == "cmdNs" || k == "command" {
			continue
		}
		rest = append(rest, bson.E{Key: k, Value: el.Value()})
	}
	ns := db
	if coll != "" {
		ns += "." + coll
	}
	s := strings.TrimSpace(cmd + " " + ns)
	if len(rest) > 0 {
		if b, err := bson.MarshalExtJSON(rest, false, false); err == nil {
			s += " " + string(b)
		}
	}
	return s
}

// profiler reads system.profile of every database with profiling enabled.
func (m *MongoDB) profiler(ctx context.Context, now time.Time) ([]*mongoQuery, float64, bool) {
	if m.profDBs == nil || now.Sub(m.lastProfDBs) > 5*time.Minute {
		m.lastProfDBs = now
		m.profDBs = []string{}
		names, err := m.cl.ListDatabaseNames(ctx, bson.D{})
		if err != nil {
			if !m.warnedProf {
				m.warnedProf = true
				m.log.Warn("mongodb listDatabases failed; profiler top queries disabled", "target", m.t.Key(), "err", err)
			}
			return nil, 0, false
		}
		for _, n := range names {
			if internalDBs[n] {
				continue
			}
			var p bson.M
			if err := m.cl.Database(n).RunCommand(ctx, bson.D{{Key: "profile", Value: -1}}).Decode(&p); err == nil {
				if lvl, _ := PathNum(p, "was"); lvl > 0 {
					m.profDBs = append(m.profDBs, n)
				}
			}
		}
	}
	if len(m.profDBs) == 0 {
		return nil, 0, false
	}
	since := m.profSince
	m.profSince = now
	if since.IsZero() {
		return nil, 0, true // baseline only
	}
	byShape := map[string]*mongoQuery{}
	var order []string
	var slow float64
	for _, db := range m.profDBs {
		cur, err := m.cl.Database(db).Collection("system.profile").Find(ctx,
			bson.D{{Key: "ts", Value: bson.D{{Key: "$gt", Value: bson.NewDateTimeFromTime(since)}, {Key: "$lte", Value: bson.NewDateTimeFromTime(now)}}}},
			options.Find().SetLimit(5000).SetProjection(bson.D{{Key: "op", Value: 1}, {Key: "ns", Value: 1}, {Key: "command", Value: 1},
				{Key: "millis", Value: 1}, {Key: "nreturned", Value: 1}, {Key: "nMatched", Value: 1}, {Key: "ndeleted", Value: 1}}))
		if err != nil {
			continue
		}
		for cur.Next(ctx) {
			var e bson.M
			if err := cur.Decode(&e); err != nil {
				continue
			}
			text := ProfileShape(e)
			ms, _ := PathNum(e, "millis")
			rows, _ := PathNum(e, "nreturned")
			if rows == 0 {
				rows, _ = PathNum(e, "nMatched")
			}
			if ms >= m.t.SlowQueryMs {
				slow++
			}
			q := byShape[text]
			if q == nil {
				q = &mongoQuery{hash: HashText(text), text: text}
				byShape[text] = q
				order = append(order, text)
			}
			q.calls++
			q.totalMs += ms
			q.rowsN += rows
			q.samples = append(q.samples, ms)
		}
		cur.Close(ctx)
	}
	out := make([]*mongoQuery, 0, len(order))
	for _, k := range order {
		q := byShape[k]
		q.p95, _ = rate.Quantile(q.samples, 0.95)
		out = append(out, q)
	}
	return out, rate.PerMinute(slow, now.Sub(since)), true
}

// commandNoise are command fields that do not change a query's shape.
var commandNoise = map[string]bool{
	"lsid": true, "$db": true, "$clusterTime": true, "$readPreference": true, "txnNumber": true,
	"autocommit": true, "startTransaction": true, "comment": true, "maxTimeMS": true, "readConcern": true,
	"writeConcern": true, "$audit": true, "$client": true, "batchSize": true, "cursor": true, "apiVersion": true,
	"apiStrict": true, "apiDeprecationErrors": true, "documents": true, "singleBatch": true, "limit": true, "skip": true,
	"ordered": true, "bypassDocumentValidation": true, "$configTime": true, "$topologyTime": true, "mayBypassWriteBlocking": true,
}

// ProfileShape normalizes a system.profile entry into "<op> <ns> <command shape JSON>" with every
// literal replaced by "?" (field names, operators and array lengths up to the first element kept).
func ProfileShape(e bson.M) string {
	op, _ := e["op"].(string)
	ns, _ := e["ns"].(string)
	cmd := bson.M{}
	switch c := e["command"].(type) {
	case bson.M:
		cmd = c
	case bson.D:
		for _, el := range c {
			cmd[el.Key] = el.Value
		}
	}
	keys := make([]string, 0, len(cmd))
	for k := range cmd {
		if !commandNoise[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(op + " " + ns)
	if len(keys) > 0 {
		b.WriteString(" {")
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.Quote(k) + ":")
			if k == "find" || k == "aggregate" || k == "update" || k == "delete" || k == "count" || k == "distinct" || k == "findAndModify" || k == "insert" {
				b.WriteString(strconv.Quote(fmt.Sprint(cmd[k]))) // collection name
				continue
			}
			writeShape(&b, cmd[k])
		}
		b.WriteByte('}')
	}
	return b.String()
}

func writeShape(b *strings.Builder, v any) {
	switch x := v.(type) {
	case bson.M:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.Quote(k) + ":")
			writeShape(b, x[k])
		}
		b.WriteByte('}')
	case bson.D:
		b.WriteByte('{')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.Quote(e.Key) + ":")
			writeShape(b, e.Value)
		}
		b.WriteByte('}')
	case bson.A:
		b.WriteByte('[')
		if len(x) > 0 {
			writeShape(b, x[0])
			if len(x) > 1 {
				b.WriteString(",…")
			}
		}
		b.WriteByte(']')
	default:
		b.WriteString(`"?"`)
	}
}
