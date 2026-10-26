// Package emit turns scrape results into OTLP metrics (contracts §5) via the OpenTelemetry SDK.
package emit

import "go.opentelemetry.io/otel/attribute"

// Metric names (contracts §5). Gauges unless noted.
const (
	DBCPU            = "trooper.db.cpu"             // 0..1
	DBMemory         = "trooper.db.memory"          // 0..1
	DBDisk           = "trooper.db.disk"            // 0..1
	DBConnections    = "trooper.db.connections"     //
	DBMaxConnections = "trooper.db.max_connections" //
	DBOps            = "trooper.db.ops"             // per second
	DBLatencyP95     = "trooper.db.latency.p95"     // ms
	DBSlow           = "trooper.db.slow"            // per minute
	DBHitRatio       = "trooper.db.hit_ratio"       // 0..1
	DBEvictions      = "trooper.db.evictions"       // per minute
	DBReplLag        = "trooper.db.replication_lag" // ms

	// Query metrics: calls/total_ms/rows are monotonic Sums with DELTA temporality (the amount
	// in the export interval; the writer sums them per minute), p95_ms is a gauge.
	DBQueryCalls   = "trooper.db.query.calls"
	DBQueryTotalMs = "trooper.db.query.total_ms"
	DBQueryRows    = "trooper.db.query.rows"
	DBQueryP95Ms   = "trooper.db.query.p95_ms"

	BrokerLag             = "trooper.broker.lag"
	BrokerEndOffset       = "trooper.broker.end_offset"
	BrokerCommittedOffset = "trooper.broker.committed_offset"
	BrokerConsumeRate     = "trooper.broker.consume_rate" // msg/s
	BrokerProduceRate     = "trooper.broker.produce_rate" // msg/s
	BrokerUnacked         = "trooper.broker.unacked"
	BrokerGroupMembers    = "trooper.broker.group.members"
	BrokerGroupRebalances = "trooper.broker.group.rebalances" // per hour (sliding window)
	BrokerNodeCPU         = "trooper.broker.node.cpu"
	BrokerNodeMemory      = "trooper.broker.node.memory"
	BrokerNodeDisk        = "trooper.broker.node.disk"
	BrokerNodeNetIn       = "trooper.broker.node.net_in" // MB/s
	// Custom (→ metrics table): RabbitMQ file descriptor / Erlang process usage, 0..1.
	BrokerNodeFD   = "trooper.broker.node.fd"
	BrokerNodeProc = "trooper.broker.node.proc"

	// Topic / queue metadata (broker_topics_1m).
	BrokerTopicPartitions  = "trooper.broker.topic.partitions"
	BrokerTopicReplication = "trooper.broker.topic.replication"
	BrokerTopicRetentionMs = "trooper.broker.topic.retention_ms" // -1 = unlimited
	BrokerTopicAvgMsgBytes = "trooper.broker.topic.avg_msg_bytes"
	BrokerTopicInRate      = "trooper.broker.topic.in_rate"       // msg/s
	BrokerTopicBytesInRate = "trooper.broker.topic.bytes_in_rate" // byte/s
	BrokerTopicSizeBytes   = "trooper.broker.topic.size_bytes"
	// BrokerOldestAgeS is the age of the oldest waiting message (broker_lag_1m.oldest_age_s).
	BrokerOldestAgeS = "trooper.broker.oldest_age_s"

	// CollectorUp is 1 when the last scrape of a target succeeded, 0 otherwise (custom metric).
	CollectorUp = "trooper.collector.up"
)

// Attribute keys (contracts §5).
const (
	AttrDBID      = "trooper.db.id"
	AttrDBName    = "trooper.db.name"
	AttrDBSystem  = "db.system"
	AttrDBNode    = "trooper.db.node"
	AttrDBRole    = "trooper.db.role"
	AttrQueryHash = "trooper.db.query.hash"
	AttrQueryText = "db.query.text"

	AttrCluster     = "trooper.broker.cluster"
	AttrMsgSystem   = "messaging.system"
	AttrDestination = "messaging.destination.name"
	AttrGroup       = "messaging.consumer.group.name"
	AttrPartition   = "messaging.destination.partition.id"
	AttrGroupState  = "trooper.broker.group.state"
	AttrBrokerNode  = "trooper.broker.node"

	AttrTarget     = "trooper.collector.target"
	AttrTargetKind = "trooper.collector.kind"
	AttrError      = "error.type"
)

// GaugeNames lists every gauge the emitter registers.
var GaugeNames = []string{
	DBCPU, DBMemory, DBDisk, DBConnections, DBMaxConnections, DBOps, DBLatencyP95, DBSlow, DBHitRatio, DBEvictions, DBReplLag,
	DBQueryP95Ms,
	BrokerLag, BrokerEndOffset, BrokerCommittedOffset, BrokerConsumeRate, BrokerProduceRate, BrokerUnacked,
	BrokerGroupMembers, BrokerGroupRebalances,
	BrokerNodeCPU, BrokerNodeMemory, BrokerNodeDisk, BrokerNodeNetIn, BrokerNodeFD, BrokerNodeProc,
	BrokerTopicPartitions, BrokerTopicReplication, BrokerTopicRetentionMs, BrokerTopicAvgMsgBytes,
	BrokerTopicInRate, BrokerTopicBytesInRate, BrokerTopicSizeBytes, BrokerOldestAgeS,
	CollectorUp,
}

// CounterNames lists the delta sums.
var CounterNames = []string{DBQueryCalls, DBQueryTotalMs, DBQueryRows}

// Units per metric (UCUM), informational.
var units = map[string]string{
	DBCPU: "1", DBMemory: "1", DBDisk: "1", DBHitRatio: "1",
	DBOps: "{operation}/s", DBSlow: "{query}/min", DBEvictions: "{key}/min",
	DBLatencyP95: "ms", DBReplLag: "ms", DBQueryP95Ms: "ms", DBQueryTotalMs: "ms",
	BrokerConsumeRate: "{message}/s", BrokerProduceRate: "{message}/s",
	BrokerGroupRebalances: "{rebalance}/h",
	BrokerNodeCPU:         "1", BrokerNodeMemory: "1", BrokerNodeDisk: "1", BrokerNodeNetIn: "MBy/s",
	BrokerNodeFD: "1", BrokerNodeProc: "1",
	BrokerTopicRetentionMs: "ms", BrokerTopicAvgMsgBytes: "By", BrokerTopicInRate: "{message}/s",
	BrokerTopicBytesInRate: "By/s", BrokerTopicSizeBytes: "By", BrokerOldestAgeS: "s",
}

// Sample is one data point.
type Sample struct {
	Name  string
	Value float64
	Attrs attribute.Set
}

// S builds a sample.
func S(name string, v float64, attrs ...attribute.KeyValue) Sample {
	return Sample{Name: name, Value: v, Attrs: attribute.NewSet(attrs...)}
}

// With returns base attributes plus extra.
func With(base []attribute.KeyValue, extra ...attribute.KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(base)+len(extra))
	out = append(out, base...)
	return append(out, extra...)
}

// Result is what one scrape of one target produced.
type Result struct {
	// Gauges replace the target's previous snapshot.
	Gauges []Sample
	// Counts are added to the delta sums (query calls/total_ms/rows in this interval).
	Counts []Sample
}

// Gauge appends a gauge sample.
func (r *Result) Gauge(name string, v float64, attrs ...attribute.KeyValue) {
	r.Gauges = append(r.Gauges, S(name, v, attrs...))
}

// Count appends a delta-sum sample.
func (r *Result) Count(name string, v float64, attrs ...attribute.KeyValue) {
	r.Counts = append(r.Counts, S(name, v, attrs...))
}

// Merge appends other's samples.
func (r *Result) Merge(other Result) {
	r.Gauges = append(r.Gauges, other.Gauges...)
	r.Counts = append(r.Counts, other.Counts...)
}
