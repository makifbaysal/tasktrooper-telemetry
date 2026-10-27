//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tasktrooper/telemetry/services/ingest/internal/app"
)

// TestInternalCredential: ingest-gw with TT_INTERNAL_TOKEN resolves x-trooper-project against Postgres
// (temporary project 9601, deleted at the end).
func TestInternalCredential(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	pg, err := pgxpool.New(ctx, app.GatewayFromEnv().Common.PostgresURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	if _, err := pg.Exec(ctx, `INSERT INTO projects (id, org_id, name, slug) VALUES (9601, 1, 'internal-e2e', 'internal-e2e-9601') ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = pg.Exec(context.Background(), `DELETE FROM projects WHERE id = 9601`) }()

	gcfg := app.GatewayFromEnv()
	gcfg.GW.HTTPAddr, gcfg.GW.GRPCAddr, gcfg.MetricsAddr = "127.0.0.1:0", "127.0.0.1:0", ""
	gcfg.InternalToken = "e2e-internal-token"
	addrs := make(chan string, 1)
	gctx, gstop := context.WithCancel(ctx)
	gdone := make(chan error, 1)
	go func() { gdone <- app.RunGateway(gctx, gcfg, log, func(h, _ string) { addrs <- h }) }()
	defer func() { gstop(); <-gdone }()
	var base string
	select {
	case a := <-addrs:
		base = "http://" + a
	case err := <-gdone:
		t.Fatalf("gateway failed: %v", err)
	}

	send := func(token, project, origin string) int {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/logs", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-trooper-internal", token)
		req.Header.Set("x-trooper-project", project)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	for _, tc := range []struct {
		token, project, origin string
		want                   int
	}{
		{"e2e-internal-token", "9601", "", 200},
		{"e2e-internal-token", "9601", "https://app.example.com", 403},
		{"wrong", "9601", "", 401},
		{"e2e-internal-token", "9699", "", 401},
	} {
		if got := send(tc.token, tc.project, tc.origin); got != tc.want {
			t.Errorf("%+v: got %d want %d", tc, got, tc.want)
		}
	}
}
