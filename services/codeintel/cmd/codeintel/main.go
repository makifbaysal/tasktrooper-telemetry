// Command codeintel reads connected repositories (ADR-014, contracts §13/§18): endpoint/consumer/cron/view/dependency
// inventory, criticality (static rules + optional Claude enrichment), matching with observed traffic, alert
// suggestions, generated load test drafts and alert-as-code monitors. Serves /healthz, /metrics and the GitHub, GitLab and
// Bitbucket webhooks.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tasktrooper/telemetry/services/pkg/aiquota"
	"github.com/tasktrooper/telemetry/services/pkg/config"
	"github.com/tasktrooper/telemetry/services/pkg/githubapp"
	"github.com/tasktrooper/telemetry/services/pkg/logx"
	"github.com/tasktrooper/telemetry/services/pkg/secrets"
	"github.com/tasktrooper/telemetry/services/pkg/vcs"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/analyze"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/enrich"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/obs"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/observe"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/quota"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/runner"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/source"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/store"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/webhook"
)

func main() {
	once := flag.Int64("once", 0, "analyze this repository id now (full scan), print the report and exit")
	flag.Parse()
	if err := run(*once); err != nil {
		fmt.Fprintln(os.Stderr, "codeintel:", err)
		os.Exit(1)
	}
}

func run(once int64) error {
	common := config.Load()
	log := logx.New("codeintel", common.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	skey, err := secrets.Key()
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, common.PostgresURL)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer st.Close()
	resolve := func(ctx context.Context, ref string, projectID int64) ([]byte, error) {
		return secrets.Resolve(ctx, ref, projectID, skey, st)
	}

	app, err := githubapp.FromEnv(ctx)
	switch {
	case errors.Is(err, githubapp.ErrNotConfigured):
		log.Info("GitHub App not configured; installation repositories cannot be cloned")
		app = nil
	case err != nil:
		return err
	}
	webhookSecret, err := secretValue(ctx, os.Getenv("TT_GITHUB_WEBHOOK_SECRET"))
	if err != nil {
		return fmt.Errorf("TT_GITHUB_WEBHOOK_SECRET: %w", err)
	}

	var llm enrich.Enricher
	if l := enrich.NewLLM(os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("TT_CODEINTEL_MODEL")); l != nil {
		l.Log = log
		llm = l
		log.Info("LLM enrichment enabled", "model", l.Model())
	} else {
		log.Info("LLM enrichment disabled (set ANTHROPIC_API_KEY and TT_CODEINTEL_MODEL); static criticality rules only")
	}

	qcfg, err := aiquota.FromEnv()
	if err != nil {
		return err
	}

	an := &analyze.Analyzer{
		Store: st,
		Fetcher: &source.Fetcher{WorkDir: config.String("TT_CODEINTEL_WORKDIR", ""), App: app, Token: resolve,
			APIURL: os.Getenv("TT_GITHUB_API_URL")},
		Traffic:     &lazyClickHouse{url: common.ClickHouseURL},
		LLM:         llm,
		MaxLLMItems: config.Int("TT_CODEINTEL_LLM_MAX_ITEMS", 300),
		Quota:       &quota.Gate{Store: quota.PGStore{Pool: st.DB}, Config: qcfg, Log: log},
		Log:         log,
	}
	if once > 0 {
		return analyzeOnce(ctx, st, an, once)
	}

	reg := obs.NewRegistry()
	reg.Describe("codeintel_analyses_total", "counter", "Finished analyses by result.")
	reg.Describe("codeintel_webhooks_total", "counter", "GitHub / GitLab / Bitbucket webhook deliveries by event and result.")
	reg.Describe("codeintel_repositories", "gauge", "Connected repositories.")
	r := &runner.Runner{
		Store: st, Analyzer: an, Metrics: reg, Log: log,
		Interval:    config.Duration("TT_CODEINTEL_INTERVAL", 15*time.Second),
		Debounce:    config.Duration("TT_CODEINTEL_DEBOUNCE", 30*time.Second),
		Timeout:     config.Duration("TT_CODEINTEL_ANALYZE_TIMEOUT", 10*time.Minute),
		Concurrency: config.Int("TT_CODEINTEL_CONCURRENCY", 2),
	}
	wh := &webhook.Handler{Store: st, Secret: webhookSecret, Metrics: reg, Log: log,
		RepoSecret: func(ctx context.Context, repo model.Repo) ([]byte, error) {
			return resolve(ctx, repo.WebhookSecretRef, repo.ProjectID)
		}}
	if len(webhookSecret) == 0 {
		log.Warn("TT_GITHUB_WEBHOOK_SECRET not set; only repository webhooks with webhook_secret_ref are accepted")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		c, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := st.Ping(c); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /metrics", reg)
	mux.Handle("POST /v1/webhooks/github", wh)
	repoSecret := func(ctx context.Context, repo model.Repo) ([]byte, error) {
		b, err := resolve(ctx, repo.WebhookSecretRef, repo.ProjectID)
		return []byte(strings.TrimSpace(string(b))), err
	}
	for _, p := range []string{vcs.GitLab, vcs.Bitbucket} { // per-repository webhooks (contracts §18a)
		mux.Handle("POST /v1/webhooks/"+p, &webhook.RepoHook{Provider: p, Store: st, RepoSecret: repoSecret, Metrics: reg, Log: log})
	}
	addr := config.String("TT_CODEINTEL_ADDR", ":8096")
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: time.Minute}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
	}()
	go func() {
		log.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			stop()
		}
	}()
	r.Run(ctx)
	return nil
}

func analyzeOnce(ctx context.Context, st *store.Store, an *analyze.Analyzer, id int64) error {
	started, _, ok, err := st.Claim(ctx, id, time.Hour)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("repository %d is disconnected or being analyzed", id)
	}
	repo, err := st.Repo(ctx, id)
	if err != nil {
		return err
	}
	repo.LastAnalyzedSHA = "" // -once is always a full scan (no git diff against the previous analysis)
	rep, err := an.Run(ctx, repo, nil)
	if err != nil {
		_ = st.FinishError(context.WithoutCancel(ctx), id, started, err.Error(), []string{store.FullScan})
		return err
	}
	if err := st.FinishOK(ctx, id, started, rep.SHA, rep.Languages, rep.Services, rep.Summary()); err != nil {
		return err
	}
	fmt.Printf("llm: %s %s (%.2f credits, %d skipped)\n", rep.LLM.Status, rep.LLM.Reason, rep.LLM.Credits, rep.LLM.Skipped)
	fmt.Printf("%s @ %s: %d items (%d observed, %d shadow), %d suggestions, %d load test drafts, %d LLM; monitors +%d ~%d -%d\n",
		repo.FullName, rep.SHA, rep.Items, rep.Observed, rep.Shadows, rep.Suggestions, rep.Scenarios, rep.LLMItems,
		rep.Monitors.Created, rep.Monitors.Updated, rep.Monitors.Deleted)
	for _, w := range rep.Warnings {
		fmt.Println("warning:", w)
	}
	return nil
}

// secretValue reads a literal secret or an env:/file: reference.
func secretValue(ctx context.Context, v string) ([]byte, error) {
	if strings.HasPrefix(v, "env:") || strings.HasPrefix(v, "file:") {
		b, err := secrets.Resolve(ctx, v, 0, nil, nil)
		return []byte(strings.TrimSpace(string(b))), err
	}
	return []byte(v), nil
}

// lazyClickHouse connects on first use and retries on the next analysis when ClickHouse is down.
type lazyClickHouse struct {
	url string
	mu  sync.Mutex
	ch  *observe.ClickHouse
}

func (l *lazyClickHouse) Traffic(ctx context.Context, orgID, projectID int64) (*observe.Traffic, error) {
	l.mu.Lock()
	if l.ch == nil {
		ch, err := observe.Open(ctx, l.url)
		if err != nil {
			l.mu.Unlock()
			return nil, fmt.Errorf("clickhouse: %w", err)
		}
		l.ch = ch
	}
	ch := l.ch
	l.mu.Unlock()
	return ch.Traffic(ctx, orgID, projectID)
}
