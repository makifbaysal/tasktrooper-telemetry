package svc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/propagation"

	trooper "github.com/tasktrooper/telemetry-go"
	trooperamqp "github.com/tasktrooper/telemetry-go/integrations/amqp091"
	trooperfranz "github.com/tasktrooper/telemetry-go/integrations/franzgo"
	troopersql "github.com/tasktrooper/telemetry-go/integrations/sql"

	"github.com/tasktrooper/telemetry/tools/demo/internal/infra"
	"github.com/tasktrooper/telemetry/tools/demo/internal/world"
)

// ---------------------------------------------------------------- Kafka: one goroutine per assigned partition

type kafkaConsumer struct {
	a       *App
	group   string
	process string
	topics  []string
	handle  func(ctx context.Context, rec *kgo.Record) error
	// throttle returns how long to wait before the next record of a partition (outside the consumer span).
	throttle func(partition int32) time.Duration

	cl      *kgo.Client
	mu      sync.Mutex
	workers map[string]*partWorker
	inflow  sync.Map // partition → *rateMeter
}

type partWorker struct {
	mu     sync.Mutex
	queue  []*kgo.Record
	wake   chan struct{}
	quit   chan struct{}
	done   chan struct{}
	paused bool
}

func (k *kafkaConsumer) run(ctx context.Context) error {
	tt := trooperfranz.New(trooperfranz.WithConsumerGroup(k.group), trooperfranz.WithProcess(k.process), trooperfranz.WithAttemptHeader("tt-attempt"))
	k.workers = map[string]*partWorker{}
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(k.a.RT.Kafka...),
		kgo.ConsumerGroup(k.group),
		kgo.ConsumeTopics(k.topics...),
		kgo.ClientID(fmt.Sprintf("%s-%d", k.a.Name, k.a.Instance)),
		kgo.AutoCommitMarks(),
		kgo.AutoCommitInterval(2*time.Second),
		kgo.BlockRebalanceOnPoll(),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtEnd()),
		kgo.OnPartitionsAssigned(func(ctx context.Context, cl *kgo.Client, assigned map[string][]int32) {
			k.mu.Lock()
			defer k.mu.Unlock()
			for t, ps := range assigned {
				for _, p := range ps {
					w := &partWorker{wake: make(chan struct{}, 1), quit: make(chan struct{}), done: make(chan struct{})}
					k.workers[key(t, p)] = w
					go k.work(ctx, tt, t, p, w)
				}
			}
		}),
		kgo.OnPartitionsRevoked(k.stop),
		kgo.OnPartitionsLost(k.stop),
	)
	if err != nil {
		return err
	}
	k.cl = cl
	defer func() {
		cl.CommitMarkedOffsets(context.Background())
		cl.Close()
	}()
	for {
		fetches := cl.PollRecords(ctx, 500)
		if fetches.IsClientClosed() || ctx.Err() != nil {
			return ctx.Err()
		}
		fetches.EachError(func(t string, p int32, err error) {
			k.a.Log.Warn("fetch error", "topic", t, "partition", p, "err", err.Error())
		})
		fetches.EachPartition(func(p kgo.FetchTopicPartition) {
			if m, _ := k.inflow.LoadOrStore(p.Partition, &rateMeter{}); true {
				m.(*rateMeter).observe(p.HighWatermark)
			}
			k.mu.Lock()
			w := k.workers[key(p.Topic, p.Partition)]
			k.mu.Unlock()
			if w == nil || len(p.Records) == 0 {
				return
			}
			w.mu.Lock()
			w.queue = append(w.queue, p.Records...)
			if len(w.queue) > 2000 && !w.paused {
				w.paused = true
				cl.PauseFetchPartitions(map[string][]int32{p.Topic: {p.Partition}})
			}
			w.mu.Unlock()
			select {
			case w.wake <- struct{}{}:
			default:
			}
		})
		cl.AllowRebalance()
	}
}

func key(t string, p int32) string { return t + "/" + strconv.Itoa(int(p)) }

func (k *kafkaConsumer) stop(ctx context.Context, cl *kgo.Client, lost map[string][]int32) {
	k.mu.Lock()
	var ws []*partWorker
	for t, ps := range lost {
		for _, p := range ps {
			if w := k.workers[key(t, p)]; w != nil {
				ws = append(ws, w)
				delete(k.workers, key(t, p))
			}
		}
	}
	k.mu.Unlock()
	for _, w := range ws {
		close(w.quit)
		<-w.done
	}
	cl.ResumeFetchPartitions(lost)
	cl.CommitMarkedOffsets(ctx)
}

func (k *kafkaConsumer) work(ctx context.Context, tt *trooperfranz.Tracer, topic string, part int32, w *partWorker) {
	defer close(w.done)
	for {
		w.mu.Lock()
		var rec *kgo.Record
		if len(w.queue) > 0 {
			rec, w.queue = w.queue[0], w.queue[1:]
			if w.paused && len(w.queue) < 500 {
				w.paused = false
				k.cl.ResumeFetchPartitions(map[string][]int32{topic: {part}})
			}
		}
		w.mu.Unlock()
		if rec == nil {
			select {
			case <-w.quit:
				return
			case <-ctx.Done():
				return
			case <-w.wake:
			}
			continue
		}
		if k.throttle != nil {
			if d := k.throttle(part); d > 0 {
				select {
				case <-w.quit:
					return
				case <-time.After(d):
				}
			}
		}
		_ = tt.ProcessRecord(context.Background(), rec, func(ctx context.Context) error { return k.handle(ctx, rec) })
		k.cl.MarkCommitRecords(rec)
	}
}

// rateMeter estimates a partition's produce rate from high-watermark deltas.
type rateMeter struct {
	mu   sync.Mutex
	t0   time.Time
	hw0  int64
	rate float64
}

func (m *rateMeter) observe(hw int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if m.t0.IsZero() {
		m.t0, m.hw0 = now, hw
		return
	}
	if dt := now.Sub(m.t0).Seconds(); dt >= 15 {
		r := float64(hw-m.hw0) / dt
		if m.rate == 0 {
			m.rate = r
		} else {
			m.rate = 0.7*m.rate + 0.3*r
		}
		m.t0, m.hw0 = now, hw
	}
}

func (m *rateMeter) get() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rate
}

// ---------------------------------------------------------------- order-events-consumer

func runOrderEvents(ctx context.Context, a *App, _ int) error {
	db, err := a.PG(ctx, "orders-db", 4, 1)
	if err != nil {
		return err
	}
	cache := a.Redis("cache-main")
	kc, err := a.Producer()
	if err != nil {
		return err
	}
	retry := func(ctx context.Context, rec *kgo.Record, cause error) error {
		attempt, _ := strconv.Atoi(trooperfranz.Carrier{Headers: &rec.Headers}.Get("tt-attempt"))
		if attempt < 1 {
			attempt = 1
		}
		out := &kgo.Record{Key: rec.Key, Value: rec.Value}
		if attempt >= 3 {
			trooper.MarkDLQ(ctx)
			out.Topic = "orders.created.dlq"
			a.Log.ErrorContext(ctx, fmt.Sprintf("giving up after %d attempts, sent to orders.created.dlq reason=%v", attempt, cause))
		} else {
			out.Topic = rec.Topic
			trooper.SetAttempt(trooperfranz.Carrier{Headers: &out.Headers}, attempt+1)
			a.Log.WarnContext(ctx, fmt.Sprintf("retrying message attempt=%d topic=%s reason=lock_timeout", attempt+1, rec.Topic))
		}
		kc.Produce(context.WithoutCancel(ctx), out, nil)
		return cause
	}
	consumers := []*kafkaConsumer{
		{group: world.GroupFulfilment, process: "order-created-handler", topics: []string{"orders.created"},
			handle: func(ctx context.Context, rec *kgo.Record) error {
				id := ID(1, 200000)
				err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
					if string(rec.Key) == HotMerchant {
						Work(ctx, 30, 0.5) // the merchant's rows are contended
					}
					rows, err := tx.Query(ctx, `SELECT id, status FROM orders WHERE id = ANY($1) FOR UPDATE`, []int64{int64(id)})
					if err != nil {
						return err
					}
					rows.Close()
					_, err = tx.Exec(ctx, `UPDATE orders SET status = $1, updated_at = now() WHERE id = $2`, "fulfilling", id)
					return err
				})
				if err == nil && Chance(0.006) {
					err = errors.New("lock_timeout: canceling statement due to lock timeout")
				}
				if err != nil {
					return retry(ctx, rec, err)
				}
				_ = cache.Del(ctx, fmt.Sprintf("order:%d", id)).Err()
				a.Produce(ctx, kc, "orders.status-changed", strconv.Itoa(id), fmt.Appendf(nil, `{"order_id":%d,"status":"fulfilling"}`, id))
				Work(ctx, 6, 0.5)
				if Chance(0.1) {
					a.Log.InfoContext(ctx, fmt.Sprintf("processed message topic=orders.created partition=%d offset=%d", rec.Partition, rec.Offset))
				}
				return nil
			}},
		{group: world.GroupSearchIndex, process: "order-search-indexer", topics: []string{"orders.created"},
			handle: func(ctx context.Context, rec *kgo.Record) error {
				Work(ctx, 6, 0.4)
				return cache.SetEx(ctx, fmt.Sprintf("search:order:%d", rec.Offset), "1", 10*time.Minute).Err()
			}},
		{group: world.GroupProjections, process: "order-status-projector", topics: []string{"orders.status-changed"},
			handle: func(ctx context.Context, rec *kgo.Record) error {
				_, err := db.Exec(ctx, `UPDATE orders SET status = $1, updated_at = now() WHERE id = $2`, "projected", ID(1, 200000))
				Work(ctx, 3, 0.4)
				return err
			}},
		{group: world.GroupDLQReplay, process: "dlq-replayer", topics: []string{"orders.created.dlq"},
			handle: func(ctx context.Context, rec *kgo.Record) error {
				Work(ctx, 25, 0.5)
				if Chance(0.03) {
					err := errors.New("replay failed: order already cancelled")
					trooper.CaptureError(ctx, err)
					return err
				}
				return nil
			}},
	}
	var wg sync.WaitGroup
	for _, c := range consumers {
		c.a = a
		if c.group == world.GroupFulfilment {
			c.throttle = func(p int32) time.Duration {
				if !a.RT.Active("fulfilment-hot-partition") {
					return 0
				}
				// one member's capacity is ~60% of what the hottest partition receives
				max := 0.0
				c.inflow.Range(func(_, v any) bool { max = maxf(max, v.(*rateMeter).get()); return true })
				if max <= 0 {
					return 0
				}
				return time.Duration(float64(time.Second) / (0.6 * max))
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if err := c.run(ctx); err != nil && ctx.Err() == nil {
					a.Log.Error("consumer stopped, restarting", "group", c.group, "err", err.Error())
					time.Sleep(2 * time.Second)
				}
			}
		}()
	}
	wg.Wait()
	return ctx.Err()
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------- email-worker (RabbitMQ + MySQL)

func runEmailWorker(ctx context.Context, a *App, _ int) error {
	mdb, err := troopersql.Open("mysql", world.MySQLDSN("notifications"), troopersql.WithNamespace("notifications-db"), troopersql.WithPeer("notifications-db"))
	if err != nil {
		return err
	}
	mdb.SetMaxOpenConns(8)
	a.onClose(func() { _ = mdb.Close() })
	cache := a.Redis("cache-main")

	conn, err := amqp.Dial(world.RabbitURL())
	if err != nil {
		return err
	}
	a.onClose(func() { _ = conn.Close() })
	pub, err := conn.Channel()
	if err != nil {
		return err
	}
	if err := infra.DeclareRabbit(pub); err != nil {
		return err
	}
	var pubMu sync.Mutex
	logEmail := func(ctx context.Context, template, status string) error {
		_, err := mdb.ExecContext(ctx, `INSERT INTO email_log (message_id, template, recipient_hash, status, sent_at) VALUES (?, ?, ?, ?, ?)`,
			HexID(16), template, HexID(32), status, time.Now().UTC())
		return err
	}
	send := func(ctx context.Context, template string, smtpMs float64, failP float64, d amqp.Delivery) error {
		var unsub int
		_ = mdb.QueryRowContext(ctx, `SELECT count(*) FROM unsubscribes WHERE recipient_hash = ?`, HexID(32)).Scan(&unsub)
		_ = cache.Get(ctx, "tpl:"+template).Err()
		Work(ctx, smtpMs, 0.45) // SMTP round trip
		if Chance(failP) {
			err := errors.New("SMTP 421 service not available, will retry")
			a.Log.ErrorContext(ctx, err.Error(), "template", template)
			_ = logEmail(ctx, template, "deferred")
			if d.Redelivered {
				trooper.MarkDLQ(ctx)
				_ = d.Nack(false, false)
			} else {
				_ = d.Nack(false, true)
			}
			return err
		}
		if err := logEmail(ctx, template, "sent"); err != nil {
			_ = d.Nack(false, true)
			return err
		}
		pubMu.Lock()
		err := trooperamqp.Publish(ctx, pub, world.ExchangeEmailEvents, "", false, false,
			amqp.Publishing{ContentType: "application/json", Body: fmt.Appendf(nil, `{"type":"email.sent","template":%q}`, template)})
		pubMu.Unlock()
		if err != nil {
			a.Log.WarnContext(ctx, "publish email.events failed", "err", err.Error())
		}
		return d.Ack(false)
	}
	consume := func(queue, process string, prefetch int, smtpMs, failP float64) error {
		ch, err := conn.Channel()
		if err != nil {
			return err
		}
		if err := ch.Qos(prefetch, 0, false); err != nil {
			return err
		}
		ds, err := ch.Consume(queue, fmt.Sprintf("%s-%d", process, a.Instance), false, false, false, false, nil)
		if err != nil {
			return err
		}
		sem := make(chan struct{}, prefetch)
		for {
			select {
			case <-ctx.Done():
				_ = ch.Close()
				return ctx.Err()
			case d, ok := <-ds:
				if !ok {
					return errors.New("delivery channel closed")
				}
				sem <- struct{}{}
				go func() {
					defer func() { <-sem }()
					_ = trooperamqp.ProcessDelivery(context.Background(), d, func(ctx context.Context) error {
						return send(ctx, process, smtpMs, failP, d)
					}, trooperamqp.WithQueue(queue), trooperamqp.WithProcess(process))
				}()
			}
		}
	}
	errc := make(chan error, 3)
	go func() { errc <- consume(world.QueueTransactional, "email-sender", 8, 450, 0.0085) }()
	go func() { errc <- consume(world.QueueMarketing, "newsletter-sender", 2, 900, 0.012) }()
	if a.Instance == 0 {
		go func() {
			// invoice-batch: "0 2 * * *" in production; every 5 minutes in the demo
			Every(ctx, 5*time.Minute, func() { invoiceBatch(a, mdb) })
		}()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errc:
		return err
	}
}

func invoiceBatch(a *App, mdb *sql.DB) {
	m := trooper.Message{System: "cron", Destination: "send_invoices", Process: "invoice-batch", Partition: -1}
	_ = trooper.ProcessMessage(context.Background(), propagation.MapCarrier{}, m, trooper.PanicRecover, func(ctx context.Context) error {
		n := ID(20, 60)
		for i := 0; i < n; i++ {
			if _, err := mdb.ExecContext(ctx, `INSERT INTO invoices (customer_id, amount, status, created_at) VALUES (?, ?, ?, ?)`,
				ID(1, 50000), float64(ID(1000, 90000))/100, "sent", time.Now().UTC()); err != nil {
				return err
			}
			Work(ctx, 40, 0.3)
		}
		_, _ = mdb.ExecContext(ctx, `UPDATE invoices SET status = ? WHERE status = ? LIMIT 1000`, "archived", "sent")
		a.Log.InfoContext(ctx, fmt.Sprintf("invoice batch finished invoices=%d", n))
		return nil
	})
}
