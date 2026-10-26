package flatten

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cespare/xxhash/v2"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"

	"github.com/tasktrooper/telemetry/services/ingest/internal/norm"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

// Collector tables are AggregatingMergeTree with SimpleAggregateFunction(max|min|sum). A column that
// was not reported must not win a merge, and ClickHouse max/min over NaN is order-dependent, so
// unreported max columns are -Inf and unreported min columns +Inf (readers treat non-finite as missing).
var (
	unsetMax = math.Inf(-1)
	unsetMin = math.Inf(1)
)

// MetricRow is one row of telemetry.metrics (custom/runtime metrics).
type MetricRow struct {
	OrgID, ProjectID uint32
	TS               time.Time
	Service, Env     string
	Name, Type, Unit string
	Attrs            map[string]string
	AttrsHash        uint64
	Value            float64
	Count            uint64
	BucketBounds     []float64
	BucketCounts     []uint64
}

// MetricColumns is the INSERT column list matching MetricRow.Values.
const MetricColumns = "org_id, project_id, ts, service, env, name, type, unit, attrs, attrs_hash, value, count, bucket_bounds, bucket_counts"

// Values returns the row in MetricColumns order.
func (r *MetricRow) Values() []any {
	return []any{r.OrgID, r.ProjectID, r.TS, r.Service, r.Env, r.Name, r.Type, r.Unit, r.Attrs, r.AttrsHash, r.Value, r.Count, r.BucketBounds, r.BucketCounts}
}

// ── collector tables ──

type DBKey struct {
	Org, Project uint32
	DatabaseID   string
	Node         string
	Minute       int64
}

// DBRow is one row of telemetry.db_1m.
type DBRow struct {
	Key          DBKey
	Engine, Role string
	V            [11]float64 // cpu, memory, disk, connections, max_connections, ops, p95_ms, slow, hit_ratio, evictions, repl_lag_ms
}

const DBColumns = "org_id, project_id, database_id, engine, node, role, minute, cpu, memory, disk, connections, max_connections, ops, p95_ms, slow, hit_ratio, evictions, repl_lag_ms"

const dbHitRatio = 8

func (r *DBRow) Values() []any {
	v := []any{r.Key.Org, r.Key.Project, r.Key.DatabaseID, r.Engine, r.Key.Node, r.Role, time.Unix(r.Key.Minute, 0).UTC()}
	for _, f := range r.V {
		v = append(v, f)
	}
	return v
}

var dbMetricCol = map[string]int{
	"trooper.db.cpu": 0, "trooper.db.memory": 1, "trooper.db.disk": 2, "trooper.db.connections": 3,
	"trooper.db.max_connections": 4, "trooper.db.ops": 5, "trooper.db.latency.p95": 6, "trooper.db.slow": 7,
	"trooper.db.hit_ratio": 8, "trooper.db.evictions": 9, "trooper.db.replication_lag": 10,
}

type DBQueryKey struct {
	Org, Project uint32
	DatabaseID   string
	QueryHash    uint64
	Minute       int64
}

// DBQueryRow is one row of telemetry.db_queries_1m.
type DBQueryRow struct {
	Key                         DBQueryKey
	Query                       string
	Calls, TotalMs, P95Ms, Rows float64
}

const DBQueryColumns = "org_id, project_id, database_id, query_hash, minute, query, calls, total_ms, p95_ms, rows"

func (r *DBQueryRow) Values() []any {
	return []any{r.Key.Org, r.Key.Project, r.Key.DatabaseID, r.Key.QueryHash, time.Unix(r.Key.Minute, 0).UTC(), r.Query, r.Calls, r.TotalMs, r.P95Ms, r.Rows}
}

type BrokerLagKey struct {
	Org, Project          uint32
	Cluster, Group, Topic string
	Partition             uint32
	Minute                int64
}

// BrokerLagRow is one row of telemetry.broker_lag_1m.
type BrokerLagRow struct {
	Key BrokerLagKey
	V   [7]float64 // lag, end_offset, committed_offset, consume_rate, produce_rate, unacked, oldest_age_s
}

const BrokerLagColumns = "org_id, project_id, cluster_id, consumer_group, topic, partition, minute, lag, end_offset, committed_offset, consume_rate, produce_rate, unacked, oldest_age_s"

func (r *BrokerLagRow) Values() []any {
	k := r.Key
	return []any{k.Org, k.Project, k.Cluster, k.Group, k.Topic, k.Partition, time.Unix(k.Minute, 0).UTC(), r.V[0], r.V[1], r.V[2], r.V[3], r.V[4], r.V[5], r.V[6]}
}

var brokerLagCol = map[string]int{
	"trooper.broker.lag": 0, "trooper.broker.end_offset": 1, "trooper.broker.committed_offset": 2,
	"trooper.broker.consume_rate": 3, "trooper.broker.produce_rate": 4, "trooper.broker.unacked": 5,
	"trooper.broker.oldest_age_s": 6,
}

type BrokerTopicKey struct {
	Org, Project   uint32
	Cluster, Topic string
	Minute         int64
}

// BrokerTopicRow is one row of telemetry.broker_topics_1m.
type BrokerTopicRow struct {
	Key BrokerTopicKey
	V   [7]float64 // partitions, replication, retention_ms, avg_msg_bytes, in_rate, bytes_in_rate, size_bytes
}

const BrokerTopicColumns = "org_id, project_id, cluster_id, topic, minute, partitions, replication, retention_ms, avg_msg_bytes, in_rate, bytes_in_rate, size_bytes"

func (r *BrokerTopicRow) Values() []any {
	k := r.Key
	v := []any{k.Org, k.Project, k.Cluster, k.Topic, time.Unix(k.Minute, 0).UTC()}
	for _, f := range r.V {
		v = append(v, f)
	}
	return v
}

var brokerTopicCol = map[string]int{
	"trooper.broker.topic.partitions": 0, "trooper.broker.topic.replication": 1, "trooper.broker.topic.retention_ms": 2,
	"trooper.broker.topic.avg_msg_bytes": 3, "trooper.broker.topic.in_rate": 4, "trooper.broker.topic.bytes_in_rate": 5,
	"trooper.broker.topic.size_bytes": 6,
}

type BrokerGroupKey struct {
	Org, Project   uint32
	Cluster, Group string
	Minute         int64
}

// BrokerGroupRow is one row of telemetry.broker_groups_1m.
type BrokerGroupRow struct {
	Key                 BrokerGroupKey
	State               string
	Members, Rebalances float64
}

const BrokerGroupColumns = "org_id, project_id, cluster_id, consumer_group, minute, state, members, rebalances"

func (r *BrokerGroupRow) Values() []any {
	k := r.Key
	return []any{k.Org, k.Project, k.Cluster, k.Group, time.Unix(k.Minute, 0).UTC(), r.State, r.Members, r.Rebalances}
}

type BrokerNodeKey struct {
	Org, Project  uint32
	Cluster, Node string
	Minute        int64
}

// BrokerNodeRow is one row of telemetry.broker_nodes_1m.
type BrokerNodeRow struct {
	Key BrokerNodeKey
	V   [4]float64 // cpu, memory, disk, net_in
}

const BrokerNodeColumns = "org_id, project_id, cluster_id, node, minute, cpu, memory, disk, net_in"

func (r *BrokerNodeRow) Values() []any {
	k := r.Key
	return []any{k.Org, k.Project, k.Cluster, k.Node, time.Unix(k.Minute, 0).UTC(), r.V[0], r.V[1], r.V[2], r.V[3]}
}

var brokerNodeCol = map[string]int{
	"trooper.broker.node.cpu": 0, "trooper.broker.node.memory": 1, "trooper.broker.node.disk": 2, "trooper.broker.node.net_in": 3,
}

// MetricsBatch accumulates metric rows; collector rows are merged per entity and minute so each flush
// writes one row per entity (contracts §5).
type MetricsBatch struct {
	Metrics      []MetricRow
	DB           map[DBKey]*DBRow
	DBQueries    map[DBQueryKey]*DBQueryRow
	BrokerLag    map[BrokerLagKey]*BrokerLagRow
	BrokerGroups map[BrokerGroupKey]*BrokerGroupRow
	BrokerNodes  map[BrokerNodeKey]*BrokerNodeRow
	BrokerTopics map[BrokerTopicKey]*BrokerTopicRow
}

// NewMetricsBatch returns an empty batch.
func NewMetricsBatch() *MetricsBatch {
	return &MetricsBatch{
		DB: map[DBKey]*DBRow{}, DBQueries: map[DBQueryKey]*DBQueryRow{}, BrokerLag: map[BrokerLagKey]*BrokerLagRow{},
		BrokerGroups: map[BrokerGroupKey]*BrokerGroupRow{}, BrokerNodes: map[BrokerNodeKey]*BrokerNodeRow{},
		BrokerTopics: map[BrokerTopicKey]*BrokerTopicRow{},
	}
}

// Len is the number of rows the batch would insert.
func (b *MetricsBatch) Len() int {
	return len(b.Metrics) + len(b.DB) + len(b.DBQueries) + len(b.BrokerLag) + len(b.BrokerGroups) + len(b.BrokerNodes) + len(b.BrokerTopics)
}

// Reset empties the batch, keeping allocations.
func (b *MetricsBatch) Reset() {
	b.Metrics = b.Metrics[:0]
	clear(b.DB)
	clear(b.DBQueries)
	clear(b.BrokerLag)
	clear(b.BrokerGroups)
	clear(b.BrokerNodes)
	clear(b.BrokerTopics)
}

// pair looks attributes up on the data point first, then on the resource.
type pair struct{ dp, res attrIndex }

func (p pair) str(k string) string {
	if s := p.dp.str(k); s != "" {
		return s
	}
	return p.res.str(k)
}

// Add flattens req into the batch.
func (b *MetricsBatch) Add(t tenant.Tenant, req *colmetricspb.ExportMetricsServiceRequest) {
	for _, rm := range req.GetResourceMetrics() {
		resKV := rm.GetResource().GetAttributes()
		res := newResource(resKV)
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				name := m.GetName()
				if strings.HasPrefix(name, "trooper.db.") || strings.HasPrefix(name, "trooper.broker.") {
					if b.addCollector(t, name, m, resKV) {
						continue
					}
				}
				b.addCustom(t, &res, m)
			}
		}
	}
}

// numberPoints returns gauge/sum data points.
func numberPoints(m *metricspb.Metric) []*metricspb.NumberDataPoint {
	switch d := m.GetData().(type) {
	case *metricspb.Metric_Gauge:
		return d.Gauge.GetDataPoints()
	case *metricspb.Metric_Sum:
		return d.Sum.GetDataPoints()
	}
	return nil
}

func numberValue(dp *metricspb.NumberDataPoint) float64 {
	switch v := dp.GetValue().(type) {
	case *metricspb.NumberDataPoint_AsDouble:
		return v.AsDouble
	case *metricspb.NumberDataPoint_AsInt:
		return float64(v.AsInt)
	}
	return 0
}

func minuteOf(ns uint64) int64 {
	if ns == 0 {
		return time.Now().Unix() / 60 * 60
	}
	return int64(ns/1e9) / 60 * 60
}

// addCollector routes a trooper.db.* / trooper.broker.* metric; false if the name is not a known collector metric.
func (b *MetricsBatch) addCollector(t tenant.Tenant, name string, m *metricspb.Metric, resKV []*commonpb.KeyValue) bool {
	pts := numberPoints(m)
	dbCol, isDB := dbMetricCol[name]
	lagCol, isLag := brokerLagCol[name]
	nodeCol, isNode := brokerNodeCol[name]
	topicCol, isTopic := brokerTopicCol[name]
	switch {
	case isDB:
		col := dbCol
		for _, dp := range pts {
			a := pair{attrIndex(dp.GetAttributes()), attrIndex(resKV)}
			k := DBKey{t.OrgID, t.ProjectID, a.str("trooper.db.id"), a.str("trooper.db.node"), minuteOf(dp.GetTimeUnixNano())}
			r := b.DB[k]
			if r == nil {
				r = &DBRow{Key: k}
				for i := range r.V {
					r.V[i] = unsetMax
				}
				r.V[dbHitRatio] = unsetMin
				b.DB[k] = r
			}
			if e := a.str("db.system"); e != "" {
				r.Engine = e
			}
			if role := a.str("trooper.db.role"); role != "" {
				r.Role = role
			}
			v := numberValue(dp)
			if col == dbHitRatio {
				r.V[col] = math.Min(r.V[col], v)
			} else {
				r.V[col] = math.Max(r.V[col], v)
			}
		}
		return true
	case strings.HasPrefix(name, "trooper.db.query."):
		field := strings.TrimPrefix(name, "trooper.db.query.")
		if field != "calls" && field != "total_ms" && field != "p95_ms" && field != "rows" {
			return false
		}
		for _, dp := range pts {
			a := pair{attrIndex(dp.GetAttributes()), attrIndex(resKV)}
			q := a.str("db.query.text")
			if q != "" {
				q = norm.Query(q)
			}
			h, err := strconv.ParseUint(a.str("trooper.db.query.hash"), 10, 64)
			if err != nil || h == 0 {
				h = norm.QueryHash(q)
				if h == 0 {
					h = xxhash.Sum64String(a.str("trooper.db.query.hash"))
				}
			}
			k := DBQueryKey{t.OrgID, t.ProjectID, a.str("trooper.db.id"), h, minuteOf(dp.GetTimeUnixNano())}
			r := b.DBQueries[k]
			if r == nil {
				r = &DBQueryRow{Key: k}
				b.DBQueries[k] = r
			}
			if q != "" {
				r.Query = q
			}
			v := numberValue(dp)
			switch field {
			case "calls":
				r.Calls += v
			case "total_ms":
				r.TotalMs += v
			case "p95_ms":
				r.P95Ms = math.Max(r.P95Ms, v)
			case "rows":
				r.Rows += v
			}
		}
		return true
	case isLag:
		col := lagCol
		for _, dp := range pts {
			a := pair{attrIndex(dp.GetAttributes()), attrIndex(resKV)}
			part, _ := strconv.ParseUint(a.str("messaging.destination.partition.id"), 10, 32)
			k := BrokerLagKey{t.OrgID, t.ProjectID, a.str("trooper.broker.cluster"), a.str("messaging.consumer.group.name"),
				a.str("messaging.destination.name"), uint32(part), minuteOf(dp.GetTimeUnixNano())}
			r := b.BrokerLag[k]
			if r == nil {
				r = &BrokerLagRow{Key: k}
				for i := range r.V {
					r.V[i] = unsetMax
				}
				b.BrokerLag[k] = r
			}
			r.V[col] = math.Max(r.V[col], numberValue(dp))
		}
		return true
	case name == "trooper.broker.group.members" || name == "trooper.broker.group.rebalances":
		for _, dp := range pts {
			a := pair{attrIndex(dp.GetAttributes()), attrIndex(resKV)}
			k := BrokerGroupKey{t.OrgID, t.ProjectID, a.str("trooper.broker.cluster"), a.str("messaging.consumer.group.name"), minuteOf(dp.GetTimeUnixNano())}
			r := b.BrokerGroups[k]
			if r == nil {
				r = &BrokerGroupRow{Key: k, Members: unsetMax, Rebalances: unsetMax}
				b.BrokerGroups[k] = r
			}
			if s := a.str("trooper.broker.group.state"); s != "" {
				r.State = s
			}
			v := numberValue(dp)
			if name == "trooper.broker.group.members" {
				r.Members = math.Max(r.Members, v)
			} else {
				r.Rebalances = math.Max(r.Rebalances, v)
			}
		}
		return true
	case isTopic:
		col := topicCol
		for _, dp := range pts {
			a := pair{attrIndex(dp.GetAttributes()), attrIndex(resKV)}
			k := BrokerTopicKey{t.OrgID, t.ProjectID, a.str("trooper.broker.cluster"), a.str("messaging.destination.name"), minuteOf(dp.GetTimeUnixNano())}
			r := b.BrokerTopics[k]
			if r == nil {
				r = &BrokerTopicRow{Key: k}
				for i := range r.V {
					r.V[i] = unsetMax
				}
				b.BrokerTopics[k] = r
			}
			r.V[col] = math.Max(r.V[col], numberValue(dp))
		}
		return true
	case isNode:
		col := nodeCol
		for _, dp := range pts {
			a := pair{attrIndex(dp.GetAttributes()), attrIndex(resKV)}
			k := BrokerNodeKey{t.OrgID, t.ProjectID, a.str("trooper.broker.cluster"), a.str("trooper.broker.node"), minuteOf(dp.GetTimeUnixNano())}
			r := b.BrokerNodes[k]
			if r == nil {
				r = &BrokerNodeRow{Key: k}
				for i := range r.V {
					r.V[i] = unsetMax
				}
				b.BrokerNodes[k] = r
			}
			r.V[col] = math.Max(r.V[col], numberValue(dp))
		}
		return true
	}
	return false
}

func (b *MetricsBatch) addCustom(t tenant.Tenant, res *resourceInfo, m *metricspb.Metric) {
	base := MetricRow{OrgID: t.OrgID, ProjectID: t.ProjectID, Service: res.service, Env: res.env, Name: m.GetName(), Unit: m.GetUnit()}
	emit := func(ns uint64, kvs []*commonpb.KeyValue, fill func(r *MetricRow)) {
		r := base
		r.TS = time.Unix(0, int64(ns)).UTC()
		if ns == 0 {
			r.TS = time.Now().UTC()
		}
		r.Attrs = attrMap(kvs, 0)
		r.AttrsHash = AttrsHash(r.Attrs)
		r.BucketBounds, r.BucketCounts = []float64{}, []uint64{}
		fill(&r)
		b.Metrics = append(b.Metrics, r)
	}
	switch d := m.GetData().(type) {
	case *metricspb.Metric_Gauge:
		for _, dp := range d.Gauge.GetDataPoints() {
			emit(dp.GetTimeUnixNano(), dp.GetAttributes(), func(r *MetricRow) { r.Type, r.Value = "gauge", numberValue(dp) })
		}
	case *metricspb.Metric_Sum:
		for _, dp := range d.Sum.GetDataPoints() {
			emit(dp.GetTimeUnixNano(), dp.GetAttributes(), func(r *MetricRow) { r.Type, r.Value = "sum", numberValue(dp) })
		}
	case *metricspb.Metric_Histogram:
		for _, dp := range d.Histogram.GetDataPoints() {
			emit(dp.GetTimeUnixNano(), dp.GetAttributes(), func(r *MetricRow) {
				r.Type, r.Value, r.Count = "histogram", dp.GetSum(), dp.GetCount()
				r.BucketBounds = append(r.BucketBounds, dp.GetExplicitBounds()...)
				r.BucketCounts = append(r.BucketCounts, dp.GetBucketCounts()...)
			})
		}
	case *metricspb.Metric_ExponentialHistogram:
		for _, dp := range d.ExponentialHistogram.GetDataPoints() {
			emit(dp.GetTimeUnixNano(), dp.GetAttributes(), func(r *MetricRow) {
				r.Type, r.Value, r.Count = "exp_histogram", dp.GetSum(), dp.GetCount()
				// Positive buckets as explicit upper bounds: base^(offset+i+1), base = 2^(2^-scale); zero bucket first.
				base := math.Exp2(math.Exp2(-float64(dp.GetScale())))
				r.BucketBounds = append(r.BucketBounds, 0)
				r.BucketCounts = append(r.BucketCounts, dp.GetZeroCount())
				pos := dp.GetPositive()
				for i, c := range pos.GetBucketCounts() {
					r.BucketBounds = append(r.BucketBounds, math.Pow(base, float64(int(pos.GetOffset())+i+1)))
					r.BucketCounts = append(r.BucketCounts, c)
				}
			})
		}
	case *metricspb.Metric_Summary:
		for _, dp := range d.Summary.GetDataPoints() {
			emit(dp.GetTimeUnixNano(), dp.GetAttributes(), func(r *MetricRow) {
				r.Type, r.Value, r.Count = "summary", dp.GetSum(), dp.GetCount()
				for _, q := range dp.GetQuantileValues() {
					putAttr(r.Attrs, "quantile."+strconv.FormatFloat(q.GetQuantile(), 'g', -1, 64), strconv.FormatFloat(q.GetValue(), 'g', -1, 64), MaxAttrValueLen)
				}
			})
		}
	}
}

// AttrsHash is a stable series id over the sorted attributes.
func AttrsHash(m map[string]string) uint64 {
	if len(m) == 0 {
		return 0
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if !strings.HasPrefix(k, "quantile.") {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	d := xxhash.New()
	for _, k := range keys {
		_, _ = d.WriteString(k)
		_, _ = d.Write([]byte{'='})
		_, _ = d.WriteString(m[k])
		_, _ = d.Write([]byte{0})
	}
	return d.Sum64()
}
