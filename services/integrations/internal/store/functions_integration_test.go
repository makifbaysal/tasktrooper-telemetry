//go:build integration

package store

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/integrations/internal/factory"
	"github.com/tasktrooper/telemetry/services/integrations/internal/fakes"
	"github.com/tasktrooper/telemetry/services/integrations/internal/model"
	"github.com/tasktrooper/telemetry/services/integrations/internal/otlp"
	"github.com/tasktrooper/telemetry/services/integrations/internal/runner"
)

// Uses project 9701 (created and removed by the test); needs deploy/postgres/011_serverless.sql.
func TestSyncFunctions(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	const project = 9701
	_, _ = s.DB.Exec(ctx, `DELETE FROM projects WHERE id = $1`, project)
	if _, err := s.DB.Exec(ctx, `INSERT INTO projects (id, org_id, name, slug) VALUES ($1, 1, 'faas-it', 'faas-it-9701')`, project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.DB.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, project) })
	in := model.Integration{ProjectID: project, Provider: "azure", Name: "it-azure"}
	if err := s.DB.QueryRow(ctx, `INSERT INTO integrations (project_id, provider, name, config) VALUES ($1, 'azure', $2, '{}') RETURNING id`,
		project, in.Name).Scan(&in.ID); err != nil {
		t.Fatal(err)
	}
	// A row added by hand (integration_id NULL) with a slug discovery also produces.
	if _, err := s.DB.Exec(ctx, `INSERT INTO serverless_functions (project_id, provider, slug, name, service) VALUES ($1, 'azure', 'azure-manual', 'manual', 'm')`, project); err != nil {
		t.Fatal(err)
	}
	mem := 2048
	fns := []model.Function{
		{Slug: "azure-orders", Name: "orders", App: "orders", Region: "westeurope", Runtime: "python 3.11", Triggers: []string{"http", "schedule"},
			Service: "orders-svc", URL: "https://orders.azurewebsites.net", Meta: map[string]any{"plan": "Consumption", "functions": []any{map[string]any{"name": "a"}}}},
		{Slug: "azure-billing", Name: "billing", App: "billing", MemoryMB: &mem},
		{Slug: "azure-manual", Name: "manual"},
	}
	slugs, err := s.SyncFunctions(ctx, in, fns)
	if err != nil {
		t.Fatal(err)
	}
	if len(slugs) != 2 || slugs[0] != "azure-orders" || slugs[1] != "azure-billing" {
		t.Fatalf("owned %v", slugs)
	}
	var triggers []string
	var service, plan string
	var memMB *int
	if err := s.DB.QueryRow(ctx, `SELECT triggers, service, meta->>'plan', memory_mb FROM serverless_functions WHERE project_id=$1 AND slug='azure-orders'`, project).
		Scan(&triggers, &service, &plan, &memMB); err != nil {
		t.Fatal(err)
	}
	if len(triggers) != 2 || service != "orders-svc" || plan != "Consumption" || memMB != nil {
		t.Errorf("row %v %q %q %v", triggers, service, plan, memMB)
	}
	var billingService string
	_ = s.DB.QueryRow(ctx, `SELECT service FROM serverless_functions WHERE project_id=$1 AND slug='azure-billing'`, project).Scan(&billingService)
	if billingService != "billing" {
		t.Errorf("service defaults to name: %q", billingService)
	}
	var manualIID *int64
	_ = s.DB.QueryRow(ctx, `SELECT integration_id FROM serverless_functions WHERE project_id=$1 AND slug='azure-manual'`, project).Scan(&manualIID)
	if manualIID != nil {
		t.Error("manual row was taken over")
	}
	// billing disappears → missing; reappears → cleared.
	if _, err := s.SyncFunctions(ctx, in, fns[:1]); err != nil {
		t.Fatal(err)
	}
	var missing bool
	_ = s.DB.QueryRow(ctx, `SELECT missing FROM serverless_functions WHERE project_id=$1 AND slug='azure-billing'`, project).Scan(&missing)
	if !missing {
		t.Error("billing should be missing")
	}
	if _, err := s.SyncFunctions(ctx, in, fns); err != nil {
		t.Fatal(err)
	}
	_ = s.DB.QueryRow(ctx, `SELECT missing FROM serverless_functions WHERE project_id=$1 AND slug='azure-billing'`, project).Scan(&missing)
	if missing {
		t.Error("billing should be back")
	}
	var manualMissing bool
	_ = s.DB.QueryRow(ctx, `SELECT missing FROM serverless_functions WHERE project_id=$1 AND slug='azure-manual'`, project).Scan(&manualMissing)
	if manualMissing {
		t.Error("manual row must never be touched")
	}
}

type captureSender struct{ samples []model.Sample }

func (c *captureSender) Send(_ context.Context, _ otlp.Target, s []model.Sample) error {
	c.samples = append(c.samples, s...)
	return nil
}

// Fake AWS (Lambda only) + fake Azure → factory → runner → real Postgres (project 9701).
func TestServerlessRunOnce(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	const project = 9701
	_, _ = s.DB.Exec(ctx, `DELETE FROM projects WHERE id = $1`, project)
	if _, err := s.DB.Exec(ctx, `INSERT INTO projects (id, org_id, name, slug) VALUES ($1, 1, 'faas-it', 'faas-it-9701')`, project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.DB.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, project) })
	ts := time.Now().UTC().Truncate(time.Minute).Add(-time.Minute)
	awsFake := fakes.NewAWS(t, ts, func(name string, d map[string]string) (float64, bool) { return 7, d["FunctionName"] != "" })
	defer awsFake.Close()
	azFake := fakes.NewAzure(t, ts)
	defer azFake.Close()
	t.Setenv("TT_TEST_AZ_SECRET", "s3cret-value")
	fac := &factory.Factory{Endpoints: factory.Endpoints{AWS: awsFake.URL, AzureLogin: azFake.URL, AzureARM: azFake.URL}}
	snd := &captureSender{}
	r := runner.New(runner.Options{Store: s, Sender: snd, Factory: fac.Build, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	for _, x := range []struct{ provider, cfg, cred string }{
		{"aws", `{"regions":["eu-central-1"],"services":["lambda"]}`, ""},
		{"azure", `{"tenantId":"tenant-1","clientId":"client-1"}`, "env:TT_TEST_AZ_SECRET"},
	} {
		in := model.Integration{ProjectID: project, Provider: x.provider, Name: "it-" + x.provider, Config: []byte(x.cfg), CredentialsRef: x.cred}
		if err := s.DB.QueryRow(ctx, `INSERT INTO integrations (project_id, provider, name, config, credentials_ref) VALUES ($1, $2, $3, $4, nullif($5, '')) RETURNING id`,
			project, x.provider, in.Name, x.cfg, x.cred).Scan(&in.ID); err != nil {
			t.Fatal(err)
		}
		if err := r.RunOnce(ctx, in); err != nil {
			t.Fatalf("%s: %v", x.provider, err)
		}
		var status string
		var resources int
		_ = s.DB.QueryRow(ctx, `SELECT status, resources FROM integrations WHERE id = $1`, in.ID).Scan(&status, &resources)
		if status != "ok" || resources == 0 {
			t.Errorf("%s status %q resources %d", x.provider, status, resources)
		}
	}
	rows, err := s.DB.Query(ctx, `SELECT slug, provider, service, array_to_string(triggers, ','), coalesce(meta->>'plan', '') FROM serverless_functions WHERE project_id = $1 ORDER BY slug`, project)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var slug, provider, service, triggers, plan string
		if err := rows.Scan(&slug, &provider, &service, &triggers, &plan); err != nil {
			t.Fatal(err)
		}
		got = append(got, strings.Join([]string{slug, provider, service, triggers, plan}, "|"))
	}
	want := []string{
		"aws-eu-central-1-nightly-report|aws|nightly-report|schedule:nightly-schedule|",
		"aws-eu-central-1-orders-api|aws|orders|api,http|",
		"aws-eu-central-1-orders-worker|aws|orders-worker|kinesis:clicks,sqs:orders|",
		"azure-billing-flex|azure|billing-flex|eventhub:invoices|Flex",
		"azure-orders-func|azure|orders|http,schedule|Consumption",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rows\n got %v\nwant %v", got, want)
	}
	ids := map[string]bool{}
	for _, x := range snd.samples {
		ids[x.Attrs["trooper.faas.id"]] = true
	}
	if len(ids) != 5 {
		t.Errorf("samples for %v", ids)
	}
}
