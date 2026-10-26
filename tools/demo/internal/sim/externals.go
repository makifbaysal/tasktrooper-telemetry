// Package sim runs everything outside the instrumented services: third-party APIs (inventory-api,
// iyzico, Stripe), uninstrumented consumers (ledger-sync, reco-trainer, Kafka Connect sinks,
// crm-sync), the marketing campaign publisher, the webhook sink for notifications, and the client
// traffic (RUM sessions of the web / mobile apps + partner, webhook, probe and unknown callers).
package sim

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tasktrooper/telemetry/tools/demo/internal/world"
)

// lognormal draws around median.
func lognormal(median, sigma float64) float64 {
	return median * expf(rand.NormFloat64()*sigma)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Listen serves h on a random local port and returns its base URL.
func Listen(ctx context.Context, h http.Handler) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	go func() { <-ctx.Done(); _ = srv.Close() }()
	return "http://" + ln.Addr().String(), nil
}

// Externals starts the third-party HTTP APIs the services call (no SDK: they are not ours).
func Externals(ctx context.Context, rt *world.Runtime) (map[string]string, error) {
	out := map[string]string{}
	delay := func(r *http.Request, median, sigma float64) bool {
		return sleepCtx(r.Context(), time.Duration(lognormal(median, sigma)*float64(time.Millisecond)))
	}
	inv := http.NewServeMux()
	inv.HandleFunc("POST /v1/reservations", func(w http.ResponseWriter, r *http.Request) {
		med := 35.0
		if rand.Float64() < 0.03 {
			med = 700 // lock contention on popular SKUs: "inventory reservation slow"
		}
		if !delay(r, med, 0.5) {
			return
		}
		switch x := rand.Float64(); {
		case x < 0.002:
			http.Error(w, "upstream error", 503)
		case x < 0.01:
			w.WriteHeader(409)
		default:
			w.WriteHeader(201)
		}
	})
	inv.HandleFunc("GET /v1/stock/{sku}", func(w http.ResponseWriter, r *http.Request) {
		if delay(r, 20, 0.4) {
			_, _ = io.WriteString(w, `{"available":12}`)
		}
	})
	iy := http.NewServeMux()
	iy.HandleFunc("POST /payment/{op}", func(w http.ResponseWriter, r *http.Request) {
		bad := rt.Active("payments-provider-errors")
		med, sig := 160.0, 0.5
		if bad {
			med, sig = 380, 0.6
		}
		x := rand.Float64()
		if bad && x < 0.012 {
			sleepCtx(r.Context(), 7*time.Second) // client gives up at 6 s: "provider timeout"
			return
		}
		if !delay(r, med, sig) {
			return
		}
		switch {
		case bad && x < 0.035:
			http.Error(w, `{"status":"failure","errorCode":"10051"}`, 502)
		case x < 0.003:
			http.Error(w, `{"status":"failure"}`, 500)
		case x < 0.02:
			w.WriteHeader(402)
		default:
			_, _ = io.WriteString(w, `{"status":"success"}`)
		}
	})
	st := http.NewServeMux()
	st.HandleFunc("GET /v1/events/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !delay(r, 110, 0.5) {
			return
		}
		if rand.Float64() < 0.004 {
			http.Error(w, "rate limited", 503)
			return
		}
		_, _ = io.WriteString(w, `{"type":"charge.succeeded"}`)
	})
	for name, h := range map[string]http.Handler{"inventory-api": inv, "iyzico": iy, "stripe": st} {
		u, err := Listen(ctx, h)
		if err != nil {
			return nil, err
		}
		out[name] = u
	}
	return out, nil
}

// ---------------------------------------------------------------- uninstrumented consumers

// ExternalConsumers runs the consumer groups that have no TaskTrooper SDK (lag comes from the broker only).
func ExternalConsumers(ctx context.Context, log *slog.Logger, rt *world.Runtime, recoCycle time.Duration) {
	// plain is one group membership: it lasts `life` (0 = until ctx ends), then the member leaves.
	plain := func(group, member string, topics []string, perRecord time.Duration, life time.Duration) {
		cctx, cancel := context.WithCancel(ctx)
		if life > 0 {
			cctx, cancel = context.WithTimeout(ctx, life)
		}
		defer cancel()
		cl, err := kgo.NewClient(kgo.SeedBrokers(rt.Kafka...), kgo.ConsumerGroup(group), kgo.ConsumeTopics(topics...),
			kgo.ClientID(member), kgo.ConsumeResetOffset(kgo.NewOffset().AtEnd()),
			kgo.SessionTimeout(10*time.Second), kgo.RebalanceTimeout(10*time.Second))
		if err != nil {
			log.Warn("external consumer", "group", group, "err", err)
			sleepCtx(ctx, 5*time.Second)
			return
		}
		for cctx.Err() == nil {
			fs := cl.PollRecords(cctx, 200)
			fs.EachRecord(func(*kgo.Record) {
				if perRecord > 0 {
					sleepCtx(cctx, perRecord)
				}
			})
		}
		cl.Close()
		sleepCtx(ctx, 3*time.Second)
	}
	forever := func(group, member string, topics []string) {
		for ctx.Err() == nil {
			plain(group, member, topics, 0, 0)
		}
	}
	go forever(world.GroupS3Sink, "connect-s3-sink-0", []string{"orders.created", "payments.events"})
	go forever(world.GroupAudit, "connect-opensearch-0", []string{"auth.events"})
	// ledger-sync: two members; a member leaves when a batch exceeds max.poll.interval and rejoins,
	// so the group rebalances all the time and falls behind on payments.events.
	for i := 0; i < 2; i++ {
		go func() {
			sleepCtx(ctx, time.Duration(i)*90*time.Second)
			for ctx.Err() == nil {
				if rt.Active("ledger-sync-rebalancing") {
					plain(world.GroupLedger, fmt.Sprintf("ledger-service-%d", i), []string{"payments.events"}, 900*time.Millisecond, 3*time.Minute)
				} else {
					// healthy: stable membership (re-checks the scenario every 30 min)
					plain(world.GroupLedger, fmt.Sprintf("ledger-service-%d", i), []string{"payments.events"}, 0, 30*time.Minute)
				}
			}
		}()
	}
	// reco-trainer: Spark batch — starts every cycle, reads everything since its last commit, leaves.
	go func() {
		for ctx.Err() == nil {
			if !sleepCtx(ctx, recoCycle) {
				return
			}
			cl, err := kgo.NewClient(kgo.SeedBrokers(rt.Kafka...), kgo.ConsumerGroup(world.GroupRecoTrainer), kgo.ConsumeTopics("reco.events"),
				kgo.ClientID("reco-trainer-spark"), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
			if err != nil {
				continue
			}
			n := 0
			for idle := 0; idle < 3 && ctx.Err() == nil; {
				c, cancel := context.WithTimeout(ctx, 3*time.Second)
				fs := cl.PollRecords(c, 5000)
				cancel()
				if fs.NumRecords() == 0 {
					idle++
				}
				n += fs.NumRecords()
			}
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = cl.CommitUncommittedOffsets(c)
			cancel()
			cl.Close()
			log.Info("reco-trainer batch done", "records", n)
		}
	}()
}

// RabbitExternal runs crm-sync (reads email.events, not instrumented) and the backoffice campaign
// publisher (emails.marketing).
func RabbitExternal(ctx context.Context, log *slog.Logger, rpsScale float64) {
	go func() {
		for ctx.Err() == nil {
			conn, err := amqp.Dial(world.RabbitURL())
			if err != nil {
				sleepCtx(ctx, 3*time.Second)
				continue
			}
			ch, _ := conn.Channel()
			_ = ch.Qos(20, 0, false)
			for i := 0; i < 2; i++ {
				ds, err := ch.Consume(world.QueueEmailEvents, fmt.Sprintf("crm-sync-%d", i+1), false, false, false, false, nil)
				if err != nil {
					break
				}
				go func() {
					for d := range ds {
						sleepCtx(ctx, time.Duration(lognormal(120, 0.4)*float64(time.Millisecond)))
						_ = d.Ack(false)
					}
				}()
			}
			pub, _ := conn.Channel()
			closed := conn.NotifyClose(make(chan *amqp.Error, 1))
			tick := time.NewTicker(time.Second)
		loop:
			for {
				select {
				case <-ctx.Done():
					break loop
				case <-closed:
					break loop
				case <-tick.C:
					// marketing campaigns: ~1.7 msg/s at mock scale, bursty
					n := poisson(0.06 * rpsScale)
					for j := 0; j < n; j++ {
						_ = pub.PublishWithContext(ctx, world.ExchangeEmails, "email.marketing.campaign", false, false,
							amqp.Publishing{ContentType: "application/json", Body: []byte(`{"template":"newsletter"}`), Timestamp: time.Now()})
					}
				}
			}
			tick.Stop()
			conn.Close()
		}
	}()
}

// ---------------------------------------------------------------- webhook sink

// WebhookSink receives alert notifications (notification channels of type webhook) and logs them.
func WebhookSink(ctx context.Context, log *slog.Logger, file string) (*atomic.Int64, error) {
	var n atomic.Int64
	f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hook/{channel...}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		n.Add(1)
		ch := r.PathValue("channel")
		fmt.Fprintf(f, "%s channel=%s %s\n", time.Now().Format(time.RFC3339), ch, strings.TrimSpace(string(b)))
		log.Info("notification received", "channel", ch, "bytes", len(b))
		w.WriteHeader(204)
	})
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", world.WebhookSinkPort))
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("webhook sink :%d: %w", world.WebhookSinkPort, err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	go func() { <-ctx.Done(); _ = srv.Close(); f.Close() }()
	return &n, nil
}
