//go:build integration

// Package itest prepares real servers (started by `make itest-up`, container names ttcol-*)
// for the integration tests: monitoring users, extensions, load, Kafka topics and groups.
package itest

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Addresses (override with env).
var (
	PostgresAdmin = env("TTCOL_PG_ADMIN", "postgres://postgres:ttcol@localhost:55432/postgres?sslmode=disable")
	PostgresHost  = env("TTCOL_PG_HOST", "localhost")
	PostgresPort  = env("TTCOL_PG_PORT", "55432")
	MySQLAdmin    = env("TTCOL_MYSQL_ADMIN", "root:ttcol@tcp(localhost:53306)/?interpolateParams=true")
	MySQLPort     = env("TTCOL_MYSQL_PORT", "53306")
	RedisAddr     = env("TTCOL_REDIS", "localhost:56379")
	RabbitMgmt    = env("TTCOL_RABBIT_MGMT", "http://localhost:55672")
	RabbitAMQP    = env("TTCOL_RABBIT_AMQP", "amqp://ttcol:ttcol@localhost:55673/")
	KafkaBrokers  = env("TTCOL_KAFKA", "localhost:19092")
	// KafkaMetrics is the Redpanda Prometheus endpoint (node metrics).
	KafkaMetrics = env("TTCOL_KAFKA_METRICS", "http://localhost:9644/public_metrics")
	// RabbitProm is the rabbitmq_prometheus plugin endpoint (container port 15692).
	RabbitProm = env("TTCOL_RABBIT_PROM", "http://localhost:55692/metrics")
	// MongoURI is the ttcol-mongo single-node replica set (no auth).
	MongoURI = env("TTCOL_MONGO", "mongodb://localhost:57017/?directConnection=true")
	// Kafka4 is an Apache Kafka 4.x broker (ttcol-kafka4) for KIP-848 consumer groups.
	Kafka4 = env("TTCOL_KAFKA4", "localhost:59092")
)

// MonitorPassword is the password of the monitoring users created here.
const MonitorPassword = "trooper-pw"

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Wait retries fn until it succeeds or the deadline passes.
func Wait(t *testing.T, what string, d time.Duration, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(d)
	var err error
	for time.Now().Before(deadline) {
		if err = fn(); err == nil {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s not ready: %v", what, err)
}

// SetupPostgres creates pg_stat_statements and the trooper_monitor role (pg_monitor).
func SetupPostgres(t *testing.T) {
	ctx := context.Background()
	var conn *pgx.Conn
	Wait(t, "postgres", 90*time.Second, func() (err error) {
		conn, err = pgx.Connect(ctx, PostgresAdmin)
		if err == nil {
			err = conn.Ping(ctx)
		}
		return err
	})
	defer conn.Close(ctx)
	for _, q := range []string{
		`CREATE EXTENSION IF NOT EXISTS pg_stat_statements`,
		`DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'trooper_monitor') THEN
		   CREATE ROLE trooper_monitor LOGIN PASSWORD '` + MonitorPassword + `'; END IF; END $$`,
		`GRANT pg_monitor TO trooper_monitor`,
		`CREATE TABLE IF NOT EXISTS ttcol_orders (id serial PRIMARY KEY, total numeric)`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("postgres setup %q: %v", q, err)
		}
	}
}

// PostgresLoad runs n statements (insert + select) as the admin user.
func PostgresLoad(t *testing.T, n int) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, PostgresAdmin)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for i := 0; i < n; i++ {
		if _, err := conn.Exec(ctx, `INSERT INTO ttcol_orders (total) VALUES ($1)`, i); err != nil {
			t.Fatal(err)
		}
		var c int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM ttcol_orders WHERE total > $1`, i).Scan(&c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(ctx, `SELECT pg_sleep(0.6)`); err != nil { // one "slow" statement
		t.Fatal(err)
	}
}

// SetupMySQL creates trooper_monitor with the grants the UI shows.
func SetupMySQL(t *testing.T) {
	var db *sql.DB
	Wait(t, "mysql", 120*time.Second, func() (err error) {
		db, err = sql.Open("mysql", MySQLAdmin)
		if err == nil {
			err = db.Ping()
		}
		return err
	})
	defer db.Close()
	for _, q := range []string{
		`CREATE USER IF NOT EXISTS 'trooper_monitor'@'%' IDENTIFIED BY '` + MonitorPassword + `'`,
		`GRANT PROCESS, REPLICATION CLIENT ON *.* TO 'trooper_monitor'@'%'`,
		`GRANT SELECT ON performance_schema.* TO 'trooper_monitor'@'%'`,
		`CREATE DATABASE IF NOT EXISTS ttcol`,
		`CREATE TABLE IF NOT EXISTS ttcol.orders (id int AUTO_INCREMENT PRIMARY KEY, total int)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("mysql setup %q: %v", q, err)
		}
	}
}

// MySQLLoad runs n insert+select pairs.
func MySQLLoad(t *testing.T, n int) {
	db, err := sql.Open("mysql", MySQLAdmin)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 0; i < n; i++ {
		if _, err := db.Exec(`INSERT INTO ttcol.orders (total) VALUES (?)`, i); err != nil {
			t.Fatal(err)
		}
		var c int
		if err := db.QueryRow(`SELECT count(*) FROM ttcol.orders WHERE total > ?`, i).Scan(&c); err != nil {
			t.Fatal(err)
		}
	}
}

// RedisClient connects to the test Redis.
func RedisClient(t *testing.T) *redis.Client {
	c := redis.NewClient(&redis.Options{Addr: RedisAddr})
	Wait(t, "redis", 30*time.Second, func() error { return c.Ping(context.Background()).Err() })
	return c
}

// RedisLoad does n SETs, n GET hits and n/2 GET misses.
func RedisLoad(t *testing.T, c *redis.Client, n int) {
	ctx := context.Background()
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("ttcol:k:%d", i)
		if err := c.Set(ctx, k, i, time.Minute).Err(); err != nil {
			t.Fatal(err)
		}
		c.Get(ctx, k)
		if i%2 == 0 {
			c.Get(ctx, "ttcol:missing:"+k)
		}
	}
}

// SetupRabbit creates the `trooper` monitoring user (no vhost rights) via the management API.
func SetupRabbit(t *testing.T) {
	put := func(path, body string) error {
		req, _ := http.NewRequest(http.MethodPut, RabbitMgmt+path, strings.NewReader(body))
		req.SetBasicAuth("ttcol", "ttcol")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("PUT %s: %s", path, resp.Status)
		}
		return nil
	}
	Wait(t, "rabbitmq management", 120*time.Second, func() error {
		return put("/api/users/trooper", `{"password":"`+MonitorPassword+`","tags":"monitoring"}`)
	})
	if err := put("/api/permissions/%2F/trooper", `{"configure":"^$","write":"^$","read":"^$"}`); err != nil {
		t.Fatal(err)
	}
}

// RabbitQueue declares queue, publishes n messages and starts consumers with the given tags
// (prefetch 5, no acks, so unacked > 0 and ready > 0). Returns a cleanup func.
func RabbitQueue(t *testing.T, queue string, n int, tags ...string) func() {
	var conn *amqp.Connection
	Wait(t, "rabbitmq amqp", 60*time.Second, func() (err error) {
		conn, err = amqp.Dial(RabbitAMQP)
		return err
	})
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ch.QueueDeclare(queue, false, true, false, false, amqp.Table{"x-message-ttl": int32(600000)}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		// timestamped 1 min ago → head_message_timestamp / oldest_age_s ≈ 60
		pub := amqp.Publishing{Body: []byte("0123456789"), Timestamp: time.Now().Add(-time.Minute)}
		if err := ch.PublishWithContext(context.Background(), "", queue, false, false, pub); err != nil {
			t.Fatal(err)
		}
	}
	for _, tag := range tags {
		cch, err := conn.Channel()
		if err != nil {
			t.Fatal(err)
		}
		if err := cch.Qos(5, 0, false); err != nil {
			t.Fatal(err)
		}
		if _, err := cch.Consume(queue, tag, false, false, false, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	return func() { _, _ = ch.QueueDelete(queue, false, false, false); conn.Close() }
}

// Kafka holds a test topic + group: n records produced, the group committed `committed` of them
// on partition 0 and stays joined (Stable, 1 member) until Close.
type Kafka struct {
	Topic, Group string
	Brokers      string
	adm          *kadm.Client
	admCl        *kgo.Client
	consumer     *kgo.Client
}

// SetupKafka creates ttcol-<rand> topic (3 partitions) and group.
func SetupKafka(t *testing.T, n int) *Kafka { return SetupKafkaAt(t, KafkaBrokers, n, false) }

// SetupKafkaAt is SetupKafka against brokers; kip848 joins the group with the KIP-848 protocol
// (server-side assignment, Kafka 4.0+).
func SetupKafkaAt(t *testing.T, brokers string, n int, kip848 bool) *Kafka {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	suffix := fmt.Sprintf("%06d", rand.IntN(1_000_000))
	k := &Kafka{Topic: "ttcol-orders-" + suffix, Group: "ttcol-group-" + suffix, Brokers: brokers}
	var err error
	k.admCl, err = kgo.NewClient(kgo.SeedBrokers(strings.Split(k.Brokers, ",")...))
	if err != nil {
		t.Fatal(err)
	}
	k.adm = kadm.NewClient(k.admCl)
	retention := "3600000"
	if _, err := k.adm.CreateTopic(ctx, 3, 1, map[string]*string{"retention.ms": &retention}, k.Topic); err != nil {
		t.Fatal(err)
	}
	prod, err := kgo.NewClient(kgo.SeedBrokers(strings.Split(k.Brokers, ",")...), kgo.DefaultProduceTopic(k.Topic),
		kgo.RecordPartitioner(kgo.ManualPartitioner()))
	if err != nil {
		t.Fatal(err)
	}
	defer prod.Close()
	for i := 0; i < n; i++ {
		if err := prod.ProduceSync(ctx, &kgo.Record{Value: []byte("x"), Partition: int32(i % 3)}).FirstErr(); err != nil {
			t.Fatal(err)
		}
	}
	// consumer: join, poll a few records, commit them, stay in the group
	k.consumer, err = kgo.NewClient(append([]kgo.Opt{kgo.SeedBrokers(strings.Split(k.Brokers, ",")...),
		kgo.ConsumerGroup(k.Group), kgo.ConsumeTopics(k.Topic), kgo.DisableAutoCommit(),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart())}, groupProto(kip848)...)...)
	if err != nil {
		t.Fatal(err)
	}
	fetches := k.consumer.PollRecords(ctx, 5)
	if err := fetches.Err(); err != nil {
		t.Fatal(err)
	}
	if err := k.consumer.CommitRecords(ctx, fetches.Records()...); err != nil {
		t.Fatal(err)
	}
	return k
}

func groupProto(kip848 bool) []kgo.Opt {
	if kip848 {
		return []kgo.Opt{kgo.Balancers(kgo.RangeBalancer()), kgo.ServerSideBalancer()}
	}
	return nil
}

// Produce writes n records of size bytes round-robin over the 3 partitions.
func (k *Kafka) Produce(t *testing.T, n, size int) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	prod, err := kgo.NewClient(kgo.SeedBrokers(strings.Split(k.Brokers, ",")...), kgo.DefaultProduceTopic(k.Topic),
		kgo.RecordPartitioner(kgo.ManualPartitioner()))
	if err != nil {
		t.Fatal(err)
	}
	defer prod.Close()
	for i := 0; i < n; i++ {
		val := make([]byte, size) // random: incompressible, so on-disk size ≈ payload
		for j := range val {
			val[j] = byte(rand.IntN(256))
		}
		if err := prod.ProduceSync(ctx, &kgo.Record{Value: val, Partition: int32(i % 3)}).FirstErr(); err != nil {
			t.Fatal(err)
		}
	}
}

// Close leaves the group and deletes the topic and the group.
func (k *Kafka) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	k.consumer.Close()
	_, _ = k.adm.DeleteGroup(ctx, k.Group)
	_, _ = k.adm.DeleteTopics(ctx, k.Topic)
	k.admCl.Close()
}

// MySQLPreparedLoad keeps one server-side prepared statement allocated and executes it n times
// (it only shows up in prepared_statements_instances while allocated). Returns a close func.
func MySQLPreparedLoad(t *testing.T, n int) func() {
	dsn := strings.Replace(MySQLAdmin, "interpolateParams=true", "interpolateParams=false", 1)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	st, err := db.Prepare(`SELECT id, total FROM ttcol.orders WHERE total = ? AND id > 0`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		rows, err := st.Query(i)
		if err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	return func() { st.Close(); db.Close() }
}

// SetupMongo initiates the single-node replica set (once), enables $queryStats collection
// (internalQueryStatsRateLimit, 0 on self-managed servers by default) and seeds ttcol.orders.
func SetupMongo(t *testing.T) *mongo.Client {
	ctx := context.Background()
	cl, err := mongo.Connect(options.Client().ApplyURI(MongoURI).SetServerSelectionTimeout(5 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	admin := cl.Database("admin")
	Wait(t, "mongodb", 90*time.Second, func() error { return admin.RunCommand(ctx, bson.D{{Key: "ping", Value: 1}}).Err() })
	if err := admin.RunCommand(ctx, bson.D{{Key: "replSetGetStatus", Value: 1}}).Err(); err != nil {
		_ = admin.RunCommand(ctx, bson.D{{Key: "replSetInitiate", Value: bson.D{{Key: "_id", Value: "rs0"},
			{Key: "members", Value: bson.A{bson.D{{Key: "_id", Value: 0}, {Key: "host", Value: "localhost:27017"}}}}}}}).Err()
	}
	Wait(t, "mongodb primary", 60*time.Second, func() error {
		var st bson.M
		if err := admin.RunCommand(ctx, bson.D{{Key: "replSetGetStatus", Value: 1}}).Decode(&st); err != nil {
			return err
		}
		if fmt.Sprint(st["myState"]) != "1" {
			return fmt.Errorf("myState %v", st["myState"])
		}
		return nil
	})
	if err := admin.RunCommand(ctx, bson.D{{Key: "setParameter", Value: 1}, {Key: "internalQueryStatsRateLimit", Value: -1}}).Err(); err != nil {
		t.Logf("internalQueryStatsRateLimit: %v", err)
	}
	return cl
}

// MongoLoad inserts and queries ttcol.<coll> n times.
func MongoLoad(t *testing.T, cl *mongo.Client, coll string, n int) {
	ctx := context.Background()
	c := cl.Database("ttcol").Collection(coll)
	for i := 0; i < n; i++ {
		if _, err := c.InsertOne(ctx, bson.D{{Key: "status", Value: "new"}, {Key: "total", Value: i}}); err != nil {
			t.Fatal(err)
		}
		cur, err := c.Find(ctx, bson.D{{Key: "status", Value: "paid"}, {Key: "total", Value: bson.D{{Key: "$gt", Value: i}}}})
		if err != nil {
			t.Fatal(err)
		}
		cur.Close(ctx)
	}
}

// MongoProfile sets the profiler level of database ttcol (2 = all operations).
func MongoProfile(t *testing.T, cl *mongo.Client, level int) {
	if err := cl.Database("ttcol").RunCommand(context.Background(), bson.D{{Key: "profile", Value: level}}).Err(); err != nil {
		t.Fatal(err)
	}
}
