package scrape

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"

	"github.com/tasktrooper/telemetry/services/collector/internal/config"
	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
	"github.com/tasktrooper/telemetry/services/collector/internal/rate"
)

// Kafka reads offsets and group metadata with the admin API; it never consumes or produces.
// Needed ACLs: Describe on cluster, topics and groups.
//
//   - end_offset per (topic, partition) from ListOffsets(latest); produce_rate = Δend / s
//
//   - committed_offset per (group, topic, partition) from OffsetFetch; consume_rate = Δcommitted / s
//
//   - lag = end − committed (when a partition is assigned but never committed: end − log start)
//
//   - group.members / group.state from DescribeGroups; group.rebalances = rebalances in the last
//     hour (rate.RebalanceCounter: DescribeGroups has no generation id, member-set changes are used)
//
//   - topics nobody reads are reported with an empty group so "no readers" can be shown
//
//   - KIP-848 groups (new consumer protocol) via ConsumerGroupDescribe; rebalances = group epoch bumps
//
//   - topic metadata (trooper.broker.topic.*): see topicMetadata
//
// Broker node CPU / memory / network are not available over the Kafka protocol: they come from
// optional Prometheus endpoints (nodes[].metricsUrl, see NodeMapper) or the cloud integration.
// Disk usage comes from DescribeLogDirs volume totals (Kafka 3.3+) when no endpoint reports it.
type Kafka struct {
	t      config.Broker
	cl     *kgo.Client
	adm    *kadm.Client
	log    *slog.Logger
	c      *rate.Counters
	rb     *rate.RebalanceCounter
	topicF *regexp.Regexp
	groupF *regexp.Regexp

	// KIP-848: ConsumerGroupDescribe support, re-checked every 10 min
	kip848   bool
	kip848At time.Time

	meta   map[string]topicMeta // retention etc., refreshed every TopicMetadataEvery
	metaAt time.Time
	prevSz map[string]sizeReading // topic → previous (size, messages) for avg message size

	nodes  *NodeMapper
	prom   *promClient
	warned map[string]bool
}

// KafkaOpts builds the franz-go client options for a broker target (TLS + SASL).
func KafkaOpts(t config.Broker) ([]kgo.Opt, error) {
	seeds := strings.FieldsFunc(t.Endpoint, func(r rune) bool { return r == ',' || r == ' ' })
	opts := []kgo.Opt{
		kgo.SeedBrokers(seeds...),
		kgo.ClientID("trooper-agent"),
		kgo.DialTimeout(t.Timeout.D()),
		kgo.RequestTimeoutOverhead(t.Timeout.D()),
		kgo.MetadataMinAge(5 * time.Second),
	}
	a := t.Auth
	if a.TLS || a.CAFile != "" || a.CertFile != "" || a.Mechanism == "iam" { // MSK IAM listeners (9098) are TLS-only
		tc := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: a.InsecureSkipVerify} //nolint:gosec // opt-in
		if a.CAFile != "" {
			pem, err := os.ReadFile(a.CAFile)
			if err != nil {
				return nil, fmt.Errorf("kafka caFile: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errors.New("kafka caFile: no certificates found")
			}
			tc.RootCAs = pool
		}
		if a.CertFile != "" {
			cert, err := tls.LoadX509KeyPair(a.CertFile, a.KeyFile)
			if err != nil {
				return nil, fmt.Errorf("kafka client cert: %w", err)
			}
			tc.Certificates = []tls.Certificate{cert}
		}
		opts = append(opts, kgo.DialTLSConfig(tc))
	}
	switch a.Mechanism {
	case "", "none":
	case "plain", "scram-sha-256", "scram-sha-512":
		user, pass, err := t.Credentials()
		if err != nil {
			return nil, err
		}
		switch a.Mechanism {
		case "plain":
			opts = append(opts, kgo.SASL(plain.Auth{User: user, Pass: pass}.AsMechanism()))
		case "scram-sha-256":
			opts = append(opts, kgo.SASL(scram.Auth{User: user, Pass: pass}.AsSha256Mechanism()))
		default:
			opts = append(opts, kgo.SASL(scram.Auth{User: user, Pass: pass}.AsSha512Mechanism()))
		}
	case "iam":
		m, err := awsIAMMechanism(t)
		if err != nil {
			return nil, err
		}
		opts = append(opts, kgo.SASL(m))
	case "oauthbearer", "oauth":
		m, err := oauthMechanism(t)
		if err != nil {
			return nil, err
		}
		opts = append(opts, kgo.SASL(m))
	default:
		return nil, fmt.Errorf("unknown kafka auth mechanism %q", a.Mechanism)
	}
	return opts, nil
}

// NewKafka builds the admin client (connections are lazy).
func NewKafka(t config.Broker, log *slog.Logger) (*Kafka, error) {
	opts, err := KafkaOpts(t)
	if err != nil {
		return nil, err
	}
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, err
	}
	k := &Kafka{t: t, cl: cl, adm: kadm.NewClient(cl), log: log, c: rate.NewCounters(), rb: rate.NewRebalanceCounter(),
		prevSz: map[string]sizeReading{}, nodes: NewNodeMapper(), prom: newPromClient(t.Timeout.D(), t.Auth.InsecureSkipVerify),
		warned: map[string]bool{}}
	if t.TopicFilter != "" {
		k.topicF = regexp.MustCompile(t.TopicFilter)
	}
	if t.GroupFilter != "" {
		k.groupF = regexp.MustCompile(t.GroupFilter)
	}
	return k, nil
}

// Close closes the client.
func (k *Kafka) Close() error { k.cl.Close(); return nil }

func (k *Kafka) topicOK(t string) bool { return k.topicF == nil || k.topicF.MatchString(t) }
func (k *Kafka) groupOK(g string) bool { return k.groupF == nil || k.groupF.MatchString(g) }

// GenerationKey identifies a group generation by its sorted member ids.
func GenerationKey(memberIDs []string) string {
	s := slices.Clone(memberIDs)
	slices.Sort(s)
	return strings.Join(s, ",")
}

// Scrape implements Scraper.
func (k *Kafka) Scrape(ctx context.Context) (emit.Result, error) {
	var r emit.Result
	now := time.Now()
	cluster := k.t.Key()

	topics, err := k.adm.ListTopics(ctx)
	if err != nil {
		return r, fmt.Errorf("list topics: %w", err)
	}
	var names []string
	for _, t := range topics.Sorted() {
		if t.Err == nil && !t.IsInternal && k.topicOK(t.Topic) && !strings.HasPrefix(t.Topic, "_") {
			names = append(names, t.Topic)
		}
	}
	ends := kadm.ListedOffsets{}
	starts := kadm.ListedOffsets{}
	if len(names) > 0 {
		if ends, err = k.adm.ListEndOffsets(ctx, names...); err != nil && len(ends) == 0 {
			return r, fmt.Errorf("list end offsets: %w", err)
		}
		starts, _ = k.adm.ListStartOffsets(ctx, names...)
	}
	// produce rate per partition (independent of groups)
	produce := map[string]map[int32]float64{}
	ends.Each(func(o kadm.ListedOffset) {
		if o.Err != nil || o.Offset < 0 {
			return
		}
		if v, ok := k.c.Rate(fmt.Sprintf("end/%s/%d", o.Topic, o.Partition), float64(o.Offset), now); ok {
			if produce[o.Topic] == nil {
				produce[o.Topic] = map[int32]float64{}
			}
			produce[o.Topic][o.Partition] = v
		}
	})

	listed, err := k.adm.ListGroups(ctx)
	if err != nil && len(listed) == 0 {
		return r, fmt.Errorf("list groups: %w", err)
	}
	var groups []string
	for _, g := range listed.Sorted() {
		if (g.ProtocolType == "consumer" || g.ProtocolType == "") && k.groupOK(g.Group) {
			groups = append(groups, g.Group)
		}
	}
	read := map[string]bool{} // topics with at least one group
	if len(groups) > 0 {
		described, err := k.describeGroups(ctx, groups, now)
		if err != nil {
			return r, err
		}
		fetched := k.adm.FetchManyOffsets(ctx, groups...)
		for _, g := range described {
			reb := k.rb.Observe(g.Group, g.State, g.Generation, now)
			ga := emit.GroupAttrs(cluster, "kafka", g.Group, g.State)
			r.Gauge(emit.BrokerGroupMembers, float64(g.Members), ga...)
			r.Gauge(emit.BrokerGroupRebalances, float64(reb), ga...)

			// partitions to report: committed ∪ assigned
			parts := map[string]map[int32]int64{} // topic → partition → committed (-1 = none)
			if f, ok := fetched[g.Group]; ok && f.Err == nil {
				f.Fetched.Each(func(o kadm.OffsetResponse) {
					if o.Err == nil && k.topicOK(o.Topic) {
						if parts[o.Topic] == nil {
							parts[o.Topic] = map[int32]int64{}
						}
						parts[o.Topic][o.Partition] = o.At
					}
				})
			}
			g.Assigned.Each(func(t string, p int32) {
				if !k.topicOK(t) {
					return
				}
				if parts[t] == nil {
					parts[t] = map[int32]int64{}
				}
				if _, ok := parts[t][p]; !ok {
					parts[t][p] = -1
				}
			})
			for topic, ps := range parts {
				read[topic] = true
				for p, committed := range ps {
					end, ok := ends.Lookup(topic, p)
					if !ok || end.Err != nil || end.Offset < 0 {
						continue
					}
					la := emit.LagAttrs(cluster, "kafka", topic, g.Group, strconv.Itoa(int(p)))
					lag := LagFor(end.Offset, committed, startOf(starts, topic, p))
					r.Gauge(emit.BrokerLag, float64(lag), la...)
					r.Gauge(emit.BrokerEndOffset, float64(end.Offset), la...)
					if committed >= 0 {
						r.Gauge(emit.BrokerCommittedOffset, float64(committed), la...)
						if v, ok := k.c.Rate(fmt.Sprintf("commit/%s/%s/%d", g.Group, topic, p), float64(committed), now); ok {
							r.Gauge(emit.BrokerConsumeRate, v, la...)
						}
					}
					if v, ok := produce[topic][p]; ok {
						r.Gauge(emit.BrokerProduceRate, v, la...)
					}
				}
			}
		}
	}
	// topics without readers: offsets + produce rate under group ""
	for _, topic := range names {
		if read[topic] {
			continue
		}
		for p, end := range ends[topic] {
			if end.Err != nil || end.Offset < 0 {
				continue
			}
			la := emit.LagAttrs(cluster, "kafka", topic, "", strconv.Itoa(int(p)))
			r.Gauge(emit.BrokerEndOffset, float64(end.Offset), la...)
			if v, ok := produce[topic][p]; ok {
				r.Gauge(emit.BrokerProduceRate, v, la...)
			}
		}
	}
	// topic metadata and node metrics never fail the scrape (missing ACLs, old brokers)
	disks := k.topicMetadata(ctx, topics, names, ends, starts, produce, now, &r)
	k.nodeMetrics(ctx, disks, now, &r)
	k.c.Prune(now.Add(-10 * k.t.Interval.D()))
	k.rb.Prune(now.Add(-2 * time.Hour))
	return r, nil
}

func startOf(s kadm.ListedOffsets, topic string, p int32) int64 {
	if o, ok := s.Lookup(topic, p); ok && o.Err == nil && o.Offset >= 0 {
		return o.Offset
	}
	return 0
}

// LagFor returns end − committed; with no commit (committed < 0) the group would start from the
// log start (auto.offset.reset=earliest) or the end (latest) — we report end − start as the
// upper bound. Never negative.
func LagFor(end, committed, start int64) int64 {
	var lag int64
	if committed < 0 {
		lag = end - start
	} else {
		lag = end - committed
	}
	return max(lag, 0)
}
