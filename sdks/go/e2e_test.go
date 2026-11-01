//go:build e2e

package trooper_test

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/makifbaysal/tasktrooper-telemetry/sdks/go"
)

// TestE2EIngestGateway sends real telemetry to a locally running ingest-gw.
// Run: TROOPER_E2E_DSN=http://<ingest-key>@localhost:4318/<project-id> go test -tags e2e -run E2E ./...
// (DSN from onboarding or `api bootstrap`; no seeded key exists, ADR-028).
func TestE2EIngestGateway(t *testing.T) {
	dsn := os.Getenv("TROOPER_E2E_DSN")
	if dsn == "" {
		t.Skip("TROOPER_E2E_DSN not set")
	}
	d, err := trooper.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(d.Endpoint + "/healthz")
	if err != nil {
		t.Skipf("ingest-gw not reachable at %s: %v", d.Endpoint, err)
	}
	resp.Body.Close()

	before := trooper.DroppedBatches()
	shutdown, err := trooper.Start(context.Background(), trooper.Config{DSN: dsn, Service: "trooper-go-e2e", Environment: "dev", Version: trooper.Version})
	if err != nil {
		t.Fatal(err)
	}
	ctx, srv := otel.Tracer("e2e").Start(context.Background(), "GET /e2e/:id", trace.WithSpanKind(trace.SpanKindServer))
	ctx, entry := trooper.BeginEntry(ctx, "GET", "/e2e/:id", nil)
	_, db := otel.Tracer("e2e").Start(ctx, "SELECT", trace.WithSpanKind(trace.SpanKindClient))
	db.End()
	entry.Finish("")
	srv.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if got := trooper.DroppedBatches(); got != before {
		t.Fatalf("ingest-gw rejected %d batches", got-before)
	}
}
