package aws

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/tasktrooper/telemetry/services/integrations/internal/model"
)

// Point is the newest value of one CloudWatch series.
type Point struct {
	Value float64
	TS    time.Time
}

// Values are fetched results: resource key → CloudWatch metric name → newest point.
type Values map[string]map[string]Point

func (v Values) get(key, metric string) (Point, bool) {
	p, ok := v[key][metric]
	return p, ok
}

type cwQuery struct {
	key, ns, metric, stat string
	// as is the result label (default metric) — needed when one metric is read with several stats.
	as   string
	dims [][2]string
}

func q(key, ns, metric, stat string, dims ...[2]string) cwQuery {
	return cwQuery{key: key, ns: ns, metric: metric, stat: stat, dims: dims}
}

// Resource keys in Values.
func rdsKey(id string) string      { return "rds/" + id }
func cacheKey(node string) string  { return "ec/" + node }
func brokerKey(c, b string) string { return "msk/" + c + "/" + b }
func lagKey(c, g, t string) string { return "msklag/" + c + "/" + g + "/" + t }
func queueKey(name string) string  { return "sqs/" + name }

func plan(rs *regionState) []cwQuery {
	var qs []cwQuery
	for _, db := range rs.rds {
		d := [2]string{"DBInstanceIdentifier", db.ID}
		k := rdsKey(db.ID)
		for _, m := range []string{"CPUUtilization", "FreeableMemory", "DatabaseConnections", "ReadIOPS", "WriteIOPS"} {
			qs = append(qs, q(k, "AWS/RDS", m, "Average", d))
		}
		if db.Aurora {
			qs = append(qs, q(k, "AWS/RDS", "AuroraReplicaLag", "Average", d))
		} else {
			qs = append(qs, q(k, "AWS/RDS", "FreeStorageSpace", "Average", d), q(k, "AWS/RDS", "ReplicaLag", "Average", d))
		}
	}
	for _, g := range rs.caches {
		for _, n := range g.Nodes {
			d := [][2]string{{"CacheClusterId", n.ClusterID}, {"CacheNodeId", "0001"}}
			k := cacheKey(n.ClusterID)
			for _, m := range []string{"EngineCPUUtilization", "DatabaseMemoryUsagePercentage", "CurrConnections", "CacheHitRate"} {
				qs = append(qs, q(k, "AWS/ElastiCache", m, "Average", d...))
			}
			for _, m := range []string{"Evictions", "GetTypeCmds", "SetTypeCmds"} {
				qs = append(qs, q(k, "AWS/ElastiCache", m, "Sum", d...))
			}
		}
	}
	for _, c := range rs.msk {
		for _, b := range c.Brokers {
			d := [][2]string{{"Cluster Name", c.Name}, {"Broker ID", b}}
			for _, m := range []string{"CpuUser", "CpuSystem", "MemoryUsed", "MemoryFree", "KafkaDataLogsDiskUsed", "BytesInPerSec"} {
				qs = append(qs, q(brokerKey(c.Name, b), "AWS/Kafka", m, "Average", d...))
			}
		}
		for _, l := range c.Lags {
			d := [][2]string{{"Cluster Name", c.Name}, {"Consumer Group", l[0]}, {"Topic", l[1]}}
			qs = append(qs, q(lagKey(c.Name, l[0], l[1]), "AWS/Kafka", "SumOffsetLag", "Maximum", d...))
		}
	}
	for _, qu := range rs.queues {
		d := [2]string{"QueueName", qu.Name}
		k := queueKey(qu.Name)
		for _, m := range []string{"ApproximateNumberOfMessagesVisible", "ApproximateNumberOfMessagesNotVisible", "ApproximateAgeOfOldestMessage"} {
			qs = append(qs, q(k, "AWS/SQS", m, "Maximum", d))
		}
		qs = append(qs, q(k, "AWS/SQS", "NumberOfMessagesSent", "Sum", d), q(k, "AWS/SQS", "NumberOfMessagesDeleted", "Sum", d))
	}
	return qs
}

// Series are all datapoints of the window: resource key → label → points, newest first.
type Series map[string]map[string][]Point

// newest keeps the newest datapoint of every series.
func (s Series) newest() Values {
	out := Values{}
	for k, m := range s {
		for label, pts := range m {
			if len(pts) == 0 {
				continue
			}
			if out[k] == nil {
				out[k] = map[string]Point{}
			}
			out[k][label] = pts[0]
		}
	}
	return out
}

// fetch runs GetMetricData and keeps the newest datapoint of every series (see fetchSeries).
func fetch(ctx context.Context, cw *cloudwatch.Client, qs []cwQuery, end time.Time) (Values, error) {
	s, err := fetchSeries(ctx, cw, qs, end)
	return s.newest(), err
}

// fetchSeries runs GetMetricData in batches of ≤500 queries over [end-10m, end) with 60 s periods and keeps every
// datapoint, newest first (end = start of the current minute → the last complete minute).
func fetchSeries(ctx context.Context, cw *cloudwatch.Client, qs []cwQuery, end time.Time) (Series, error) {
	out := Series{}
	var errs error
	for i := 0; i < len(qs); i += 500 {
		batch := qs[i:min(i+500, len(qs))]
		in := &cloudwatch.GetMetricDataInput{
			StartTime: sdk.Time(end.Add(-10 * time.Minute)), EndTime: sdk.Time(end), ScanBy: cwtypes.ScanByTimestampDescending,
		}
		for j, x := range batch {
			dims := make([]cwtypes.Dimension, 0, len(x.dims))
			for _, d := range x.dims {
				dims = append(dims, cwtypes.Dimension{Name: sdk.String(d[0]), Value: sdk.String(d[1])})
			}
			in.MetricDataQueries = append(in.MetricDataQueries, cwtypes.MetricDataQuery{
				Id: sdk.String("q" + strconv.Itoa(j)),
				MetricStat: &cwtypes.MetricStat{
					Metric: &cwtypes.Metric{Namespace: sdk.String(x.ns), MetricName: sdk.String(x.metric), Dimensions: dims},
					Period: sdk.Int32(60), Stat: sdk.String(x.stat),
				},
				ReturnData: sdk.Bool(true),
			})
		}
		pg := cloudwatch.NewGetMetricDataPaginator(cw, in)
		for pg.HasMorePages() {
			res, err := pg.NextPage(ctx)
			if err != nil {
				errs = errors.Join(errs, err)
				break
			}
			for _, r := range res.MetricDataResults {
				j, err := strconv.Atoi(strings.TrimPrefix(sdk.ToString(r.Id), "q"))
				if err != nil || j >= len(batch) || len(r.Values) == 0 || len(r.Timestamps) == 0 {
					continue
				}
				x := batch[j]
				m := out[x.key]
				if m == nil {
					m = map[string][]Point{}
					out[x.key] = m
				}
				label := x.metric
				if x.as != "" {
					label = x.as
				}
				// Timestamps are descending; a later page only has older points for the same id.
				for k := range min(len(r.Values), len(r.Timestamps)) {
					m[label] = append(m[label], Point{Value: r.Values[k], TS: r.Timestamps[k]})
				}
			}
		}
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
	var errs error
	for i, rs := range st.regions {
		qs := append(plan(rs), lambdaQueries(rs)...)
		if len(qs) == 0 {
			continue
		}
		s, err := fetchSeries(ctx, p.regions[i].cw, qs, end)
		errs = errors.Join(errs, wrap(rs.region, "cloudwatch:GetMetricData", err))
		res.Samples = append(res.Samples, Map(rs, s.newest(), p.cfg)...)
		res.Samples = append(res.Samples, MapLambda(rs, s)...)
	}
	return res, errs
}

type emitter struct {
	region string
	out    []model.Sample
}

func (e *emitter) add(name string, v float64, ts time.Time, attrs map[string]string) {
	e.out = append(e.out, model.Sample{Name: name, Value: v, TS: ts.UTC().Truncate(time.Minute), Region: e.region, Attrs: attrs})
}

// one emits name = metric·scale when present.
func (e *emitter) one(v Values, key, metric, name string, scale float64, attrs map[string]string) {
	if pt, ok := v.get(key, metric); ok {
		e.add(name, pt.Value*scale, pt.TS, attrs)
	}
}

// Map turns CloudWatch values into trooper samples for one region (exported for tests).
func Map(rs *regionState, v Values, cfg Config) []model.Sample {
	e := &emitter{region: rs.region}
	for _, db := range rs.rds {
		k := rdsKey(db.ID)
		a := model.DBAttrs(model.Slug("aws", db.ID), db.ID, model.DBSystem(db.Engine), db.ID, db.Role)
		e.one(v, k, "CPUUtilization", model.DBCPU, 0.01, a)
		if pt, ok := v.get(k, "FreeableMemory"); ok {
			if gb := InstanceMemoryGB(db.Class, cfg.InstanceMemoryGB); gb > 0 {
				e.add(model.DBMemory, clamp01(1-pt.Value/(gb*(1<<30))), pt.TS, a)
			}
		}
		if pt, ok := v.get(k, "FreeStorageSpace"); ok && db.AllocatedGB > 0 {
			e.add(model.DBDisk, clamp01(1-pt.Value/(db.AllocatedGB*(1<<30))), pt.TS, a)
		}
		e.one(v, k, "DatabaseConnections", model.DBConnections, 1, a)
		r, rok := v.get(k, "ReadIOPS")
		w, wok := v.get(k, "WriteIOPS")
		if rok || wok {
			ts := r.TS
			if !rok {
				ts = w.TS
			}
			e.add(model.DBOps, r.Value+w.Value, ts, a)
		}
		e.one(v, k, "ReplicaLag", model.DBReplLag, 1000, a)    // seconds → ms
		e.one(v, k, "AuroraReplicaLag", model.DBReplLag, 1, a) // already ms
	}
	for _, g := range rs.caches {
		slug := model.Slug("aws", g.ID)
		for _, n := range g.Nodes {
			k := cacheKey(n.ClusterID)
			a := model.DBAttrs(slug, g.ID, "redis", n.ClusterID, n.Role)
			e.one(v, k, "EngineCPUUtilization", model.DBCPU, 0.01, a)
			e.one(v, k, "DatabaseMemoryUsagePercentage", model.DBMemory, 0.01, a)
			e.one(v, k, "CurrConnections", model.DBConnections, 1, a)
			e.one(v, k, "Evictions", model.DBEvictions, 1, a) // Sum over 60 s = per minute
			e.one(v, k, "CacheHitRate", model.DBHitRatio, 0.01, a)
			gt, gok := v.get(k, "GetTypeCmds")
			st, sok := v.get(k, "SetTypeCmds")
			if gok || sok {
				ts := gt.TS
				if !gok {
					ts = st.TS
				}
				e.add(model.DBOps, (gt.Value+st.Value)/60, ts, a)
			}
		}
	}
	for _, c := range rs.msk {
		cluster := model.Slug("aws", c.Name)
		for _, b := range c.Brokers {
			k := brokerKey(c.Name, b)
			a := model.NodeAttrs(cluster, "kafka", b)
			u, uok := v.get(k, "CpuUser")
			s, sok := v.get(k, "CpuSystem")
			if uok || sok {
				ts := u.TS
				if !uok {
					ts = s.TS
				}
				e.add(model.BrokerNodeCPU, clamp01((u.Value+s.Value)/100), ts, a)
			}
			used, ok1 := v.get(k, "MemoryUsed")
			free, ok2 := v.get(k, "MemoryFree")
			if ok1 && ok2 && used.Value+free.Value > 0 {
				e.add(model.BrokerNodeMemory, used.Value/(used.Value+free.Value), used.TS, a)
			}
			e.one(v, k, "KafkaDataLogsDiskUsed", model.BrokerNodeDisk, 0.01, a)
			e.one(v, k, "BytesInPerSec", model.BrokerNodeNetIn, 1e-6, a) // bytes/s → MB/s
		}
		for _, l := range c.Lags {
			e.one(v, lagKey(c.Name, l[0], l[1]), "SumOffsetLag", model.BrokerLag, 1, model.LagAttrs(cluster, "kafka", l[1], l[0], "0"))
		}
	}
	sqsCluster := model.Slug("aws", SQSName(rs.region))
	for _, qu := range rs.queues {
		k := queueKey(qu.Name)
		group := qu.Name
		if g := cfg.SQSConsumers[qu.Name]; g != "" {
			group = g
		}
		a := model.LagAttrs(sqsCluster, "aws_sqs", qu.Name, group, "0")
		e.one(v, k, "ApproximateNumberOfMessagesVisible", model.BrokerLag, 1, a)
		e.one(v, k, "ApproximateNumberOfMessagesNotVisible", model.BrokerUnacked, 1, a)
		e.one(v, k, "ApproximateAgeOfOldestMessage", model.BrokerOldestAge, 1, a)
		e.one(v, k, "NumberOfMessagesSent", model.BrokerProduceRate, 1.0/60, a)
		e.one(v, k, "NumberOfMessagesDeleted", model.BrokerConsumeRate, 1.0/60, a)
		ta := model.TopicAttrs(sqsCluster, "aws_sqs", qu.Name)
		if pt, ok := v.get(k, "NumberOfMessagesSent"); ok {
			e.add(model.TopicInRate, pt.Value/60, pt.TS, ta)
			if qu.RetentionSec > 0 {
				e.add(model.TopicRetentionMs, qu.RetentionSec*1000, pt.TS, ta)
			}
		}
	}
	return e.out
}

func clamp01(f float64) float64 { return max(0, min(1, f)) }

// InstanceMemoryGB returns the memory of an RDS instance class (db.<family>.<size>), 0 if unknown.
// Covers the burstable (t), general purpose (m) and memory optimized (r, x2g ×2) families; others via
// config.instanceMemoryGb.
func InstanceMemoryGB(class string, override map[string]float64) float64 {
	if gb, ok := override[class]; ok {
		return gb
	}
	parts := strings.Split(strings.TrimPrefix(class, "db."), ".")
	if len(parts) != 2 || parts[0] == "" {
		return 0
	}
	fam, size := parts[0], parts[1]
	if fam == "serverless" {
		return 0
	}
	// Multiplier of the size relative to "large".
	mult := 0.0
	switch size {
	case "large":
		mult = 1
	case "xlarge":
		mult = 2
	case "metal":
		return 0
	default:
		if n, err := strconv.Atoi(strings.TrimSuffix(size, "xlarge")); err == nil && strings.HasSuffix(size, "xlarge") {
			mult = float64(2 * n)
		}
	}
	switch fam[0] {
	case 't':
		switch size {
		case "micro":
			return 1
		case "small":
			return 2
		case "medium":
			return 4
		}
		return 8 * mult
	case 'm':
		return 8 * mult
	case 'r':
		return 16 * mult
	case 'x':
		return 32 * mult
	}
	return 0
}
