// Package gcp discovers Cloud SQL, Memorystore Redis and Pub/Sub subscriptions over the Google REST APIs and
// reads their metrics from Cloud Monitoring timeSeries.list (contracts §5, §10; mapping table in README.md).
package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/tasktrooper/telemetry/services/integrations/internal/httpx"
	"github.com/tasktrooper/telemetry/services/integrations/internal/model"
)

// Config is integrations.config for provider gcp.
type Config struct {
	ProjectIDs []string `json:"projectIds"`
	Services   []string `json:"services"` // cloudsql | memorystore | pubsub (empty = all)
	Env        string   `json:"env"`
	// ConsumerMap maps a subscription to the consumer group name shown in the UI (default: subscription name).
	ConsumerMap map[string]string `json:"consumerMap"`
}

// Endpoints override the API bases (tests).
type Endpoints struct {
	SQLAdmin, Redis, PubSub, Monitoring string
	// NoAuth disables OAuth2 (tests).
	NoAuth bool
}

// Provider implements model.Provider.
type Provider struct {
	cfg                     Config
	sql, redis, pubsub, mon *httpx.Client
}

const scope = "https://www.googleapis.com/auth/cloud-platform"

// New builds the provider. creds is the service-account JSON (empty = Application Default Credentials).
func New(ctx context.Context, raw json.RawMessage, creds []byte, ep Endpoints) (*Provider, error) {
	var cfg Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("gcp config: %w", err)
		}
	}
	var auth func(*http.Request) error
	if !ep.NoAuth {
		var c *google.Credentials
		var err error
		if len(creds) > 0 {
			c, err = google.CredentialsFromJSON(ctx, creds, scope) //nolint:staticcheck // service account JSON entered by the user
		} else {
			c, err = google.FindDefaultCredentials(ctx, scope)
		}
		if err != nil {
			return nil, fmt.Errorf("gcp credentials: %w", err)
		}
		if len(cfg.ProjectIDs) == 0 && c.ProjectID != "" {
			cfg.ProjectIDs = []string{c.ProjectID}
		}
		ts := oauth2.ReuseTokenSource(nil, c.TokenSource)
		auth = func(r *http.Request) error {
			t, err := ts.Token()
			if err != nil {
				return errors.New("token: " + strings.SplitN(err.Error(), "\n", 2)[0])
			}
			t.SetAuthHeader(r)
			return nil
		}
	}
	if len(cfg.ProjectIDs) == 0 {
		return nil, errors.New("gcp: config.projectIds is empty")
	}
	def := func(v, d string) string {
		if v == "" {
			return d
		}
		return strings.TrimRight(v, "/")
	}
	mk := func(base string) *httpx.Client { return &httpx.Client{Base: base, Auth: auth} }
	return &Provider{
		cfg:    cfg,
		sql:    mk(def(ep.SQLAdmin, "https://sqladmin.googleapis.com/v1")),
		redis:  mk(def(ep.Redis, "https://redis.googleapis.com/v1")),
		pubsub: mk(def(ep.PubSub, "https://pubsub.googleapis.com/v1")),
		mon:    mk(def(ep.Monitoring, "https://monitoring.googleapis.com/v3")),
	}, nil
}

func (p *Provider) enabled(s string) bool {
	return len(p.cfg.Services) == 0 || slices.Contains(p.cfg.Services, s)
}

// API shapes (subset).
type sqlInstance struct {
	Name            string `json:"name"`
	Project         string `json:"project"`
	DatabaseVersion string `json:"databaseVersion"`
	Region          string `json:"region"`
	GceZone         string `json:"gceZone"`
	State           string `json:"state"`
	InstanceType    string `json:"instanceType"`
	ConnectionName  string `json:"connectionName"`
	MasterInstance  string `json:"masterInstanceName"`
	IPAddresses     []struct {
		IPAddress string `json:"ipAddress"`
		Type      string `json:"type"`
	} `json:"ipAddresses"`
	Settings struct {
		Tier             string `json:"tier"`
		DataDiskSizeGb   string `json:"dataDiskSizeGb"`
		AvailabilityType string `json:"availabilityType"`
	} `json:"settings"`
}

type redisInstance struct {
	Name         string  `json:"name"` // projects/p/locations/r/instances/x
	Host         string  `json:"host"`
	Port         int     `json:"port"`
	Tier         string  `json:"tier"`
	MemorySizeGb float64 `json:"memorySizeGb"`
	RedisVersion string  `json:"redisVersion"`
	LocationID   string  `json:"locationId"`
	State        string  `json:"state"`
	ReplicaCount int     `json:"replicaCount"`
	project      string
}

type subscription struct {
	Name               string `json:"name"`  // projects/p/subscriptions/s
	Topic              string `json:"topic"` // projects/p/topics/t
	AckDeadlineSeconds int    `json:"ackDeadlineSeconds"`
	Retention          string `json:"messageRetentionDuration"`
}

type projectState struct {
	id     string
	sql    []sqlInstance
	redis  []redisInstance
	subs   []subscription
	pubsub bool
}

type state struct{ projects []*projectState }

func last(path string) string { return path[strings.LastIndex(path, "/")+1:] }

// regionOf turns "europe-west1-b" into "europe-west1"; regions pass through.
func regionOf(loc string) string {
	parts := strings.Split(loc, "-")
	if len(parts) == 3 && len(parts[2]) == 1 {
		return parts[0] + "-" + parts[1]
	}
	return loc
}

func paginate[T any](ctx context.Context, c *httpx.Client, path, field string, q url.Values) ([]T, error) {
	var out []T
	if q == nil {
		q = url.Values{}
	}
	for page := 0; page < 100; page++ {
		var raw map[string]json.RawMessage
		if err := c.Get(ctx, path, q, &raw); err != nil {
			return out, err
		}
		var items []T
		if b, ok := raw[field]; ok {
			if err := json.Unmarshal(b, &items); err != nil {
				return out, err
			}
		}
		out = append(out, items...)
		var next string
		if b, ok := raw["nextPageToken"]; ok {
			_ = json.Unmarshal(b, &next)
		}
		if next == "" {
			break
		}
		q.Set("pageToken", next)
	}
	return out, nil
}

// Discover implements model.Provider.
func (p *Provider) Discover(ctx context.Context) (*model.Inventory, error) {
	st := &state{}
	inv := &model.Inventory{State: st}
	var errs error
	multi := len(p.cfg.ProjectIDs) > 1
	for _, proj := range p.cfg.ProjectIDs {
		ps := &projectState{id: proj}
		pe := url.PathEscape(proj)
		if p.enabled("cloudsql") {
			items, err := paginate[sqlInstance](ctx, p.sql, "/projects/"+pe+"/instances", "items", nil)
			if err != nil {
				errs = errors.Join(errs, fmt.Errorf("cloudsql %s: %w", proj, err))
			}
			for _, in := range items {
				in.Project = proj
				if engineOf(in.DatabaseVersion) == "" {
					continue
				}
				ps.sql = append(ps.sql, in)
				inv.Databases = append(inv.Databases, p.sqlDatabase(in, multi))
			}
		}
		if p.enabled("memorystore") {
			items, err := paginate[redisInstance](ctx, p.redis, "/projects/"+pe+"/locations/-/instances", "instances", nil)
			if err != nil {
				errs = errors.Join(errs, fmt.Errorf("memorystore %s: %w", proj, err))
			}
			for _, in := range items {
				in.project = proj
				ps.redis = append(ps.redis, in)
				inv.Databases = append(inv.Databases, p.redisDatabase(in, multi))
			}
		}
		if p.enabled("pubsub") {
			subs, err := paginate[subscription](ctx, p.pubsub, "/projects/"+pe+"/subscriptions", "subscriptions", url.Values{"pageSize": {"1000"}})
			if err != nil {
				errs = errors.Join(errs, fmt.Errorf("pubsub %s: %w", proj, err))
			}
			if err == nil || len(subs) > 0 {
				ps.subs, ps.pubsub = subs, true
				inv.Brokers = append(inv.Brokers, pubsubBroker(proj, subs))
			}
		}
		st.projects = append(st.projects, ps)
	}
	if len(inv.Databases)+len(inv.Brokers) == 0 && errs != nil {
		return nil, errs
	}
	return inv, errs
}

func engineOf(v string) string {
	switch {
	case strings.HasPrefix(v, "POSTGRES"):
		return "postgres"
	case strings.HasPrefix(v, "MYSQL"):
		return "mysql"
	}
	return "" // SQL Server is not a supported engine
}

func sqlName(in sqlInstance, multi bool) string {
	if multi {
		return in.Project + "-" + in.Name
	}
	return in.Name
}

func (p *Provider) sqlDatabase(in sqlInstance, multi bool) model.Database {
	engine := engineOf(in.DatabaseVersion)
	port := 5432
	if engine == "mysql" {
		port = 3306
	}
	host := in.ConnectionName
	for _, t := range []string{"PRIMARY", "PRIVATE"} {
		for _, ip := range in.IPAddresses {
			if ip.Type == t && host == in.ConnectionName {
				host = ip.IPAddress
			}
		}
	}
	d := model.Database{
		Name: sqlName(in, multi), Engine: engine, Host: host, Port: port,
		Source: "Cloud SQL · " + in.Region, Version: in.DatabaseVersion, Zone: in.GceZone, InstanceClass: in.Settings.Tier,
		CloudResourceID: in.Project + ":" + in.Name, Env: p.cfg.Env,
		Meta: map[string]any{
			"project": in.Project, "connectionName": in.ConnectionName, "state": in.State, "instanceType": in.InstanceType,
			"availabilityType": in.Settings.AvailabilityType, "region": in.Region,
		},
	}
	if gb, err := strconv.ParseFloat(in.Settings.DataDiskSizeGb, 64); err == nil {
		d.DiskGB = &gb
	}
	return d
}

func redisName(in redisInstance, multi bool) string {
	if multi {
		return in.project + "-" + last(in.Name)
	}
	return last(in.Name)
}

func redisRegion(in redisInstance) string {
	parts := strings.Split(in.Name, "/")
	if len(parts) >= 4 {
		return parts[3]
	}
	return regionOf(in.LocationID)
}

func (p *Provider) redisDatabase(in redisInstance, multi bool) model.Database {
	return model.Database{
		Name: redisName(in, multi), Engine: "redis", Host: in.Host, Port: in.Port,
		Source: "Memorystore · " + redisRegion(in), Version: in.RedisVersion, Zone: in.LocationID,
		InstanceClass:   strings.TrimSpace(in.Tier + " " + strconv.FormatFloat(in.MemorySizeGb, 'f', -1, 64) + " GB"),
		CloudResourceID: in.Name, Env: p.cfg.Env,
		Meta: map[string]any{"project": in.project, "state": in.State, "replicaCount": in.ReplicaCount, "memorySizeGb": in.MemorySizeGb},
	}
}

// PubSubName is the broker name of a project's Pub/Sub ("pubsub-<project>" → slug gcp-pubsub-<project>).
func PubSubName(project string) string { return "pubsub-" + project }

func pubsubBroker(project string, subs []subscription) model.Broker {
	list := make([]map[string]any, 0, min(len(subs), 500))
	for i, s := range subs {
		if i == 500 {
			break
		}
		list = append(list, map[string]any{"name": last(s.Name), "topic": last(s.Topic), "ackDeadlineSeconds": s.AckDeadlineSeconds, "retention": s.Retention})
	}
	return model.Broker{
		Name: PubSubName(project), Kind: "pubsub", Endpoint: "pubsub.googleapis.com/projects/" + project,
		Source: "Google Pub/Sub · " + project,
		Meta:   map[string]any{"project": project, "subscriptions": list, "subscriptionCount": len(subs)},
	}
}

// ── metrics ──

// Point is the newest aligned value of one series.
type Point struct {
	Value float64
	TS    time.Time
}

type query struct {
	metric  string // full metric type
	aligner string // ALIGN_MEAN | ALIGN_RATE | ALIGN_DELTA
	// groupBy, when set, reduces with REDUCE_SUM grouped by this label (e.g. resource.label.database_id).
	groupBy string
	key     string // resource label holding the series key
}

type timeSeries struct {
	Resource struct {
		Labels map[string]string `json:"labels"`
	} `json:"resource"`
	Metric struct {
		Labels map[string]string `json:"labels"`
	} `json:"metric"`
	Points []struct {
		Interval struct {
			EndTime time.Time `json:"endTime"`
		} `json:"interval"`
		Value struct {
			DoubleValue *float64 `json:"doubleValue"`
			Int64Value  *string  `json:"int64Value"`
		} `json:"value"`
	} `json:"points"`
}

// fetch runs one timeSeries.list over [end-5m, end] and returns the newest point per key (max over series sharing a key).
func (p *Provider) fetch(ctx context.Context, project string, q query, end time.Time) (map[string]Point, error) {
	v := url.Values{
		"filter":                       {`metric.type = "` + q.metric + `"`},
		"interval.startTime":           {end.Add(-5 * time.Minute).UTC().Format(time.RFC3339)},
		"interval.endTime":             {end.UTC().Format(time.RFC3339)},
		"aggregation.alignmentPeriod":  {"60s"},
		"aggregation.perSeriesAligner": {q.aligner},
		"view":                         {"FULL"},
	}
	if q.groupBy != "" {
		v.Set("aggregation.crossSeriesReducer", "REDUCE_SUM")
		v.Set("aggregation.groupByFields", q.groupBy)
	}
	series, err := paginate[timeSeries](ctx, p.mon, "/projects/"+url.PathEscape(project)+"/timeSeries", "timeSeries", v)
	return newest(series, q.key), err
}

func newest(series []timeSeries, key string) map[string]Point {
	out := map[string]Point{}
	for _, s := range series {
		k := s.Resource.Labels[key]
		if k == "" || len(s.Points) == 0 {
			continue
		}
		pt := s.Points[0] // newest first
		var val float64
		switch {
		case pt.Value.DoubleValue != nil:
			val = *pt.Value.DoubleValue
		case pt.Value.Int64Value != nil:
			val, _ = strconv.ParseFloat(*pt.Value.Int64Value, 64)
		default:
			continue
		}
		if old, ok := out[k]; !ok || val > old.Value {
			out[k] = Point{val, pt.Interval.EndTime}
		}
	}
	return out
}

const (
	sqlKey   = "database_id"     // "<project>:<instance>"
	redisKey = "instance_id"     // "projects/p/locations/r/instances/x"
	subKey   = "subscription_id" // short name
	topicKey = "topic_id"
)

var sqlQueries = map[string]query{
	"cpu":      {"cloudsql.googleapis.com/database/cpu/utilization", "ALIGN_MEAN", "", sqlKey},
	"memory":   {"cloudsql.googleapis.com/database/memory/utilization", "ALIGN_MEAN", "", sqlKey},
	"disk":     {"cloudsql.googleapis.com/database/disk/utilization", "ALIGN_MEAN", "", sqlKey},
	"pg_conn":  {"cloudsql.googleapis.com/database/postgresql/num_backends", "ALIGN_MEAN", "resource.label.database_id", sqlKey},
	"my_conn":  {"cloudsql.googleapis.com/database/network/connections", "ALIGN_MEAN", "", sqlKey},
	"repl_lag": {"cloudsql.googleapis.com/database/replication/replica_lag", "ALIGN_MEAN", "", sqlKey},
	"pg_ops":   {"cloudsql.googleapis.com/database/postgresql/transaction_count", "ALIGN_RATE", "resource.label.database_id", sqlKey},
	"my_ops":   {"cloudsql.googleapis.com/database/mysql/queries", "ALIGN_RATE", "", sqlKey},
}

var redisQueries = map[string]query{
	"cpu":       {"redis.googleapis.com/stats/cpu_utilization", "ALIGN_MEAN", "resource.label.instance_id", redisKey},
	"memory":    {"redis.googleapis.com/stats/memory/usage_ratio", "ALIGN_MEAN", "", redisKey},
	"clients":   {"redis.googleapis.com/clients/connected", "ALIGN_MEAN", "", redisKey},
	"evictions": {"redis.googleapis.com/stats/evicted_keys", "ALIGN_DELTA", "", redisKey},
	"hit_ratio": {"redis.googleapis.com/stats/cache_hit_ratio", "ALIGN_MEAN", "", redisKey},
}

var pubsubQueries = map[string]query{
	"undelivered": {"pubsub.googleapis.com/subscription/num_undelivered_messages", "ALIGN_MEAN", "", subKey},
	"oldest":      {"pubsub.googleapis.com/subscription/oldest_unacked_message_age", "ALIGN_MEAN", "", subKey},
	"ack_rate":    {"pubsub.googleapis.com/subscription/ack_message_count", "ALIGN_RATE", "", subKey},
	"send_rate":   {"pubsub.googleapis.com/topic/send_message_operation_count", "ALIGN_RATE", "", topicKey},
}

// Values are fetched results: query name → series key → point.
type Values map[string]map[string]Point

func (p *Provider) fetchAll(ctx context.Context, project string, qs map[string]query, end time.Time) (Values, error) {
	out := Values{}
	var errs error
	for name, q := range qs {
		v, err := p.fetch(ctx, project, q, end)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("%s: %w", q.metric, err))
		}
		out[name] = v
	}
	return out, errs
}

// Collect implements model.Provider.
func (p *Provider) Collect(ctx context.Context, inv *model.Inventory, now time.Time) (*model.Result, error) {
	st, _ := inv.State.(*state)
	res := &model.Result{}
	if st == nil {
		return res, nil
	}
	end := now.Truncate(time.Minute)
	multi := len(p.cfg.ProjectIDs) > 1
	var errs error
	for _, ps := range st.projects {
		if len(ps.sql) > 0 {
			v, err := p.fetchAll(ctx, ps.id, sqlQueries, end)
			errs = errors.Join(errs, err)
			res.Samples = append(res.Samples, MapCloudSQL(ps.sql, v, multi)...)
		}
		if len(ps.redis) > 0 {
			v, err := p.fetchAll(ctx, ps.id, redisQueries, end)
			errs = errors.Join(errs, err)
			res.Samples = append(res.Samples, MapRedis(ps.redis, v, multi)...)
		}
		if ps.pubsub && len(ps.subs) > 0 {
			v, err := p.fetchAll(ctx, ps.id, pubsubQueries, end)
			errs = errors.Join(errs, err)
			res.Samples = append(res.Samples, MapPubSub(ps.id, ps.subs, v, p.cfg.ConsumerMap)...)
		}
	}
	return res, errs
}

type emitter struct {
	out []model.Sample
}

func (e *emitter) add(name string, pt Point, ok bool, scale float64, region string, attrs map[string]string) {
	if !ok {
		return
	}
	e.out = append(e.out, model.Sample{Name: name, Value: pt.Value * scale, TS: pt.TS.Truncate(time.Minute), Region: region, Attrs: attrs})
}

func get(v Values, q, k string) (Point, bool) {
	pt, ok := v[q][k]
	return pt, ok
}

// MapCloudSQL maps Cloud SQL series to trooper.db.*.
func MapCloudSQL(ins []sqlInstance, v Values, multi bool) []model.Sample {
	e := &emitter{}
	for _, in := range ins {
		k := in.Project + ":" + in.Name
		name := sqlName(in, multi)
		engine := engineOf(in.DatabaseVersion)
		role := "primary"
		if in.InstanceType == "READ_REPLICA_INSTANCE" || in.MasterInstance != "" {
			role = "replica"
		}
		a := model.DBAttrs(model.Slug("gcp", name), name, model.DBSystem(engine), in.Name, role)
		pt, ok := get(v, "cpu", k)
		e.add(model.DBCPU, pt, ok, 1, in.Region, a)
		pt, ok = get(v, "memory", k)
		e.add(model.DBMemory, pt, ok, 1, in.Region, a)
		pt, ok = get(v, "disk", k)
		e.add(model.DBDisk, pt, ok, 1, in.Region, a)
		connQ, opsQ := "pg_conn", "pg_ops"
		if engine == "mysql" {
			connQ, opsQ = "my_conn", "my_ops"
		}
		pt, ok = get(v, connQ, k)
		e.add(model.DBConnections, pt, ok, 1, in.Region, a)
		pt, ok = get(v, opsQ, k)
		e.add(model.DBOps, pt, ok, 1, in.Region, a)
		pt, ok = get(v, "repl_lag", k)
		e.add(model.DBReplLag, pt, ok, 1000, in.Region, a) // seconds → ms
	}
	return e.out
}

// MapRedis maps Memorystore series to trooper.db.*.
func MapRedis(ins []redisInstance, v Values, multi bool) []model.Sample {
	e := &emitter{}
	for _, in := range ins {
		k := in.Name
		name := redisName(in, multi)
		region := redisRegion(in)
		a := model.DBAttrs(model.Slug("gcp", name), name, "redis", last(in.Name), "primary")
		if pt, ok := get(v, "cpu", k); ok {
			pt.Value = min(pt.Value, 1) // CPU-seconds per second (one vCPU = 1)
			e.add(model.DBCPU, pt, true, 1, region, a)
		}
		pt, ok := get(v, "memory", k)
		e.add(model.DBMemory, pt, ok, 1, region, a)
		pt, ok = get(v, "clients", k)
		e.add(model.DBConnections, pt, ok, 1, region, a)
		pt, ok = get(v, "evictions", k)
		e.add(model.DBEvictions, pt, ok, 1, region, a) // delta over 60 s = per minute
		pt, ok = get(v, "hit_ratio", k)
		e.add(model.DBHitRatio, pt, ok, 1, region, a)
	}
	return e.out
}

// MapPubSub maps subscription series to trooper.broker.* (partition "0", group = subscription or ConsumerMap).
func MapPubSub(project string, subs []subscription, v Values, consumerMap map[string]string) []model.Sample {
	e := &emitter{}
	cluster := model.Slug("gcp", PubSubName(project))
	seenTopic := map[string]bool{}
	for _, s := range subs {
		sub, topic := last(s.Name), last(s.Topic)
		group := sub
		if g := consumerMap[sub]; g != "" {
			group = g
		}
		a := model.LagAttrs(cluster, "gcp_pubsub", topic, group, "0")
		pt, ok := get(v, "undelivered", sub)
		e.add(model.BrokerLag, pt, ok, 1, "global", a)
		pt, ok = get(v, "oldest", sub)
		e.add(model.BrokerOldestAge, pt, ok, 1, "global", a)
		pt, ok = get(v, "ack_rate", sub)
		e.add(model.BrokerConsumeRate, pt, ok, 1, "global", a)
		pt, ok = get(v, "send_rate", topic)
		e.add(model.BrokerProduceRate, pt, ok, 1, "global", a)
		if ok && !seenTopic[topic] {
			seenTopic[topic] = true
			e.add(model.TopicInRate, pt, true, 1, "global", model.TopicAttrs(cluster, "gcp_pubsub", topic))
		}
	}
	return e.out
}
