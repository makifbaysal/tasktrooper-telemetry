// Package fakes holds wire-level fake cloud APIs shared by provider tests and the e2e test.
package fakes

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/smithy-go/encoding/cbor"
)

// fakeAWS speaks the wire protocols the SDK uses: awsquery XML (RDS, ElastiCache), awsJson1.0 (SQS),
// restJson (MSK) and rpc-v2-cbor (CloudWatch). Responses are recorded-shape fixtures.
// AWS is a fake AWS endpoint.
type AWS struct {
	*httptest.Server
	t testing.TB
	// metric returns the CloudWatch value for (metric name, dimensions) or false.
	metric func(name string, dims map[string]string) (float64, bool)
	ts     time.Time

	mu       sync.Mutex
	GMDCalls int
	Queries  int
}

// NewAWS starts the fake; metric returns the CloudWatch value for (metric, dimensions).
func NewAWS(t testing.TB, ts time.Time, metric func(string, map[string]string) (float64, bool)) *AWS {
	f := &AWS{t: t, metric: metric, ts: ts}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *AWS) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	switch {
	case strings.HasPrefix(r.URL.Path, "/service/GraniteServiceVersion20100801/operation/"):
		f.cloudwatch(w, strings.TrimPrefix(r.URL.Path, "/service/GraniteServiceVersion20100801/operation/"), body)
	case r.Header.Get("X-Amz-Target") != "":
		f.sqs(w, r.Header.Get("X-Amz-Target"), body)
	case strings.HasPrefix(r.URL.Path, "/api/v2/clusters") || strings.HasPrefix(r.URL.Path, "/v1/clusters"):
		f.kafka(w, r)
	default:
		form, _ := url.ParseQuery(string(body))
		f.query(w, form.Get("Action"))
	}
}

const rdsXML = `<DescribeDBInstancesResponse xmlns="http://rds.amazonaws.com/doc/2014-10-31/"><DescribeDBInstancesResult><DBInstances>
<DBInstance><DBInstanceIdentifier>orders-db</DBInstanceIdentifier><Engine>postgres</Engine><EngineVersion>16.3</EngineVersion>
 <DBInstanceClass>db.r6g.large</DBInstanceClass><AvailabilityZone>eu-central-1a</AvailabilityZone><AllocatedStorage>100</AllocatedStorage>
 <DBInstanceStatus>available</DBInstanceStatus><MultiAZ>true</MultiAZ><DBInstanceArn>arn:aws:rds:eu-central-1:1:db:orders-db</DBInstanceArn>
 <Endpoint><Address>orders-db.abc.eu-central-1.rds.amazonaws.com</Address><Port>5432</Port></Endpoint></DBInstance>
<DBInstance><DBInstanceIdentifier>orders-db-ro</DBInstanceIdentifier><Engine>postgres</Engine><EngineVersion>16.3</EngineVersion>
 <DBInstanceClass>db.r6g.large</DBInstanceClass><AllocatedStorage>100</AllocatedStorage><ReadReplicaSourceDBInstanceIdentifier>orders-db</ReadReplicaSourceDBInstanceIdentifier>
 <Endpoint><Address>orders-db-ro.abc.eu-central-1.rds.amazonaws.com</Address><Port>5432</Port></Endpoint></DBInstance>
<DBInstance><DBInstanceIdentifier>users-aurora-2</DBInstanceIdentifier><Engine>aurora-mysql</Engine><EngineVersion>8.0.mysql_aurora.3.05.2</EngineVersion>
 <DBInstanceClass>db.r6g.xlarge</DBInstanceClass><AllocatedStorage>1</AllocatedStorage><DBClusterIdentifier>users</DBClusterIdentifier>
 <Endpoint><Address>users-aurora-2.abc.rds.amazonaws.com</Address><Port>3306</Port></Endpoint></DBInstance>
<DBInstance><DBInstanceIdentifier>erp</DBInstanceIdentifier><Engine>oracle-ee</Engine><DBInstanceClass>db.m5.large</DBInstanceClass></DBInstance>
</DBInstances></DescribeDBInstancesResult><ResponseMetadata><RequestId>r1</RequestId></ResponseMetadata></DescribeDBInstancesResponse>`

const clustersXML = `<DescribeDBClustersResponse xmlns="http://rds.amazonaws.com/doc/2014-10-31/"><DescribeDBClustersResult><DBClusters>
<DBCluster><DBClusterIdentifier>users</DBClusterIdentifier><DBClusterMembers>
 <DBClusterMember><DBInstanceIdentifier>users-aurora-1</DBInstanceIdentifier><IsClusterWriter>true</IsClusterWriter></DBClusterMember>
 <DBClusterMember><DBInstanceIdentifier>users-aurora-2</DBInstanceIdentifier><IsClusterWriter>false</IsClusterWriter></DBClusterMember>
</DBClusterMembers></DBCluster></DBClusters></DescribeDBClustersResult><ResponseMetadata><RequestId>r2</RequestId></ResponseMetadata></DescribeDBClustersResponse>`

const cacheXML = `<DescribeReplicationGroupsResponse xmlns="http://elasticache.amazonaws.com/doc/2015-02-02/"><DescribeReplicationGroupsResult><ReplicationGroups>
<ReplicationGroup><ReplicationGroupId>sessions</ReplicationGroupId><Engine>redis</Engine><Status>available</Status><CacheNodeType>cache.r7g.large</CacheNodeType>
 <ARN>arn:aws:elasticache:eu-central-1:1:replicationgroup:sessions</ARN><ClusterEnabled>false</ClusterEnabled>
 <MemberClusters><ClusterId>sessions-001</ClusterId><ClusterId>sessions-002</ClusterId></MemberClusters>
 <NodeGroups><NodeGroup><NodeGroupId>0001</NodeGroupId><PrimaryEndpoint><Address>sessions.abc.cache.amazonaws.com</Address><Port>6379</Port></PrimaryEndpoint>
  <NodeGroupMembers>
   <NodeGroupMember><CacheClusterId>sessions-001</CacheClusterId><CurrentRole>primary</CurrentRole></NodeGroupMember>
   <NodeGroupMember><CacheClusterId>sessions-002</CacheClusterId><CurrentRole>replica</CurrentRole></NodeGroupMember>
  </NodeGroupMembers></NodeGroup></NodeGroups></ReplicationGroup>
<ReplicationGroup><ReplicationGroupId>memc</ReplicationGroupId><Engine>memcached</Engine></ReplicationGroup>
</ReplicationGroups></DescribeReplicationGroupsResult><ResponseMetadata><RequestId>r3</RequestId></ResponseMetadata></DescribeReplicationGroupsResponse>`

func (f *AWS) query(w http.ResponseWriter, action string) {
	w.Header().Set("Content-Type", "text/xml")
	switch action {
	case "DescribeDBInstances":
		_, _ = io.WriteString(w, rdsXML)
	case "DescribeDBClusters":
		_, _ = io.WriteString(w, clustersXML)
	case "DescribeReplicationGroups":
		_, _ = io.WriteString(w, cacheXML)
	default:
		f.t.Errorf("unexpected query action %q", action)
		w.WriteHeader(400)
	}
}

func (f *AWS) sqs(w http.ResponseWriter, target string, body []byte) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.0")
	switch target {
	case "AmazonSQS.ListQueues":
		_, _ = io.WriteString(w, `{"QueueUrls":["https://sqs.eu-central-1.amazonaws.com/1/emails","https://sqs.eu-central-1.amazonaws.com/1/orders.fifo"]}`)
	case "AmazonSQS.GetQueueAttributes":
		var in struct{ QueueUrl string }
		_ = json.Unmarshal(body, &in)
		ret := "345600"
		if strings.HasSuffix(in.QueueUrl, "emails") {
			ret = "86400"
		}
		_, _ = io.WriteString(w, `{"Attributes":{"MessageRetentionPeriod":"`+ret+`","VisibilityTimeout":"30"}}`)
	default:
		f.t.Errorf("unexpected sqs target %q", target)
		w.WriteHeader(400)
	}
}

func (f *AWS) kafka(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	p := r.URL.Path
	switch {
	case p == "/api/v2/clusters":
		_, _ = io.WriteString(w, `{"clusterInfoList":[{"clusterName":"events","clusterArn":"arn:aws:kafka:eu-central-1:1:cluster/events/abc","clusterType":"PROVISIONED","state":"ACTIVE",
		 "provisioned":{"numberOfBrokerNodes":2,"currentBrokerSoftwareInfo":{"kafkaVersion":"3.6.0"},"brokerNodeGroupInfo":{"instanceType":"kafka.m5.large"}}}]}`)
	case strings.HasSuffix(p, "/nodes"):
		_, _ = io.WriteString(w, `{"nodeInfoList":[{"brokerNodeInfo":{"brokerId":1.0}},{"brokerNodeInfo":{"brokerId":2.0}}]}`)
	case strings.HasSuffix(p, "/bootstrap-brokers"):
		_, _ = io.WriteString(w, `{"bootstrapBrokerStringSaslIam":"b-1.events.kafka.eu-central-1.amazonaws.com:9098"}`)
	default:
		f.t.Errorf("unexpected kafka path %q", p)
		w.WriteHeader(404)
	}
}

func str(v cbor.Value) string {
	if s, ok := v.(cbor.String); ok {
		return string(s)
	}
	return ""
}

func (f *AWS) cloudwatch(w http.ResponseWriter, op string, body []byte) {
	w.Header().Set("Smithy-Protocol", "rpc-v2-cbor")
	w.Header().Set("Content-Type", "application/cbor")
	in, err := cbor.Decode(body)
	if err != nil {
		f.t.Errorf("cbor decode: %v", err)
		w.WriteHeader(400)
		return
	}
	req, _ := in.(cbor.Map)
	switch op {
	case "ListMetrics":
		dim := func(n, v string) cbor.Value { return cbor.Map{"Name": cbor.String(n), "Value": cbor.String(v)} }
		_, _ = w.Write(cbor.Encode(cbor.Map{"Metrics": cbor.List{
			cbor.Map{"Namespace": cbor.String("AWS/Kafka"), "MetricName": cbor.String("SumOffsetLag"), "Dimensions": cbor.List{
				dim("Cluster Name", "events"), dim("Consumer Group", "billing"), dim("Topic", "orders")}},
			cbor.Map{"Namespace": cbor.String("AWS/Kafka"), "MetricName": cbor.String("SumOffsetLag"), "Dimensions": cbor.List{
				dim("Cluster Name", "events"), dim("Consumer Group", "billing")}}, // group-level: ignored
		}}))
	case "GetMetricData":
		qs, _ := req["MetricDataQueries"].(cbor.List)
		f.mu.Lock()
		f.GMDCalls++
		f.Queries += len(qs)
		f.mu.Unlock()
		if len(qs) > 500 {
			f.t.Errorf("batch of %d queries", len(qs))
		}
		var results cbor.List
		for _, qv := range qs {
			qm := qv.(cbor.Map)
			ms := qm["MetricStat"].(cbor.Map)
			m := ms["Metric"].(cbor.Map)
			dims := map[string]string{}
			if dl, ok := m["Dimensions"].(cbor.List); ok {
				for _, d := range dl {
					dm := d.(cbor.Map)
					dims[str(dm["Name"])] = str(dm["Value"])
				}
			}
			res := cbor.Map{"Id": qm["Id"], "StatusCode": cbor.String("Complete"), "Timestamps": cbor.List{}, "Values": cbor.List{}}
			if v, ok := f.metric(str(m["MetricName"]), dims); ok {
				// Newest first (ScanBy TimestampDescending), plus an older point that must be ignored.
				res["Timestamps"] = cbor.List{
					&cbor.Tag{ID: 1, Value: cbor.Float64(float64(f.ts.Unix()))},
					&cbor.Tag{ID: 1, Value: cbor.Float64(float64(f.ts.Add(-time.Minute).Unix()))},
				}
				res["Values"] = cbor.List{cbor.Float64(v), cbor.Float64(-1)}
			}
			results = append(results, res)
		}
		_, _ = w.Write(cbor.Encode(cbor.Map{"MetricDataResults": results}))
	default:
		f.t.Errorf("unexpected cloudwatch op %q", op)
		w.WriteHeader(400)
	}
}
