package scrape

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kmsg"

	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
)

// groupView is a consumer group of either protocol, reduced to what the scraper reports.
type groupView struct {
	Group string
	// State as reported (KIP-848 states title-cased: Stable, Empty, Assigning, Reconciling).
	State string
	// Generation identifies the group generation for the rebalance counter: the group epoch for
	// KIP-848 groups, the sorted member ids for classic groups (DescribeGroups has no generation).
	Generation string
	Members    int
	Assigned   kadm.TopicsSet
}

type topicMeta struct {
	retentionMs   float64
	haveRetention bool
}

type sizeReading struct {
	size, msgs float64
	at         time.Time
}

func (k *Kafka) warnOnce(key string, err error) {
	if err == nil {
		delete(k.warned, key)
		return
	}
	if !k.warned[key] {
		k.warned[key] = true
		k.log.Warn("kafka "+key+" unavailable", "target", k.t.Key(), "err", err)
	}
}

// TitleState turns KIP-848 lower-case states ("stable", "not_ready") into the classic spelling.
func TitleState(s string) string {
	if s == "" {
		return s
	}
	parts := strings.Split(strings.ToLower(s), "_")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

// describeGroups describes KIP-848 groups with ConsumerGroupDescribe (when any broker supports
// API key 69; re-checked every 10 min) and every other group with the classic DescribeGroups.
// ConsumerGroupDescribe answers classic groups with an error (GROUP_ID_NOT_FOUND), which is how
// the two protocols are told apart: ListGroups' type filter needs Kafka 3.8+ and is silently
// dropped by older brokers.
func (k *Kafka) describeGroups(ctx context.Context, groups []string, now time.Time) ([]groupView, error) {
	if k.kip848At.IsZero() || now.Sub(k.kip848At) > 10*time.Minute {
		k.kip848At = now
		k.kip848 = false
		if vs, err := k.adm.ApiVersions(ctx); err == nil {
			vs.Each(func(v kadm.BrokerApiVersions) {
				if _, ok := v.KeyMaxVersion(kmsg.ConsumerGroupDescribe.Int16()); ok && v.Err == nil {
					k.kip848 = true
				}
			})
		}
	}
	var out []groupView
	classic := groups
	if k.kip848 {
		dcg, err := k.adm.DescribeConsumerGroups(ctx, groups...)
		if err != nil && len(dcg) == 0 {
			k.warnOnce("consumer group describe (KIP-848)", err)
		}
		classic = nil
		for _, g := range groups {
			d, ok := dcg[g]
			if !ok || d.Err != nil {
				classic = append(classic, g)
				continue
			}
			out = append(out, groupView{
				Group:      g,
				State:      TitleState(d.State),
				Generation: "epoch:" + strconv.Itoa(int(d.Epoch)),
				Members:    len(d.Members),
				Assigned:   d.AssignedPartitions(),
			})
		}
	}
	if len(classic) > 0 {
		described, err := k.adm.DescribeGroups(ctx, classic...)
		if err != nil && len(described) == 0 {
			return nil, fmt.Errorf("describe groups: %w", err)
		}
		for _, g := range described.Sorted() {
			if g.Err != nil {
				continue
			}
			ids := make([]string, 0, len(g.Members))
			for _, m := range g.Members {
				ids = append(ids, m.MemberID)
			}
			out = append(out, groupView{Group: g.Group, State: g.State, Generation: GenerationKey(ids),
				Members: len(g.Members), Assigned: g.AssignedPartitions()})
		}
	}
	slices.SortFunc(out, func(a, b groupView) int { return strings.Compare(a.Group, b.Group) })
	return out, nil
}

// TopicSizeStats derives avg message size and byte rate from two (size, message count) readings.
// Preferred: Δbytes / Δmessages over the interval (new data only). When the interval has no
// usable delta (first scrape, no traffic, retention deleted segments so Δsize ≤ 0) the lifetime
// average size / retained messages is used and bytes_in_rate = in_rate × avg.
func TopicSizeStats(prev *sizeReading, size, msgs, retained float64, now time.Time, inRate float64, haveRate bool) (avg, bytesIn float64, haveAvg, haveBytes bool) {
	if prev != nil {
		dt := now.Sub(prev.at).Seconds()
		dSize, dMsgs := size-prev.size, msgs-prev.msgs
		if dt > 0 && dMsgs > 0 && dSize > 0 {
			return dSize / dMsgs, dSize / dt, true, true
		}
		if dt > 0 && dMsgs == 0 {
			bytesIn, haveBytes = 0, true
		}
	}
	if retained > 0 && size > 0 {
		avg, haveAvg = size/retained, true
		if !haveBytes && haveRate {
			bytesIn, haveBytes = inRate*avg, true
		}
	}
	return avg, bytesIn, haveAvg, haveBytes
}

// topicMetadata reports trooper.broker.topic.* for the scraped topics and returns per-broker disk
// usage from the log-dir volume totals:
//
//   - partitions / replication: metadata (replication = max replica count over partitions)
//   - retention_ms: DescribeConfigs retention.ms (-1 = unlimited), refreshed every topicMetadataEvery
//   - size_bytes: DescribeLogDirs, per partition the largest replica (one copy, not × RF)
//   - in_rate: Σ partitions' Δend offset / s
//   - avg_msg_bytes / bytes_in_rate: TopicSizeStats (on-disk bytes, i.e. after compression and
//     including record-batch overhead; no record is fetched)
func (k *Kafka) topicMetadata(ctx context.Context, topics kadm.TopicDetails, names []string, ends, starts kadm.ListedOffsets,
	produce map[string]map[int32]float64, now time.Time, r *emit.Result) map[int32]float64 {
	if len(names) == 0 {
		return nil
	}
	cluster := k.t.Key()
	if k.meta == nil || now.Sub(k.metaAt) >= k.t.TopicMetadataEvery.D() {
		rcs, err := k.adm.DescribeTopicConfigs(ctx, names...)
		k.warnOnce("describe topic configs", err)
		if err == nil || len(rcs) > 0 {
			k.meta = map[string]topicMeta{}
			k.metaAt = now
			for _, rc := range rcs {
				if rc.Err != nil {
					continue
				}
				for _, c := range rc.Configs {
					if c.Key == "retention.ms" && c.Value != nil {
						if v, err := strconv.ParseFloat(*c.Value, 64); err == nil {
							if v < 0 {
								v = -1
							}
							k.meta[rc.Name] = topicMeta{retentionMs: v, haveRetention: true}
						}
					}
				}
			}
		}
	}

	set := kadm.TopicsSet{}
	for _, n := range names {
		for _, p := range topics[n].Partitions {
			set.Add(n, p.Partition)
		}
	}
	dirs, err := k.adm.DescribeAllLogDirs(ctx, set)
	if err != nil && len(dirs) == 0 {
		k.warnOnce("describe log dirs", err)
	} else {
		k.warnOnce("describe log dirs", nil)
	}
	sizes := map[string]map[int32]int64{}
	type vol struct{ total, usable int64 }
	vols := map[int32]vol{}
	dirs.Each(func(d kadm.DescribedLogDir) {
		if d.Err != nil {
			return
		}
		if d.TotalBytes > 0 && d.UsableBytes >= 0 {
			v := vols[d.Broker]
			v.total += d.TotalBytes
			v.usable += d.UsableBytes
			vols[d.Broker] = v
		}
		d.Topics.Each(func(p kadm.DescribedLogDirPartition) {
			if p.IsFuture {
				return
			}
			if sizes[p.Topic] == nil {
				sizes[p.Topic] = map[int32]int64{}
			}
			sizes[p.Topic][p.Partition] = max(sizes[p.Topic][p.Partition], p.Size)
		})
	})

	for _, n := range names {
		td := topics[n]
		ta := emit.TopicAttrs(cluster, "kafka", n)
		repl := 0
		for _, p := range td.Partitions {
			repl = max(repl, len(p.Replicas))
		}
		r.Gauge(emit.BrokerTopicPartitions, float64(len(td.Partitions)), ta...)
		if repl > 0 {
			r.Gauge(emit.BrokerTopicReplication, float64(repl), ta...)
		}
		if m, ok := k.meta[n]; ok && m.haveRetention {
			r.Gauge(emit.BrokerTopicRetentionMs, m.retentionMs, ta...)
		}
		var inRate float64
		haveRate := len(produce[n]) > 0
		for _, v := range produce[n] {
			inRate += v
		}
		if haveRate {
			r.Gauge(emit.BrokerTopicInRate, inRate, ta...)
		}
		ps, haveSize := sizes[n]
		if !haveSize {
			continue
		}
		var size, msgs, retained float64
		for _, s := range ps {
			size += float64(s)
		}
		for p, e := range ends[n] {
			if e.Err != nil || e.Offset < 0 {
				continue
			}
			msgs += float64(e.Offset)
			retained += float64(e.Offset - startOf(starts, n, p))
		}
		r.Gauge(emit.BrokerTopicSizeBytes, size, ta...)
		var prev *sizeReading
		if p, ok := k.prevSz[n]; ok {
			prev = &p
		}
		avg, bytesIn, haveAvg, haveBytes := TopicSizeStats(prev, size, msgs, retained, now, inRate, haveRate)
		k.prevSz[n] = sizeReading{size: size, msgs: msgs, at: now}
		if haveAvg {
			r.Gauge(emit.BrokerTopicAvgMsgBytes, avg, ta...)
		}
		if haveBytes {
			r.Gauge(emit.BrokerTopicBytesInRate, bytesIn, ta...)
		}
	}
	for n, p := range k.prevSz {
		if now.Sub(p.at) > 10*k.t.Interval.D() {
			delete(k.prevSz, n)
		}
	}
	disks := map[int32]float64{}
	for b, v := range vols {
		disks[b] = 1 - float64(v.usable)/float64(v.total)
	}
	return disks
}

// brokerNodeName maps a broker id to trooper.broker.node: the configured node name, else "broker-<id>".
func (k *Kafka) brokerNodeName(id int32) string {
	for _, n := range k.t.Nodes {
		if n.BrokerID != nil && *n.BrokerID == id {
			if n.Name != "" {
				return n.Name
			}
			if n.MetricsURL != "" {
				return hostOf(n.MetricsURL)
			}
		}
	}
	return "broker-" + strconv.Itoa(int(id))
}

// nodeMetrics scrapes the configured Prometheus endpoints and adds log-dir disk usage for brokers
// whose endpoint did not report disk.
func (k *Kafka) nodeMetrics(ctx context.Context, disks map[int32]float64, now time.Time, r *emit.Result) {
	cluster := k.t.Key()
	haveDisk := map[string]bool{}
	for _, n := range k.t.Nodes {
		if n.MetricsURL == "" {
			continue
		}
		name := n.Name
		if name == "" {
			name = hostOf(n.MetricsURL)
		}
		samples, err := k.prom.fetch(ctx, n.MetricsURL)
		k.warnOnce("node metrics "+name, err)
		if err != nil {
			continue
		}
		vals := k.nodes.Map(name, samples, n.DiskMount, now)
		emitNode(r, cluster, "kafka", name, vals)
		if _, ok := vals[emit.BrokerNodeDisk]; ok {
			haveDisk[name] = true
		}
	}
	ids := make([]int32, 0, len(disks))
	for id := range disks {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		name := k.brokerNodeName(id)
		if !haveDisk[name] {
			r.Gauge(emit.BrokerNodeDisk, disks[id], emit.NodeAttrs(cluster, "kafka", name)...)
		}
	}
}

func emitNode(r *emit.Result, cluster, system, node string, vals NodeValues) {
	for _, m := range []string{emit.BrokerNodeCPU, emit.BrokerNodeMemory, emit.BrokerNodeDisk, emit.BrokerNodeNetIn} {
		if v, ok := vals[m]; ok {
			r.Gauge(m, v, emit.NodeAttrs(cluster, system, node)...)
		}
	}
}
