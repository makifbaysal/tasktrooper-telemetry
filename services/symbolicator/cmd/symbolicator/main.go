// Command symbolicator serves the symbolication API (contracts.md §12): artifact upload (source maps,
// dSYMs, R8/ProGuard mappings, Flutter symbols) and stack symbolication.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/tasktrooper/telemetry/services/pkg/config"
	"github.com/tasktrooper/telemetry/services/pkg/logx"
	"github.com/tasktrooper/telemetry/services/symbolicator/internal/auth"
	"github.com/tasktrooper/telemetry/services/symbolicator/internal/native"
	"github.com/tasktrooper/telemetry/services/symbolicator/internal/server"
	"github.com/tasktrooper/telemetry/services/symbolicator/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "symbolicator:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()
	log := logx.New("symbolicator", cfg.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	objs, err := store.NewS3(startCtx, store.S3Config{
		Endpoint:  config.String("TT_S3_ENDPOINT", "http://localhost:9002"),
		Bucket:    config.String("TT_S3_BUCKET", "tt-artifacts"),
		AccessKey: config.String("TT_S3_ACCESS_KEY", "trooper"),
		SecretKey: config.String("TT_S3_SECRET_KEY", "trooper-secret"),
		Region:    config.String("TT_S3_REGION", "us-east-1"),
	})
	if err != nil {
		return fmt.Errorf("s3: %w", err)
	}

	pool, err := pgxpool.New(ctx, cfg.PostgresURL)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(startCtx); err != nil {
		log.Warn("postgres not reachable yet; requests answer 503 until it is", "err", err)
	}

	var cache server.ResultCache
	if url := config.String("TT_REDIS_URL", cfg.RedisURL); url != "" && url != "off" {
		opt, err := redis.ParseURL(url)
		if err != nil {
			return fmt.Errorf("redis url: %w", err)
		}
		rc := redis.NewClient(opt)
		defer rc.Close()
		if err := rc.Ping(startCtx).Err(); err != nil {
			log.Warn("redis not reachable; result cache degrades to misses", "err", err)
		}
		cache = server.RedisCache{C: rc}
	}

	if p := native.SwiftDemanglerPath(); p != "" {
		log.Info("swift demangling enabled", "tool", p)
	} else {
		log.Info("swift-demangle not found; Swift names stay mangled (set TT_SWIFT_DEMANGLE)")
	}

	srv, err := server.New(server.Config{
		Store:    store.NewStore(objs),
		Auth:     auth.New(auth.PostgresLookup(pool), 60*time.Second),
		Cache:    cache,
		CacheDir: config.String("TT_SYMBOLICATOR_CACHE_DIR", filepath.Join(os.TempDir(), "tt-symbolicator")),
		Log:      log,
	})
	if err != nil {
		return err
	}
	addr := config.String("TT_SYMBOLICATOR_ADDR", ":8095")
	hs := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr)
		errc <- hs.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()
	return hs.Shutdown(shutdown)
}
