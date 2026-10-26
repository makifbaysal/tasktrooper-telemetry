package ratelimit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func count(l *Limiter, project uint32, rps, n int) (allowed int, lastWait time.Duration) {
	for range n {
		ok, wait := l.Allow(context.Background(), project, rps)
		if ok {
			allowed++
		} else {
			lastWait = wait
		}
	}
	return
}

func TestLocal(t *testing.T) {
	l := New(nil, 2, quiet)
	allowed, wait := count(l, 1, 10, 50)
	if allowed < 20 || allowed > 21 { // burst = 2×rps
		t.Fatalf("allowed=%d want ≈20", allowed)
	}
	if wait <= 0 || RetryAfterSeconds(wait) != 1 {
		t.Fatalf("wait=%v", wait)
	}
	if ok, _ := l.Allow(context.Background(), 2, 10); !ok {
		t.Fatal("projects are limited independently")
	}
	if ok, _ := l.Allow(context.Background(), 1, 0); !ok {
		t.Fatal("rps 0 = unlimited")
	}
}

func TestRedisFallback(t *testing.T) {
	// Unreachable Redis → in-memory limiter.
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 20 * time.Millisecond})
	defer rdb.Close()
	l := New(rdb, 1, quiet)
	if allowed, _ := count(l, 1, 5, 20); allowed != 5 {
		t.Fatalf("allowed=%d want 5", allowed)
	}
}

func TestRedisGCRA(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	defer rdb.Close()
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skip("redis not available:", err)
	}
	l := New(rdb, 2, quiet)
	project := rand.Uint32()
	defer rdb.Del(context.Background(), fmt.Sprintf("tt:rl:ingest:%d", project))
	allowed, wait := count(l, project, 1, 20) // burst 2; refill is 1/s, so at most one extra
	if allowed < 2 || allowed > 3 {
		t.Fatalf("allowed=%d want 2-3", allowed)
	}
	if wait <= 0 || wait > 2*time.Second {
		t.Fatalf("wait=%v", wait)
	}
	if l.downUntil.Load() != 0 {
		t.Fatal("redis path should have been used")
	}
}
