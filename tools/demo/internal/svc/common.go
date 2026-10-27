// Package svc runs one simulated backend service per OS process (`demo svc <name>`), instrumented
// only with the real TaskTrooper Go SDK and its integrations: net/http server + client, franz-go,
// amqp091, pgx, go-redis, database/sql (MySQL) and the slog bridge. One process per service because
// the SDK installs global OTel providers with one service.name.
package svc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"

	trooper "github.com/tasktrooper/telemetry-go"
	trooperfranz "github.com/tasktrooper/telemetry-go/integrations/franzgo"
	trooperredis "github.com/tasktrooper/telemetry-go/integrations/goredis"
	trooperhttp "github.com/tasktrooper/telemetry-go/integrations/nethttp"
	trooperpgx "github.com/tasktrooper/telemetry-go/integrations/pgx"
	trooperslog "github.com/tasktrooper/telemetry-go/integrations/slog"
	"github.com/tasktrooper/telemetry-go/llm"

	"github.com/tasktrooper/telemetry/tools/demo/internal/world"
)

// App is the shared runtime of one service process.
type App struct {
	Name     string
	Instance int
	RT       world.Runtime
	Log      *slog.Logger

	mu      sync.Mutex
	clients map[string]*http.Client
	closers []func()
}

// Run starts the named service and blocks until ctx is done. port is the HTTP listen port (HTTP services).
func Run(ctx context.Context, name string, instance, port int) error {
	rt, err := world.LoadRuntime()
	if err != nil {
		return err
	}
	def, ok := world.ServiceByName(name)
	if !ok {
		return fmt.Errorf("unknown service %q", name)
	}
	var topts []trooper.Option
	if rt.Profiling {
		topts = append(topts, trooper.WithProfiling())
	}
	if rt.LLM {
		// prompts / completions are recorded only for the support assistant (fake, PII-free messages)
		llm.SetCaptureContent(name == "storefront-bff")
	}
	shutdown, err := trooper.Start(ctx, trooper.Config{
		DSN:            rt.DSN,
		Service:        name,
		Environment:    world.Environment,
		Version:        def.Version,
		Debug:          rt.Debug,
		MetricInterval: 15 * time.Second,
		BatchTimeout:   2 * time.Second,
		MaxQueueSize:   20000,
		ResourceAttributes: map[string]string{
			"service.instance.id": fmt.Sprintf("%s-%d", name, instance),
			"k8s.pod.name":        fmt.Sprintf("%s-%x-%d", name, os.Getpid()&0xfffff, instance),
		},
	}, topts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "telemetry disabled: %v\n", err)
	}
	a := &App{
		Name: name, Instance: instance, RT: rt, clients: map[string]*http.Client{},
		Log: slog.New(trooperslog.NewHandler(name, slog.NewJSONHandler(os.Stdout, nil), trooperslog.WithLevel(slog.LevelDebug))).
			With("instance", instance),
	}
	defer func() {
		for i := len(a.closers) - 1; i >= 0; i-- {
			a.closers[i]()
		}
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = shutdown(c)
	}()

	var run func(context.Context, *App, int) error
	switch name {
	case "orders-api":
		run = runOrders
	case "payments-api":
		run = runPayments
	case "catalog-api":
		run = runCatalog
	case "auth-api":
		run = runAuth
	case "storefront-bff":
		run = runBFF
	case "recommendation-api":
		run = runReco
	case "order-events-consumer":
		run = runOrderEvents
	case "email-worker":
		run = runEmailWorker
	default:
		return fmt.Errorf("no implementation for %q", name)
	}
	a.Log.Info("service starting", "version", def.Version, "port", port)
	err = run(ctx, a, port)
	if errors.Is(err, context.Canceled) || errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	a.Log.Info("service stopped")
	return err
}

func (a *App) onClose(f func()) { a.closers = append(a.closers, f) }

// ---------------------------------------------------------------- HTTP

// Serve serves mux (wrapped by the SDK's net/http middleware) until ctx ends.
func (a *App) Serve(ctx context.Context, port int, mux *http.ServeMux) error {
	srv := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", port), Handler: trooperhttp.Handler(mux, trooperhttp.WithPanicPolicy(trooper.PanicRecover)),
		ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
	}()
	return srv.ListenAndServe()
}

// Client returns an instrumented client for a peer (dependency name "HTTP · <peer>").
func (a *App) Client(peer string) *http.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c := a.clients[peer]; c != nil {
		return c
	}
	base := &http.Transport{MaxIdleConnsPerHost: 64, IdleConnTimeout: 60 * time.Second}
	c := &http.Client{Transport: trooperhttp.NewTransport(base, trooperhttp.WithPeerService(peer)), Timeout: 6 * time.Second}
	a.clients[peer] = c
	return c
}

// Call does an HTTP request to a peer and returns the status code.
func (a *App) Call(ctx context.Context, peer, method, path string, body any) (int, error) {
	base := a.RT.Peers[peer]
	if base == "" {
		return 0, fmt.Errorf("no address for %s", peer)
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(fmt.Appendf(nil, "%v", body))
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.Client(peer).Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return resp.StatusCode, fmt.Errorf("%s %s %s: %d", peer, method, path, resp.StatusCode)
	}
	return resp.StatusCode, nil
}

// Fail records err on the request span, logs it and answers with status.
func (a *App) Fail(w http.ResponseWriter, r *http.Request, status int, err error, msg string, args ...any) {
	trooper.CaptureError(r.Context(), err)
	a.Log.ErrorContext(r.Context(), msg, args...)
	http.Error(w, err.Error(), status)
}

// JSON answers 200 with a small JSON body.
func JSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// ---------------------------------------------------------------- data stores

// PG returns an instrumented pgx pool for a demo database.
func (a *App) PG(ctx context.Context, id string, maxConns, minConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(world.PGDSN(id, ""))
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.Tracer = trooperpgx.NewTracer()
	cfg.ConnConfig.RuntimeParams["application_name"] = fmt.Sprintf("%s-%d", a.Name, a.Instance)
	cfg.MaxConns, cfg.MinConns = maxConns, minConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	a.onClose(pool.Close)
	return pool, nil
}

// Redis returns an instrumented client. The address host is the cache name (cache-main:6379) so the
// dependency is "redis · cache-main"; a custom dialer reaches the real container.
func (a *App) Redis(id string) *redis.Client {
	real := world.RedisAddr(id)
	cl := redis.NewClient(&redis.Options{
		Addr: id + ":6379",
		Dialer: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, real)
		},
		PoolSize: 32,
	})
	if err := trooperredis.Instrument(cl, trooperredis.WithoutMetrics()); err != nil {
		a.Log.Warn("redis instrumentation", "err", err)
	}
	a.onClose(func() { _ = cl.Close() })
	return cl
}

// Producer returns a franz-go client with the SDK's producer hooks.
func (a *App) Producer() (*kgo.Client, error) {
	tt := trooperfranz.New()
	cl, err := kgo.NewClient(kgo.SeedBrokers(a.RT.Kafka...), kgo.WithHooks(tt.Hooks()...), kgo.ProducerLinger(5*time.Millisecond),
		kgo.ClientID(a.Name), kgo.RecordPartitioner(kgo.StickyKeyPartitioner(nil)))
	if err != nil {
		return nil, err
	}
	a.onClose(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = cl.Flush(c)
		cl.Close()
	})
	return cl, nil
}

// Produce publishes asynchronously; the producer span ends when the broker acks.
func (a *App) Produce(ctx context.Context, cl *kgo.Client, topic, key string, value []byte, headers ...kgo.RecordHeader) {
	rec := &kgo.Record{Topic: topic, Value: value, Headers: headers}
	if key != "" {
		rec.Key = []byte(key)
	}
	// the request may finish before the broker acks: keep the span context, drop the cancellation
	cl.Produce(context.WithoutCancel(ctx), rec, func(_ *kgo.Record, err error) {
		if err != nil && !errors.Is(err, context.Canceled) {
			a.Log.Warn("produce failed", "topic", topic, "err", err.Error())
		}
	})
}

// ---------------------------------------------------------------- timing / randomness

// Work simulates own code time: log-normal around median ms.
func Work(ctx context.Context, medianMs, sigma float64) {
	d := time.Duration(Lognormal(medianMs, sigma) * float64(time.Millisecond))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Lognormal draws from a log-normal distribution with the given median.
func Lognormal(median, sigma float64) float64 {
	return median * math.Exp(rand.NormFloat64()*sigma)
}

// Chance returns true with probability p.
func Chance(p float64) bool { return rand.Float64() < p }

// ID returns a random id in [lo, hi].
func ID(lo, hi int) int { return lo + rand.IntN(hi-lo+1) }

// Pick returns a random element.
func Pick[T any](xs []T) T { return xs[rand.IntN(len(xs))] }

// HexID returns n random hex characters.
func HexID(n int) string {
	const hx = "0123456789abcdef"
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(hx[rand.IntN(16)])
	}
	return b.String()
}

// Every runs f every d (with ±20% jitter) until ctx ends.
func Every(ctx context.Context, d time.Duration, f func()) {
	for {
		j := time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
		select {
		case <-ctx.Done():
			return
		case <-time.After(j):
			f()
		}
	}
}
