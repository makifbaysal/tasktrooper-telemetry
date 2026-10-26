// Package ratelimit implements a per-project GCRA limiter in Redis (shared by all ingest-gw replicas)
// that falls back to an in-process token bucket while Redis is unavailable.
package ratelimit

import (
	"context"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"
)

// gcra: KEYS[1]=key, ARGV[1]=rate/s, ARGV[2]=burst. Returns {allowed, retry_after_us}.
// TAT (theoretical arrival time) is stored in microseconds of Redis server time.
var gcra = redis.NewScript(`
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000000 + tonumber(t[2])
local interval = 1000000 / rate
local tat = tonumber(redis.call('GET', KEYS[1]) or now)
if tat < now then tat = now end
local new_tat = tat + interval
local allow_at = new_tat - burst * interval
if allow_at > now then
  return {0, math.ceil(allow_at - now)}
end
redis.call('SET', KEYS[1], string.format('%.0f', new_tat), 'PX', math.ceil((new_tat - now) / 1000) + 1000)
return {1, 0}
`)

// Limiter rate-limits requests per project.
type Limiter struct {
	rdb        redis.Scripter // nil = local only
	burstScale float64
	timeout    time.Duration
	log        *slog.Logger

	downUntil atomic.Int64 // unix nanos; Redis skipped until then

	mu    sync.Mutex
	local map[uint32]*rate.Limiter
}

// New returns a limiter; rdb may be nil (in-memory only). burst = rps * burstScale (≥ 1).
func New(rdb redis.Scripter, burstScale float64, log *slog.Logger) *Limiter {
	if burstScale < 1 {
		burstScale = 1
	}
	return &Limiter{rdb: rdb, burstScale: burstScale, timeout: 50 * time.Millisecond, log: log, local: map[uint32]*rate.Limiter{}}
}

// Allow reports whether one request for project at rps is allowed, else how long to wait.
func (l *Limiter) Allow(ctx context.Context, project uint32, rps int) (bool, time.Duration) {
	if rps <= 0 {
		return true, 0
	}
	burst := max(1, int(math.Ceil(float64(rps)*l.burstScale)))
	if l.rdb != nil && time.Now().UnixNano() >= l.downUntil.Load() {
		ok, wait, err := l.redisAllow(ctx, project, rps, burst)
		if err == nil {
			return ok, wait
		}
		l.downUntil.Store(time.Now().Add(5 * time.Second).UnixNano())
		l.log.Warn("redis rate limit unavailable, falling back to in-memory", "err", err)
	}
	return l.localAllow(project, rps, burst)
}

func (l *Limiter) redisAllow(ctx context.Context, project uint32, rps, burst int) (bool, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	key := "tt:rl:ingest:" + strconv.FormatUint(uint64(project), 10)
	res, err := gcra.Run(ctx, l.rdb, []string{key}, rps, burst).Int64Slice()
	if err != nil {
		return false, 0, err
	}
	if len(res) != 2 {
		return true, 0, nil
	}
	return res[0] == 1, time.Duration(res[1]) * time.Microsecond, nil
}

func (l *Limiter) localAllow(project uint32, rps, burst int) (bool, time.Duration) {
	l.mu.Lock()
	lim := l.local[project]
	if lim == nil || lim.Limit() != rate.Limit(rps) || lim.Burst() != burst {
		lim = rate.NewLimiter(rate.Limit(rps), burst)
		l.local[project] = lim
	}
	l.mu.Unlock()
	r := lim.Reserve()
	if d := r.Delay(); d > 0 {
		r.Cancel()
		return false, d
	}
	return true, 0
}

// RetryAfterSeconds rounds a wait up to whole seconds for the Retry-After header (min 1).
func RetryAfterSeconds(d time.Duration) int {
	return max(1, int(math.Ceil(d.Seconds())))
}
