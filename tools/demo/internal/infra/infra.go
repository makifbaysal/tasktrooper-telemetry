// Package infra starts the demo's real databases / brokers (ttdemo-* docker containers), loads
// their schemas and data, and creates the Kafka topics and RabbitMQ topology.
package infra

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tasktrooper/telemetry/tools/demo/internal/world"
)

// Up starts every missing ttdemo container and waits until all accept connections.
func Up(ctx context.Context, log *slog.Logger) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("docker not found in PATH")
	}
	for _, c := range world.Containers() {
		name := world.Prefix + c.Name
		state, _ := output(ctx, "docker", "inspect", "-f", "{{.State.Running}}", name)
		switch strings.TrimSpace(state) {
		case "true":
			continue
		case "false":
			log.Info("starting container", "name", name)
			if _, err := output(ctx, "docker", "start", name); err != nil {
				return err
			}
			continue
		}
		args := []string{"run", "-d", "--name", name, "--label", "tasktrooper.demo=1"}
		for _, p := range c.Ports {
			args = append(args, "-p", "127.0.0.1:"+p)
		}
		for _, e := range c.Env {
			args = append(args, "-e", e)
		}
		args = append(args, c.Image)
		args = append(args, c.Args...)
		log.Info("creating container", "name", name, "image", c.Image)
		if out, err := output(ctx, "docker", args...); err != nil {
			return fmt.Errorf("docker run %s: %v: %s", name, err, out)
		}
	}
	return waitReady(ctx, log)
}

// Down removes the ttdemo containers.
func Down(ctx context.Context, log *slog.Logger) error {
	var names []string
	for _, c := range world.Containers() {
		names = append(names, world.Prefix+c.Name)
	}
	log.Info("removing containers", "n", len(names))
	out, err := output(ctx, "docker", append([]string{"rm", "-f", "-v"}, names...)...)
	if err != nil && !strings.Contains(out, "No such container") {
		return fmt.Errorf("docker rm: %v: %s", err, out)
	}
	return nil
}

func output(ctx context.Context, name string, args ...string) (string, error) {
	b, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(b), err
}

func waitReady(ctx context.Context, log *slog.Logger) error {
	_ = mysql.SetLogger(stdlog.New(io.Discard, "", 0)) // "unexpected EOF" while the server starts
	check := func(what string, f func(context.Context) error) error {
		deadline := time.Now().Add(5 * time.Minute)
		for {
			c, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := f(c)
			cancel()
			if err == nil {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s not ready: %w", what, err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	for _, p := range world.PGs {
		if err := check(p.ID, func(c context.Context) error {
			conn, err := pgx.Connect(c, world.PGDSN(p.ID, "postgres"))
			if err != nil {
				return err
			}
			defer conn.Close(context.Background())
			return conn.Ping(c)
		}); err != nil {
			return err
		}
	}
	for _, r := range world.Redises {
		if err := check(r.ID, func(c context.Context) error {
			cl := redis.NewClient(&redis.Options{Addr: world.RedisAddr(r.ID)})
			defer cl.Close()
			return cl.Ping(c).Err()
		}); err != nil {
			return err
		}
	}
	if err := check("notifications-db (mysql)", func(c context.Context) error {
		db, err := sql.Open("mysql", world.MySQLDSN("notifications"))
		if err != nil {
			return err
		}
		defer db.Close()
		return db.PingContext(c)
	}); err != nil {
		return err
	}
	if err := check("rabbitmq", func(c context.Context) error {
		conn, err := amqp.Dial(world.RabbitURL())
		if err != nil {
			return err
		}
		return conn.Close()
	}); err != nil {
		return err
	}
	log.Info("containers ready")
	return nil
}

// ---------------------------------------------------------------- data

// Load creates schemas and rows in every database (idempotent: tables are only filled when empty).
func Load(ctx context.Context, log *slog.Logger) error {
	for _, p := range world.PGs {
		if err := loadPG(ctx, log, p.ID); err != nil {
			return fmt.Errorf("%s: %w", p.ID, err)
		}
	}
	if err := loadMySQL(ctx); err != nil {
		return fmt.Errorf("notifications-db: %w", err)
	}
	return nil
}

func loadPG(ctx context.Context, log *slog.Logger, id string) error {
	admin, err := pgx.Connect(ctx, world.PGDSN(id, "postgres"))
	if err != nil {
		return err
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`); err != nil {
		return err
	}
	var exists bool
	if err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, id)); err != nil {
			return err
		}
	}
	conn, err := pgx.Connect(ctx, world.PGDSN(id, ""))
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	t0 := time.Now()
	for _, stmt := range pgSchemas[id] {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%.60s…: %w", stmt, err)
		}
	}
	log.Info("database loaded", "db", id, "took", time.Since(t0).Round(time.Millisecond))
	return nil
}

func loadMySQL(ctx context.Context) error {
	db, err := sql.Open("mysql", world.MySQLDSN("notifications"))
	if err != nil {
		return err
	}
	defer db.Close()
	for _, s := range mysqlSchema {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("%.60s…: %w", s, err)
		}
	}
	return nil
}

// RecoIndex creates (healthy) or drops (scenario reco-db-slow: "migration 0042") the index the
// recommendation query needs.
func RecoIndex(ctx context.Context, present bool) error {
	conn, err := pgx.Connect(ctx, world.PGDSN("reco-db", ""))
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	q := `DROP INDEX IF EXISTS idx_reco_items_user_score`
	if present {
		q = `CREATE INDEX IF NOT EXISTS idx_reco_items_user_score ON reco_items (user_id, score DESC)`
	}
	_, err = conn.Exec(ctx, q)
	return err
}

// ---------------------------------------------------------------- kafka / rabbit

// Topics creates the demo topics on the shared Redpanda (existing topics are kept).
func Topics(ctx context.Context, brokers []string) error {
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		return err
	}
	defer cl.Close()
	adm := kadm.NewClient(cl)
	existing, err := adm.ListTopics(ctx)
	if err != nil {
		return fmt.Errorf("list topics (is Redpanda up at %v?): %w", brokers, err)
	}
	for _, t := range world.Topics {
		if existing.Has(t.Name) {
			continue
		}
		ret := fmt.Sprint(t.Retention.Milliseconds())
		res, err := adm.CreateTopic(ctx, t.Partitions, 1, map[string]*string{"retention.ms": &ret}, t.Name)
		if err == nil {
			err = res.Err
		}
		if err != nil && !strings.Contains(err.Error(), "TOPIC_ALREADY_EXISTS") {
			return fmt.Errorf("create %s: %w", t.Name, err)
		}
	}
	return nil
}

// DeleteTopics removes the demo topics and consumer groups.
func DeleteTopics(ctx context.Context, brokers []string) error {
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		return err
	}
	defer cl.Close()
	adm := kadm.NewClient(cl)
	_, _ = adm.DeleteGroups(ctx, world.GroupFulfilment, world.GroupSearchIndex, world.GroupProjections, world.GroupDLQReplay,
		world.GroupS3Sink, world.GroupLedger, world.GroupRecoTrainer, world.GroupAudit)
	_, err = adm.DeleteTopics(ctx, world.TopicNames()...)
	return err
}

// Rabbit declares exchanges, queues and bindings (idempotent).
func Rabbit() error {
	conn, err := amqp.Dial(world.RabbitURL())
	if err != nil {
		return err
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	defer ch.Close()
	return DeclareRabbit(ch)
}

// DeclareRabbit declares the topology on ch.
func DeclareRabbit(ch *amqp.Channel) error {
	if err := ch.ExchangeDeclare(world.ExchangeEmails, "topic", true, false, false, false, nil); err != nil {
		return err
	}
	if err := ch.ExchangeDeclare(world.ExchangeEmailEvents, "fanout", true, false, false, false, nil); err != nil {
		return err
	}
	queues := []struct {
		name, exchange, key string
		ttl                 int64
	}{
		{world.QueueTransactional, world.ExchangeEmails, "email.transactional.#", 24 * 3600_000},
		{world.QueueMarketing, world.ExchangeEmails, "email.marketing.#", 72 * 3600_000},
		{world.QueueEmailEvents, world.ExchangeEmailEvents, "", 0},
	}
	for _, q := range queues {
		args := amqp.Table{}
		if q.ttl > 0 {
			args["x-message-ttl"] = q.ttl
		}
		if _, err := ch.QueueDeclare(q.name, true, false, false, false, args); err != nil {
			return fmt.Errorf("queue %s: %w", q.name, err)
		}
		if err := ch.QueueBind(q.name, q.key, q.exchange, false, nil); err != nil {
			return err
		}
	}
	return nil
}
