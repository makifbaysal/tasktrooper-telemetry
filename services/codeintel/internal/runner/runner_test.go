package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/analyze"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
)

type fakeStore struct {
	mu       sync.Mutex
	repos    []model.Repo
	claimed  map[int64]bool
	pending  map[int64][]string
	ok, errs map[int64]int
	restored map[int64][]string
}

func newStore(repos ...model.Repo) *fakeStore {
	return &fakeStore{repos: repos, claimed: map[int64]bool{}, pending: map[int64][]string{}, ok: map[int64]int{}, errs: map[int64]int{},
		restored: map[int64][]string{}}
}

func (s *fakeStore) Repos(context.Context) ([]model.Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Repo(nil), s.repos...), nil
}

func (s *fakeStore) Claim(_ context.Context, id int64, _ time.Duration) (time.Time, []string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed[id] {
		return time.Time{}, nil, false, nil
	}
	s.claimed[id] = true
	p := s.pending[id]
	delete(s.pending, id)
	return time.Now(), p, true, nil
}

func (s *fakeStore) FinishOK(_ context.Context, id int64, _ time.Time, _ string, _, _ []string, _ *model.Analysis) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimed[id] = false
	s.ok[id]++
	now := time.Now()
	for i := range s.repos {
		if s.repos[i].ID == id {
			s.repos[i].AnalyzedAt = &now
		}
	}
	return nil
}

func (s *fakeStore) FinishError(_ context.Context, id int64, _ time.Time, _ string, pending []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimed[id] = false
	s.errs[id]++
	s.restored[id] = pending
	return nil
}

type fakeAnalyzer struct {
	calls   atomic.Int32
	active  atomic.Int32
	maxSeen atomic.Int32
	fn      func(model.Repo, []string) (*analyze.Report, error)
	delay   time.Duration
}

func (a *fakeAnalyzer) Run(ctx context.Context, repo model.Repo, pending []string) (*analyze.Report, error) {
	a.calls.Add(1)
	n := a.active.Add(1)
	defer a.active.Add(-1)
	for {
		m := a.maxSeen.Load()
		if n <= m || a.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	if a.delay > 0 {
		select {
		case <-time.After(a.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if a.fn != nil {
		return a.fn(repo, pending)
	}
	return &analyze.Report{SHA: "abc"}, nil
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func ptr(t time.Time) *time.Time { return &t }

func TestReady(t *testing.T) {
	now := time.Now()
	r := &Runner{Debounce: 30 * time.Second, Log: quiet(), Now: func() time.Time { return now }}
	r.defaults()
	cases := []struct {
		name string
		repo model.Repo
		want bool
	}{
		{"never analyzed", model.Repo{ID: 1, Status: "pending"}, true},
		{"not due", model.Repo{ID: 2, Status: "ready", AnalyzedAt: ptr(now.Add(-time.Hour))}, false},
		{"due, quiet", model.Repo{ID: 3, Status: "ready", AnalyzedAt: ptr(now.Add(-time.Hour)), AnalyzeRequestedAt: ptr(now.Add(-time.Minute))}, true},
		{"push burst debounced", model.Repo{ID: 4, Status: "ready", AnalyzedAt: ptr(now.Add(-time.Minute)), AnalyzeRequestedAt: ptr(now.Add(-5 * time.Second))}, false},
		{"long burst still runs", model.Repo{ID: 5, Status: "ready", AnalyzedAt: ptr(now.Add(-time.Hour)), AnalyzeRequestedAt: ptr(now.Add(-5 * time.Second))}, true},
		{"disconnected", model.Repo{ID: 6, Status: "disconnected"}, false},
		{"other replica", model.Repo{ID: 7, Status: "analyzing", AnalysisStartedAt: ptr(now.Add(-time.Minute))}, false},
		{"stale claim", model.Repo{ID: 8, Status: "analyzing", AnalysisStartedAt: ptr(now.Add(-time.Hour))}, true},
	}
	for _, c := range cases {
		if got := r.ready(c.repo, now); got != c.want {
			t.Errorf("%s: ready=%v", c.name, got)
		}
	}
	// failure backoff unless re-requested
	r.failed[9] = failure{at: now.Add(-time.Minute), requested: now.Add(-2 * time.Minute)}
	if r.ready(model.Repo{ID: 9, Status: "error", AnalyzeRequestedAt: ptr(now.Add(-2 * time.Minute))}, now) {
		t.Error("backoff ignored")
	}
	if !r.ready(model.Repo{ID: 9, Status: "error", AnalyzeRequestedAt: ptr(now.Add(-40 * time.Second))}, now) {
		t.Error("re-request must bypass backoff")
	}
}

func TestOneAnalysisPerRepoAndConcurrency(t *testing.T) {
	st := newStore(model.Repo{ID: 1}, model.Repo{ID: 2}, model.Repo{ID: 3})
	an := &fakeAnalyzer{delay: 100 * time.Millisecond}
	r := &Runner{Store: st, Analyzer: an, Concurrency: 2, Log: quiet()}
	ctx := context.Background()
	for range 5 {
		r.Tick(ctx) // repeated ticks while running must not start the same repo twice
	}
	r.Wait()
	if an.maxSeen.Load() > 2 {
		t.Fatalf("concurrency %d", an.maxSeen.Load())
	}
	if an.calls.Load() != 2 {
		t.Fatalf("calls %d (third repo waits for a free worker)", an.calls.Load())
	}
	r.Tick(ctx)
	r.Wait()
	if an.calls.Load() != 3 || st.ok[1] != 1 || st.ok[2] != 1 || st.ok[3] != 1 {
		t.Fatalf("calls %d ok %v", an.calls.Load(), st.ok)
	}
	r.Tick(ctx) // nothing due any more
	r.Wait()
	if an.calls.Load() != 3 {
		t.Fatal("re-ran analyzed repos")
	}
}

func TestFailureAndPanicRestorePending(t *testing.T) {
	analyzed := time.Now().Add(-time.Hour)
	st := newStore(
		model.Repo{ID: 1, AnalyzedAt: &analyzed, LastAnalyzedSHA: "a", AnalyzeRequestedAt: ptr(time.Now().Add(-time.Minute))},
		model.Repo{ID: 2},
	)
	st.pending[1] = []string{"a.go", "b.go"}
	an := &fakeAnalyzer{fn: func(repo model.Repo, pending []string) (*analyze.Report, error) {
		if repo.ID == 1 {
			if len(pending) != 2 {
				t.Errorf("pending %v", pending)
			}
			return nil, errors.New("boom")
		}
		panic("extractor bug")
	}}
	r := &Runner{Store: st, Analyzer: an, Log: quiet()}
	r.Tick(context.Background())
	r.Wait()
	if st.errs[1] != 1 || st.errs[2] != 1 {
		t.Fatalf("errs %v", st.errs)
	}
	if got := st.restored[1]; len(got) != 2 {
		t.Fatalf("incremental pending not restored: %v", got)
	}
	if got := st.restored[2]; len(got) != 1 || got[0] != "*" {
		t.Fatalf("full scan failure must restore *: %v", got)
	}
	r.Tick(context.Background()) // backoff
	r.Wait()
	if an.calls.Load() != 2 {
		t.Fatalf("retried during backoff: %d", an.calls.Load())
	}
}

func TestTimeout(t *testing.T) {
	st := newStore(model.Repo{ID: 1})
	an := &fakeAnalyzer{delay: time.Second}
	r := &Runner{Store: st, Analyzer: an, Timeout: 50 * time.Millisecond, Log: quiet()}
	t0 := time.Now()
	r.Tick(context.Background())
	r.Wait()
	if time.Since(t0) > 500*time.Millisecond || st.errs[1] != 1 {
		t.Fatalf("timeout not enforced: %v errs %v", time.Since(t0), st.errs)
	}
}
