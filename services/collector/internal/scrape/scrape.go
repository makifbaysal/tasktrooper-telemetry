// Package scrape holds the per-engine scrapers. Every scraper is created once per target, keeps
// its own counter state (for deltas/rates) and is called every interval with a deadline.
package scrape

import (
	"context"
	"hash/fnv"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
)

// Scraper reads one target.
type Scraper interface {
	// Scrape returns the target's current metrics. It must honour ctx's deadline.
	Scrape(ctx context.Context) (emit.Result, error)
	Close() error
}

// MaxQueryText bounds db.query.text (attrs values ≤ 1 KB, data-model.md).
const MaxQueryText = 1024

var spaces = regexp.MustCompile(`\s+`)

// NormalizeQueryText collapses whitespace and truncates to MaxQueryText bytes on a rune boundary.
// The engines already replace literals with placeholders ($1 for Postgres, ? for MySQL).
func NormalizeQueryText(s string) string {
	s = strings.TrimSpace(spaces.ReplaceAllString(s, " "))
	if len(s) <= MaxQueryText {
		return s
	}
	cut := MaxQueryText - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// HashText returns a stable uint64 hash (FNV-1a) as a decimal string, used when the engine has
// no statement id of its own (Redis commands).
func HashText(s string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return strconv.FormatUint(h.Sum64(), 10)
}

// HexDigestToUint64 turns the first 16 hex chars of a digest (MySQL's sha256 DIGEST) into the
// decimal uint64 string that db_queries_1m.query_hash expects.
func HexDigestToUint64(d string) string {
	if len(d) > 16 {
		d = d[:16]
	}
	v, err := strconv.ParseUint(d, 16, 64)
	if err != nil {
		return HashText(d)
	}
	return strconv.FormatUint(v, 10)
}

// TopN returns the indexes of the n largest values (stable for ties by index).
func TopN(values []float64, n int) []int {
	idx := make([]int, 0, len(values))
	for i, v := range values {
		if v > 0 {
			idx = append(idx, i)
		}
	}
	// simple partial selection sort is fine for a few thousand statements
	for i := 0; i < len(idx) && i < n; i++ {
		best := i
		for j := i + 1; j < len(idx); j++ {
			if values[idx[j]] > values[idx[best]] {
				best = j
			}
		}
		idx[i], idx[best] = idx[best], idx[i]
	}
	if len(idx) > n {
		idx = idx[:n]
	}
	return idx
}
