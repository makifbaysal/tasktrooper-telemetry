package scrape

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/collector/internal/config"
	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
)

// RabbitMQ reads the management HTTP API with a `monitoring`-tagged user (no vhost rights).
//
//   - per queue (= topic, partition "0"): lag = messages_ready, unacked = messages_unacknowledged,
//     produce_rate = publish rate, consume_rate = deliver_get rate (msg/s, the plugin's own
//     5 s-sampled rates)
//   - group = consumer tag prefix (GroupFromConsumerTag); auto-generated tags (amq.ctag-…) and
//     queues whose consumers carry no usable tag use the queue name; a queue without consumers is
//     reported with group "" (no readers). Several groups on one queue share its ready count and
//     split the deliver rate by consumer count.
//   - group.members = consumers with that tag prefix; state Stable / Empty; no rebalances
//   - node memory = mem_used / mem_limit (the high watermark, i.e. "how close to blocking
//     publishers"); CPU, disk usage % and network are not exposed (disk_free has no total) and are
//     left to the cloud integration.
type RabbitMQ struct {
	t      config.Broker
	base   *url.URL
	hc     *http.Client
	user   string
	pass   string
	log    *slog.Logger
	topicF *regexp.Regexp
	groupF *regexp.Regexp

	nodes     *NodeMapper
	prom      *promClient
	promOffAt time.Time // auto-probe of :15692 failed at (retried after 10 min)
}

// NewRabbitMQ validates the endpoint and credentials.
func NewRabbitMQ(t config.Broker, log *slog.Logger) (*RabbitMQ, error) {
	u, err := url.Parse(strings.TrimRight(t.Endpoint, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("rabbitmq endpoint %q: want http(s)://host:15672", t.Endpoint)
	}
	user, pass, err := t.Credentials()
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if t.Auth.InsecureSkipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in
	}
	q := &RabbitMQ{t: t, base: u, hc: &http.Client{Timeout: t.Timeout.D(), Transport: tr}, user: user, pass: pass, log: log,
		nodes: NewNodeMapper(), prom: newPromClient(t.Timeout.D(), t.Auth.InsecureSkipVerify)}
	if t.TopicFilter != "" {
		q.topicF = regexp.MustCompile(t.TopicFilter)
	}
	if t.GroupFilter != "" {
		q.groupF = regexp.MustCompile(t.GroupFilter)
	}
	return q, nil
}

// Close implements Scraper.
func (q *RabbitMQ) Close() error { q.hc.CloseIdleConnections(); return nil }

type rmqRate struct {
	Rate float64 `json:"rate"`
}

type rmqQueue struct {
	Name         string   `json:"name"`
	Vhost        string   `json:"vhost"`
	Ready        int64    `json:"messages_ready"`
	Unacked      int64    `json:"messages_unacknowledged"`
	Consumers    int      `json:"consumers"`
	Messages     int64    `json:"messages"`
	MessageBytes int64    `json:"message_bytes"`
	Type         string   `json:"type"`
	Members      []string `json:"members"`
	// HeadTimestamp is the `timestamp` property (s since epoch) of the first ready message; only
	// classic queues report it and only when publishers set the property.
	HeadTimestamp   any            `json:"head_message_timestamp"`
	Arguments       map[string]any `json:"arguments"`
	EffectivePolicy map[string]any `json:"effective_policy_definition"`
	MessageStats    struct {
		Publish    rmqRate `json:"publish_details"`
		DeliverGet rmqRate `json:"deliver_get_details"`
	} `json:"message_stats"`
}

type rmqConsumer struct {
	Tag   string `json:"consumer_tag"`
	Queue struct {
		Name string `json:"name"`
	} `json:"queue"`
}

type rmqNode struct {
	Name          string  `json:"name"`
	Running       bool    `json:"running"`
	MemUsed       float64 `json:"mem_used"`
	MemLimit      float64 `json:"mem_limit"`
	FDUsed        float64 `json:"fd_used"`
	FDTotal       float64 `json:"fd_total"`
	ProcUsed      float64 `json:"proc_used"`
	ProcTotal     float64 `json:"proc_total"`
	DiskFree      float64 `json:"disk_free"`
	DiskFreeLimit float64 `json:"disk_free_limit"`
}

// numberOf reads a JSON number (float64) or numeric string.
func numberOf(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

// QueueRetentionMs returns the queue's message TTL: x-message-ttl argument, else the effective
// policy's message-ttl, else -1 (unlimited).
func QueueRetentionMs(args, policy map[string]any) float64 {
	if v, ok := numberOf(args["x-message-ttl"]); ok && v >= 0 {
		return v
	}
	if v, ok := numberOf(policy["message-ttl"]); ok && v >= 0 {
		return v
	}
	return -1
}

// OldestAgeSeconds is now − head_message_timestamp (never negative); ok=false when the queue
// does not report a head timestamp.
func OldestAgeSeconds(head any, now time.Time) (float64, bool) {
	ts, ok := numberOf(head)
	if !ok || ts <= 0 {
		return 0, false
	}
	if ts > 1e12 { // milliseconds
		ts /= 1000
	}
	return max(float64(now.UnixNano())/1e9-ts, 0), true
}

func (q *RabbitMQ) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(q.base.String(), "/")+path, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(q.user, q.pass)
	req.Header.Set("Accept", "application/json")
	resp, err := q.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("GET %s: %s %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out)
}

var (
	reUUIDSuffix = regexp.MustCompile(`[-_.:]?[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	reHex        = regexp.MustCompile(`^[0-9a-fA-F]+$`)
	reDigits     = regexp.MustCompile(`^[0-9]+$`)
)

func randomToken(s string) bool {
	if s == "" {
		return true
	}
	if reDigits.MatchString(s) {
		return true
	}
	digits := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	if reHex.MatchString(s) && len(s) >= 4 && digits > 0 {
		return true
	}
	return digits >= 2 // k8s pod suffixes (x2k9p), host-ish ids
}

// GroupFromConsumerTag maps a consumer tag to a "group": the tag without trailing per-instance
// tokens (uuid, numbers, hex ids, pod suffixes), split on - _ . :. Auto-generated tags
// (amq.ctag-…) and tags that are entirely random fall back to the queue name.
func GroupFromConsumerTag(tag, queue string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" || strings.HasPrefix(tag, "amq.ctag-") {
		return queue
	}
	tag = reUUIDSuffix.ReplaceAllString(tag, "")
	tokens := strings.FieldsFunc(tag, func(r rune) bool { return r == '-' || r == '_' || r == '.' || r == ':' })
	if len(tokens) == 0 {
		return queue
	}
	n := len(tokens)
	for n > 1 && randomToken(tokens[n-1]) {
		n--
	}
	if n == 1 && randomToken(tokens[0]) {
		return queue
	}
	// cut the original string after the n-th token to keep its separators
	end, seen := 0, 0
	for i := 0; i < len(tag) && seen < n; {
		j := i
		for j < len(tag) && !strings.ContainsRune("-_.:", rune(tag[j])) {
			j++
		}
		if j > i {
			seen++
		}
		end = j
		i = j + 1
	}
	return tag[:end]
}

// Scrape implements Scraper.
func (q *RabbitMQ) Scrape(ctx context.Context) (emit.Result, error) {
	var r emit.Result
	vh := url.PathEscape(q.t.Vhost)
	var queues []rmqQueue
	if err := q.get(ctx, "/api/queues/"+vh+"?disable_stats=false&enable_queue_totals=true", &queues); err != nil {
		return r, err
	}
	var consumers []rmqConsumer
	if err := q.get(ctx, "/api/consumers/"+vh, &consumers); err != nil {
		q.log.Debug("rabbitmq consumers unavailable; groups fall back to queue names", "target", q.t.Key(), "err", err)
	}
	byQueue := map[string]map[string]int{} // queue → group → consumers
	for _, c := range consumers {
		g := GroupFromConsumerTag(c.Tag, c.Queue.Name)
		if byQueue[c.Queue.Name] == nil {
			byQueue[c.Queue.Name] = map[string]int{}
		}
		byQueue[c.Queue.Name][g]++
	}
	cluster := q.t.Key()
	now := time.Now()
	members := map[string]int{}
	for _, qu := range queues {
		if q.topicF != nil && !q.topicF.MatchString(qu.Name) {
			continue
		}
		queueTopic(&r, cluster, qu)
		oldest, haveOldest := OldestAgeSeconds(qu.HeadTimestamp, now)
		groups := byQueue[qu.Name]
		if len(groups) == 0 && qu.Consumers > 0 { // consumers endpoint unavailable
			groups = map[string]int{qu.Name: qu.Consumers}
		}
		if len(groups) == 0 {
			groups = map[string]int{"": 0}
		}
		total := 0
		for _, n := range groups {
			total += n
		}
		names := make([]string, 0, len(groups))
		for g := range groups {
			names = append(names, g)
		}
		sort.Strings(names)
		for _, g := range names {
			if g != "" && q.groupF != nil && !q.groupF.MatchString(g) {
				continue
			}
			n := groups[g]
			share := 1.0
			if total > 0 {
				share = float64(n) / float64(total)
			}
			la := emit.LagAttrs(cluster, "rabbitmq", qu.Name, g, "0")
			r.Gauge(emit.BrokerLag, float64(qu.Ready), la...)
			r.Gauge(emit.BrokerUnacked, float64(qu.Unacked)*share, la...)
			r.Gauge(emit.BrokerProduceRate, qu.MessageStats.Publish.Rate, la...)
			if haveOldest {
				r.Gauge(emit.BrokerOldestAgeS, oldest, la...)
			}
			if g != "" {
				r.Gauge(emit.BrokerConsumeRate, qu.MessageStats.DeliverGet.Rate*share, la...)
				members[g] += n
			}
		}
	}
	groupNames := make([]string, 0, len(members))
	for g := range members {
		groupNames = append(groupNames, g)
	}
	sort.Strings(groupNames)
	for _, g := range groupNames {
		state := "Stable"
		if members[g] == 0 {
			state = "Empty"
		}
		ga := emit.GroupAttrs(cluster, "rabbitmq", g, state)
		r.Gauge(emit.BrokerGroupMembers, float64(members[g]), ga...)
	}

	q.nodeMetrics(ctx, now, &r)
	return r, nil
}

// queueTopic reports trooper.broker.topic.* for one queue: partitions 1, replication = quorum /
// stream members (classic: 1), retention = message TTL, size = message_bytes (ready + unacked
// bodies), avg = message_bytes / messages, in_rate = publish rate, bytes_in_rate = in_rate × avg.
func queueTopic(r *emit.Result, cluster string, qu rmqQueue) {
	ta := emit.TopicAttrs(cluster, "rabbitmq", qu.Name)
	r.Gauge(emit.BrokerTopicPartitions, 1, ta...)
	r.Gauge(emit.BrokerTopicReplication, float64(max(len(qu.Members), 1)), ta...)
	r.Gauge(emit.BrokerTopicRetentionMs, QueueRetentionMs(qu.Arguments, qu.EffectivePolicy), ta...)
	r.Gauge(emit.BrokerTopicSizeBytes, float64(qu.MessageBytes), ta...)
	r.Gauge(emit.BrokerTopicInRate, qu.MessageStats.Publish.Rate, ta...)
	if qu.Messages > 0 {
		avg := float64(qu.MessageBytes) / float64(qu.Messages)
		r.Gauge(emit.BrokerTopicAvgMsgBytes, avg, ta...)
		r.Gauge(emit.BrokerTopicBytesInRate, qu.MessageStats.Publish.Rate*avg, ta...)
	}
}

// nodeMetrics: /api/nodes gives memory (mem_used / high watermark), disk (disk_free_limit /
// disk_free: how close to the free-disk alarm, 1 = publishers blocked; the API has no volume
// total), file descriptors and Erlang processes (custom trooper.broker.node.fd / .proc). CPU and
// net_in (and better memory/disk when it is node exporter) come from Prometheus: nodes[].metricsUrl,
// or — nodeMetrics: auto and no nodes configured — the rabbitmq_prometheus plugin on the
// management host's port 15692 (that node only).
func (q *RabbitMQ) nodeMetrics(ctx context.Context, now time.Time, r *emit.Result) {
	cluster := q.t.Key()
	var nodes []rmqNode
	if err := q.get(ctx, "/api/nodes", &nodes); err != nil {
		q.log.Debug("rabbitmq nodes unavailable", "target", q.t.Key(), "err", err)
	}
	fromAPI := map[string]NodeValues{}
	for _, n := range nodes {
		if !n.Running {
			continue
		}
		v := NodeValues{}
		if f, ok := rateFraction(n.MemUsed, n.MemLimit); ok {
			v[emit.BrokerNodeMemory] = f
		}
		if n.DiskFree > 0 && n.DiskFreeLimit > 0 {
			v[emit.BrokerNodeDisk] = min(n.DiskFreeLimit/n.DiskFree, 1)
		}
		if f, ok := rateFraction(n.FDUsed, n.FDTotal); ok {
			v[emit.BrokerNodeFD] = f
		}
		if f, ok := rateFraction(n.ProcUsed, n.ProcTotal); ok {
			v[emit.BrokerNodeProc] = f
		}
		fromAPI[n.Name] = v
	}
	// Prometheus values override / extend the API's
	for name, pv := range q.promNodes(ctx, now) {
		if fromAPI[name] == nil {
			fromAPI[name] = NodeValues{}
		}
		for k, v := range pv {
			fromAPI[name][k] = v
		}
	}
	names := make([]string, 0, len(fromAPI))
	for n := range fromAPI {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		v := fromAPI[name]
		emitNode(r, cluster, "rabbitmq", name, v)
		for _, m := range []string{emit.BrokerNodeFD, emit.BrokerNodeProc} {
			if x, ok := v[m]; ok {
				r.Gauge(m, x, emit.NodeAttrs(cluster, "rabbitmq", name)...)
			}
		}
	}
}

func rateFraction(used, total float64) (float64, bool) {
	if total <= 0 {
		return 0, false
	}
	return min(max(used/total, 0), 1), true
}

// promNodes returns node values per node name from the Prometheus endpoints.
func (q *RabbitMQ) promNodes(ctx context.Context, now time.Time) map[string]NodeValues {
	type ep struct{ name, url, mount string }
	var eps []ep
	for _, n := range q.t.Nodes {
		if n.MetricsURL != "" {
			eps = append(eps, ep{n.Name, n.MetricsURL, n.DiskMount})
		}
	}
	auto := false
	if len(eps) == 0 && q.t.NodeMetrics == "auto" && (q.promOffAt.IsZero() || now.Sub(q.promOffAt) > 10*time.Minute) {
		u := *q.base
		u.Host = u.Hostname() + ":15692"
		u.Path = "/metrics"
		u.User = nil
		u.RawQuery = ""
		eps = append(eps, ep{"", u.String(), ""})
		auto = true
	}
	out := map[string]NodeValues{}
	for _, e := range eps {
		samples, err := q.prom.fetch(ctx, e.url)
		if err != nil {
			if auto {
				q.promOffAt = now
			}
			q.log.Debug("rabbitmq prometheus endpoint unavailable", "target", q.t.Key(), "url", e.url, "err", err)
			continue
		}
		name := e.name
		if name == "" {
			for _, s := range samples["rabbitmq_identity_info"] {
				if s.Labels["rabbitmq_node"] != "" {
					name = s.Labels["rabbitmq_node"]
					break
				}
			}
		}
		if name == "" {
			name = hostOf(e.url)
		}
		out[name] = q.nodes.Map(name, samples, e.mount, now)
	}
	return out
}
