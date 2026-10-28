package aws

import (
	"context"
	"errors"
	"strconv"
	"strings"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	"github.com/aws/aws-sdk-go-v2/service/kafka"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/tasktrooper/telemetry/services/integrations/internal/model"
)

// Discovered resources per region (inventory State).

type rdsInstance struct {
	ID, Engine, Class, Role string
	Aurora                  bool
	AllocatedGB             float64
}

type cacheNode struct{ ClusterID, Role string }

type cacheGroup struct {
	ID    string
	Nodes []cacheNode
}

type mskCluster struct {
	Name    string
	Brokers []string // broker ids ("1", "2", ...)
	// Lags are the (consumer group, topic) pairs that have SumOffsetLag in CloudWatch.
	Lags [][2]string
}

type sqsQueue struct {
	Name         string
	RetentionSec float64
}

type regionState struct {
	region    string
	rds       []rdsInstance
	caches    []cacheGroup
	msk       []mskCluster
	queues    []sqsQueue
	functions []lambdaFn
}

type state struct{ regions []*regionState }

// SQSName is the broker name of a region's SQS queues (slug aws-sqs-<region>).
func SQSName(region string) string { return "sqs-" + region }

// Discover implements model.Provider.
func (p *Provider) Discover(ctx context.Context) (*model.Inventory, error) {
	st := &state{}
	inv := &model.Inventory{State: st}
	var errs error
	for _, c := range p.regions {
		rs := &regionState{region: c.region}
		st.regions = append(st.regions, rs)
		if p.enabled("rds") {
			errs = errors.Join(errs, p.discoverRDS(ctx, c, rs, inv))
		}
		if p.enabled("elasticache") {
			errs = errors.Join(errs, p.discoverCache(ctx, c, rs, inv))
		}
		if p.enabled("msk") {
			errs = errors.Join(errs, p.discoverMSK(ctx, c, rs, inv))
		}
		if p.enabled("sqs") {
			errs = errors.Join(errs, p.discoverSQS(ctx, c, rs, inv))
		}
		if p.enabled("lambda") {
			errs = errors.Join(errs, p.discoverLambda(ctx, c, rs, inv))
		}
	}
	if inv.Count() == 0 && errs != nil {
		return nil, errs
	}
	return inv, errs
}

func dbEngine(e string) string {
	switch {
	case strings.Contains(e, "postgres"):
		return "postgres"
	case strings.Contains(e, "mysql"), strings.Contains(e, "mariadb"):
		return "mysql"
	}
	return "" // oracle, sqlserver, db2: unsupported engines
}

func (p *Provider) discoverRDS(ctx context.Context, c *clients, rs *regionState, inv *model.Inventory) error {
	// Aurora writer/reader roles.
	writers := map[string]bool{}
	cp := rds.NewDescribeDBClustersPaginator(c.rds, &rds.DescribeDBClustersInput{})
	for cp.HasMorePages() {
		out, err := cp.NextPage(ctx)
		if err != nil {
			break // DescribeDBClusters is optional (no Aurora / no permission)
		}
		for _, cl := range out.DBClusters {
			for _, m := range cl.DBClusterMembers {
				writers[sdk.ToString(m.DBInstanceIdentifier)] = sdk.ToBool(m.IsClusterWriter)
			}
		}
	}
	pg := rds.NewDescribeDBInstancesPaginator(c.rds, &rds.DescribeDBInstancesInput{})
	for pg.HasMorePages() {
		out, err := pg.NextPage(ctx)
		if err != nil {
			return wrap(c.region, "rds:DescribeDBInstances", err)
		}
		for _, db := range out.DBInstances {
			engine := dbEngine(sdk.ToString(db.Engine))
			if engine == "" {
				continue
			}
			id := sdk.ToString(db.DBInstanceIdentifier)
			aurora := strings.HasPrefix(sdk.ToString(db.Engine), "aurora")
			role := "primary"
			if sdk.ToString(db.ReadReplicaSourceDBInstanceIdentifier) != "" {
				role = "replica"
			}
			if w, ok := writers[id]; ok && !w {
				role = "replica"
			}
			inst := rdsInstance{ID: id, Engine: engine, Class: sdk.ToString(db.DBInstanceClass), Role: role, Aurora: aurora}
			d := model.Database{
				Name: id, Engine: engine, Source: "AWS RDS · " + c.region, Version: sdk.ToString(db.EngineVersion),
				Zone: sdk.ToString(db.AvailabilityZone), InstanceClass: inst.Class, CloudResourceID: sdk.ToString(db.DBInstanceArn), Env: p.cfg.Env,
				Meta: map[string]any{"engine": sdk.ToString(db.Engine), "status": sdk.ToString(db.DBInstanceStatus), "multiAZ": sdk.ToBool(db.MultiAZ),
					"role": role, "region": c.region},
			}
			if aurora {
				d.Source = "Amazon Aurora · " + c.region
				d.Meta["cluster"] = sdk.ToString(db.DBClusterIdentifier)
			} else if db.AllocatedStorage != nil {
				inst.AllocatedGB = float64(*db.AllocatedStorage)
				d.DiskGB = model.F(inst.AllocatedGB)
			}
			if r := sdk.ToString(db.ReadReplicaSourceDBInstanceIdentifier); r != "" {
				d.Meta["replicaSource"] = r
			}
			if db.Endpoint != nil {
				d.Host, d.Port = sdk.ToString(db.Endpoint.Address), int(sdk.ToInt32(db.Endpoint.Port))
			}
			rs.rds = append(rs.rds, inst)
			inv.Databases = append(inv.Databases, d)
		}
	}
	return nil
}

func (p *Provider) discoverCache(ctx context.Context, c *clients, rs *regionState, inv *model.Inventory) error {
	pg := elasticache.NewDescribeReplicationGroupsPaginator(c.ec, &elasticache.DescribeReplicationGroupsInput{})
	for pg.HasMorePages() {
		out, err := pg.NextPage(ctx)
		if err != nil {
			return wrap(c.region, "elasticache:DescribeReplicationGroups", err)
		}
		for _, g := range out.ReplicationGroups {
			if e := sdk.ToString(g.Engine); e != "" && e != "redis" && e != "valkey" {
				continue
			}
			cg := cacheGroup{ID: sdk.ToString(g.ReplicationGroupId)}
			roles := map[string]string{}
			var host string
			var port int
			for _, ng := range g.NodeGroups {
				if ng.PrimaryEndpoint != nil && host == "" {
					host, port = sdk.ToString(ng.PrimaryEndpoint.Address), int(sdk.ToInt32(ng.PrimaryEndpoint.Port))
				}
				for _, m := range ng.NodeGroupMembers {
					roles[sdk.ToString(m.CacheClusterId)] = sdk.ToString(m.CurrentRole)
				}
			}
			if g.ConfigurationEndpoint != nil {
				host, port = sdk.ToString(g.ConfigurationEndpoint.Address), int(sdk.ToInt32(g.ConfigurationEndpoint.Port))
			}
			for _, m := range g.MemberClusters {
				role := roles[m]
				if role == "" {
					role = "primary"
				}
				cg.Nodes = append(cg.Nodes, cacheNode{ClusterID: m, Role: role})
			}
			rs.caches = append(rs.caches, cg)
			inv.Databases = append(inv.Databases, model.Database{
				Name: cg.ID, Engine: "redis", Host: host, Port: port, Source: "Amazon ElastiCache · " + c.region,
				Version: sdk.ToString(g.Engine), InstanceClass: sdk.ToString(g.CacheNodeType), CloudResourceID: sdk.ToString(g.ARN), Env: p.cfg.Env,
				Meta: map[string]any{"status": sdk.ToString(g.Status), "members": g.MemberClusters, "clusterEnabled": sdk.ToBool(g.ClusterEnabled), "region": c.region},
			})
		}
	}
	return nil
}

func (p *Provider) discoverMSK(ctx context.Context, c *clients, rs *regionState, inv *model.Inventory) error {
	pg := kafka.NewListClustersV2Paginator(c.msk, &kafka.ListClustersV2Input{})
	var errs error
	for pg.HasMorePages() {
		out, err := pg.NextPage(ctx)
		if err != nil {
			return wrap(c.region, "kafka:ListClustersV2", err)
		}
		for _, cl := range out.ClusterInfoList {
			name, arn := sdk.ToString(cl.ClusterName), sdk.ToString(cl.ClusterArn)
			mc := mskCluster{Name: name}
			b := model.Broker{Name: name, Kind: "kafka", Endpoint: arn, Source: "Amazon MSK · " + c.region,
				Meta: map[string]any{"arn": arn, "state": string(cl.State), "type": string(cl.ClusterType), "region": c.region}}
			if pr := cl.Provisioned; pr != nil {
				if pr.CurrentBrokerSoftwareInfo != nil {
					b.Version = sdk.ToString(pr.CurrentBrokerSoftwareInfo.KafkaVersion)
				}
				if pr.BrokerNodeGroupInfo != nil {
					b.Meta["instanceType"] = sdk.ToString(pr.BrokerNodeGroupInfo.InstanceType)
				}
				b.Meta["brokers"] = sdk.ToInt32(pr.NumberOfBrokerNodes)
				np := kafka.NewListNodesPaginator(c.msk, &kafka.ListNodesInput{ClusterArn: cl.ClusterArn})
				for np.HasMorePages() {
					nodes, err := np.NextPage(ctx)
					if err != nil {
						errs = errors.Join(errs, wrap(c.region, "kafka:ListNodes "+name, err))
						break
					}
					for _, n := range nodes.NodeInfoList {
						if n.BrokerNodeInfo != nil && n.BrokerNodeInfo.BrokerId != nil {
							mc.Brokers = append(mc.Brokers, strconv.FormatFloat(*n.BrokerNodeInfo.BrokerId, 'f', -1, 64))
						}
					}
				}
			}
			if bb, err := c.msk.GetBootstrapBrokers(ctx, &kafka.GetBootstrapBrokersInput{ClusterArn: cl.ClusterArn}); err == nil {
				for _, s := range []*string{bb.BootstrapBrokerStringSaslIam, bb.BootstrapBrokerStringTls, bb.BootstrapBrokerStringSaslScram, bb.BootstrapBrokerString} {
					if sdk.ToString(s) != "" {
						b.Endpoint = *s
						break
					}
				}
			}
			// Consumer group / topic pairs with lag metrics.
			lp := cloudwatch.NewListMetricsPaginator(c.cw, &cloudwatch.ListMetricsInput{
				Namespace: sdk.String("AWS/Kafka"), MetricName: sdk.String("SumOffsetLag"),
				Dimensions: []cwtypes.DimensionFilter{{Name: sdk.String("Cluster Name"), Value: sdk.String(name)}},
			})
			for lp.HasMorePages() {
				lm, err := lp.NextPage(ctx)
				if err != nil {
					errs = errors.Join(errs, wrap(c.region, "cloudwatch:ListMetrics "+name, err))
					break
				}
				for _, m := range lm.Metrics {
					var group, topic string
					for _, d := range m.Dimensions {
						switch sdk.ToString(d.Name) {
						case "Consumer Group":
							group = sdk.ToString(d.Value)
						case "Topic":
							topic = sdk.ToString(d.Value)
						}
					}
					if group != "" && topic != "" && len(m.Dimensions) == 3 {
						mc.Lags = append(mc.Lags, [2]string{group, topic})
					}
				}
			}
			rs.msk = append(rs.msk, mc)
			inv.Brokers = append(inv.Brokers, b)
		}
	}
	return errs
}

func (p *Provider) discoverSQS(ctx context.Context, c *clients, rs *regionState, inv *model.Inventory) error {
	pg := sqs.NewListQueuesPaginator(c.sqs, &sqs.ListQueuesInput{MaxResults: sdk.Int32(1000)})
	var urls []string
	for pg.HasMorePages() && len(urls) < p.cfg.MaxQueues {
		out, err := pg.NextPage(ctx)
		if err != nil {
			return wrap(c.region, "sqs:ListQueues", err)
		}
		urls = append(urls, out.QueueUrls...)
	}
	if len(urls) > p.cfg.MaxQueues {
		urls = urls[:p.cfg.MaxQueues]
	}
	if len(urls) == 0 {
		return nil
	}
	var errs error
	meta := make([]map[string]any, 0, len(urls))
	for _, u := range urls {
		name := u[strings.LastIndex(u, "/")+1:]
		q := sqsQueue{Name: name}
		m := map[string]any{"name": name, "url": u, "fifo": strings.HasSuffix(name, ".fifo")}
		at, err := c.sqs.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: sdk.String(u), AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameAll}})
		if err != nil {
			errs = errors.Join(errs, wrap(c.region, "sqs:GetQueueAttributes "+name, err))
		} else {
			if v, err := strconv.ParseFloat(at.Attributes["MessageRetentionPeriod"], 64); err == nil {
				q.RetentionSec = v
				m["retentionSeconds"] = v
			}
			if v := at.Attributes["VisibilityTimeout"]; v != "" {
				m["visibilityTimeout"] = v
			}
			if v := at.Attributes["RedrivePolicy"]; v != "" {
				m["redrivePolicy"] = v
			}
		}
		rs.queues = append(rs.queues, q)
		if len(meta) < 500 {
			meta = append(meta, m)
		}
	}
	inv.Brokers = append(inv.Brokers, model.Broker{
		Name: SQSName(c.region), Kind: "sqs", Endpoint: "https://sqs." + c.region + ".amazonaws.com", Source: "Amazon SQS · " + c.region,
		Meta: map[string]any{"queues": meta, "queueCount": len(urls), "region": c.region},
	})
	return errs
}
