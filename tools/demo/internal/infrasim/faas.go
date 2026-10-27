package infrasim

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tasktrooper/telemetry/tools/demo/internal/world"
)

// Function is a simulated AWS Lambda function (contracts §21).
type Function struct {
	Name, Runtime, Arch, Service string
	MemoryMB, TimeoutS           int
	Triggers                     []string
	perMin                       float64 // invocations / minute at factor 1
	avgMs, p95Ms, p99Ms, maxMs   float64
	errRate                      float64
	// scenario couples the function to a demo incident
	scenario    string
	scenErrRate float64
	scenLatency float64
	concLimit   float64 // reserved concurrency (throttles above it); 0 = none
	iterator    bool    // stream trigger → iterator age
}

// Slug is trooper.faas.id (aws-<region>-<name>).
func (f Function) Slug() string { return "aws-" + Region + "-" + f.Name }

type faasWorld struct {
	fns []Function
	rng *rand.Rand
}

func newFaasWorld() *faasWorld {
	return &faasWorld{rng: rand.New(rand.NewPCG(11, 3)), fns: []Function{
		{Name: "image-resizer", Runtime: "nodejs20.x", Arch: "arm64", Service: "image-resizer", MemoryMB: 1024, TimeoutS: 30,
			Triggers: []string{"api"}, perMin: 140, avgMs: 180, p95Ms: 420, p99Ms: 780, maxMs: 1900, errRate: 0.002},
		{Name: "payment-webhook-handler", Runtime: "nodejs20.x", Arch: "x86_64", Service: "payment-webhook-handler", MemoryMB: 512, TimeoutS: 10,
			Triggers: []string{"api"}, perMin: 45, avgMs: 95, p95Ms: 210, p99Ms: 380, maxMs: 900, errRate: 0.001,
			scenario: "payments-provider-errors", scenErrRate: 0.07, scenLatency: 3.2},
		{Name: "invoice-pdf-generator", Runtime: "python3.12", Arch: "arm64", Service: "invoice-pdf-generator", MemoryMB: 2048, TimeoutS: 60,
			Triggers: []string{"sqs:invoices"}, perMin: 22, avgMs: 1450, p95Ms: 3100, p99Ms: 4800, maxMs: 9000, errRate: 0.004, concLimit: 0.55},
		{Name: "catalog-search-indexer", Runtime: "python3.12", Arch: "arm64", Service: "catalog-search-indexer", MemoryMB: 512, TimeoutS: 120,
			Triggers: []string{"kinesis:catalog-events"}, perMin: 12, avgMs: 640, p95Ms: 1300, p99Ms: 2100, maxMs: 3800, errRate: 0.0, iterator: true},
	}}
}

// batch is one provider minute of AWS/Lambda metrics (resource service.name=trooper-cloud).
func (fw *faasWorld) batch(minute time.Time, rt *world.Runtime) *Batch {
	b := NewBatch([]KV{{"service.name", "trooper-cloud"}, {"cloud.provider", "aws"}, {"cloud.region", Region}}, minute)
	f := factor(minute)
	for _, fn := range fw.fns {
		a := []KV{{"trooper.faas.id", fn.Slug()}, {"faas.name", fn.Name}}
		inv := math.Round(fn.perMin * f * (1 + (fw.rng.Float64()*2-1)*0.15))
		errRate, lat := fn.errRate, 1.0
		if fn.scenario != "" && rt != nil && rt.Active(fn.scenario) {
			errRate, lat = fn.scenErrRate, fn.scenLatency
		}
		errs := math.Round(inv * errRate * (0.6 + fw.rng.Float64()*0.8))
		jit := 1 + (fw.rng.Float64()*2-1)*0.1
		avg := fn.avgMs * lat * jit
		conc := math.Max(1, math.Ceil(inv*avg/60000*1.8))
		throttles := 0.0
		if fn.concLimit > 0 && f > 1.05 { // afternoon peak hits the reserved concurrency
			throttles = math.Round(inv * 0.03 * fw.rng.Float64())
		}
		b.Gauge("trooper.faas.invocations", inv, a...)
		b.Gauge("trooper.faas.errors", errs, a...)
		b.Gauge("trooper.faas.throttles", throttles, a...)
		b.Gauge("trooper.faas.duration.avg", avg, a...)
		b.Gauge("trooper.faas.duration.p95", fn.p95Ms*lat*jit, a...)
		b.Gauge("trooper.faas.duration.p99", fn.p99Ms*lat*jit, a...)
		b.Gauge("trooper.faas.duration.max", fn.maxMs*lat*jit, a...)
		b.Gauge("trooper.faas.concurrency", conc, a...)
		if fn.iterator {
			b.Gauge("trooper.faas.iterator_age_ms", 300+fw.rng.Float64()*900, a...)
		}
	}
	return b
}

// UpsertFunctions writes the functions into serverless_functions (project 1) when the table
// exists (migration 011). Returns -1 when it does not.
func UpsertFunctions(ctx context.Context, postgresURL string, fns []Function) (int, error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := pgx.Connect(cctx, postgresURL)
	if err != nil {
		return 0, err
	}
	defer conn.Close(context.Background())
	var exists bool
	if err := conn.QueryRow(cctx, `SELECT to_regclass('public.serverless_functions') IS NOT NULL`).Scan(&exists); err != nil {
		return 0, err
	}
	if !exists {
		return -1, nil
	}
	n := 0
	for _, f := range fns {
		meta, _ := json.Marshal(map[string]any{
			"arn":  fmt.Sprintf("arn:aws:lambda:%s:123456789012:function:%s", Region, f.Name),
			"tags": map[string]string{"tasktrooper.service": f.Service, "env": world.Environment},
		})
		if _, err := conn.Exec(cctx, `INSERT INTO serverless_functions
		    (project_id, provider, slug, name, region, runtime, memory_mb, timeout_s, arch, triggers, service, meta, last_seen_at, missing)
		  VALUES (1, 'aws', $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now(), false)
		  ON CONFLICT (project_id, slug) DO UPDATE SET name = EXCLUDED.name, region = EXCLUDED.region, runtime = EXCLUDED.runtime,
		    memory_mb = EXCLUDED.memory_mb, timeout_s = EXCLUDED.timeout_s, arch = EXCLUDED.arch, triggers = EXCLUDED.triggers,
		    service = EXCLUDED.service, meta = EXCLUDED.meta, last_seen_at = now(), missing = false`,
			f.Slug(), f.Name, Region, f.Runtime, f.MemoryMB, f.TimeoutS, f.Arch, f.Triggers, f.Service, meta); err != nil {
			return n, fmt.Errorf("%s: %w", f.Slug(), err)
		}
		n++
	}
	return n, nil
}
