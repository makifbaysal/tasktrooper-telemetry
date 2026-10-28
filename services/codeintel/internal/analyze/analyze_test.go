package analyze

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/source"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/store"
)

func TestIncremental(t *testing.T) {
	now := time.Now()
	analyzed := model.Repo{AnalyzedAt: &now, LastAnalyzedSHA: "abc"}
	big := make([]string, 201)
	cases := []struct {
		name    string
		repo    model.Repo
		pending []string
		want    bool
	}{
		{"changed files", analyzed, []string{"a.go"}, true},
		{"no list → full", analyzed, nil, false},
		{"full marker", analyzed, []string{"a.go", "*"}, false},
		{"too many", analyzed, big, false},
		{"never analyzed", model.Repo{}, []string{"a.go"}, false},
	}
	for _, c := range cases {
		if got := Incremental(c.repo, c.pending); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

type fakeDiffer struct {
	diff  *source.Diff
	err   error
	calls int
}

func (f *fakeDiffer) Fetch(context.Context, model.Repo) (*source.Checkout, error) {
	return &source.Checkout{}, nil
}
func (f *fakeDiffer) ChangedSince(_ context.Context, _ *source.Checkout, sha string) (*source.Diff, error) {
	f.calls++
	if sha != "abc" {
		return nil, errors.New("wrong base " + sha)
	}
	return f.diff, f.err
}

type plainFetcher struct{}

func (plainFetcher) Fetch(context.Context, model.Repo) (*source.Checkout, error) {
	return &source.Checkout{}, nil
}

func TestChangedFiles(t *testing.T) {
	now := time.Now()
	analyzed := model.Repo{FullName: "acme/shop", AnalyzedAt: &now, LastAnalyzedSHA: "abc"}
	big := make([]string, store.MaxPendingPaths+1)
	for i := range big {
		big[i] = fmt.Sprintf("f%d.go", i)
	}
	cases := []struct {
		name      string
		repo      model.Repo
		pending   []string
		fetcher   Fetcher
		want      []string // nil = full scan
		source    string
		reason    string // substring of FullReason
		diffCalls int
	}{
		{"never analyzed", model.Repo{}, []string{"a.go"}, &fakeDiffer{}, nil, "", "", 0},
		{"webhook list, no git", analyzed, []string{"a.go", "../x", "b/./c.go"}, &fakeDiffer{}, []string{"a.go", "b/c.go"}, DiffWebhook, "", 0},
		{"webhook list with config", analyzed, []string{"a.go", ".trooper/config.yaml"}, &fakeDiffer{}, nil, "", "service layout", 0},
		{"webhook list with modified manifest", analyzed, []string{"svc/go.mod"}, &fakeDiffer{}, []string{"svc/go.mod"}, DiffWebhook, "", 0},
		{"manual → git diff", analyzed, []string{store.FullScan}, &fakeDiffer{diff: &source.Diff{Paths: []string{"a.go", "gone.go"}}},
			[]string{"a.go", "gone.go"}, DiffGit, "", 1},
		{"empty pending → git diff", analyzed, nil, &fakeDiffer{diff: &source.Diff{Paths: []string{"a.go"}}}, []string{"a.go"}, DiffGit, "", 1},
		{"webhook overflow → git diff", analyzed, big, &fakeDiffer{diff: &source.Diff{Paths: []string{"a.go"}}}, []string{"a.go"}, DiffGit, "", 1},
		{"HEAD == base → zero-change incremental", analyzed, []string{store.FullScan}, &fakeDiffer{diff: &source.Diff{}}, []string{}, DiffGit, "", 1},
		{"base unreachable", analyzed, []string{store.FullScan}, &fakeDiffer{err: source.ErrNoBase}, nil, "", "base commit", 1},
		{"diff too large", analyzed, []string{store.FullScan}, &fakeDiffer{diff: &source.Diff{Paths: big}}, nil, "", "files changed", 1},
		{"layout change", analyzed, []string{store.FullScan}, &fakeDiffer{diff: &source.Diff{Paths: []string{"x/go.mod"}, Layout: []string{"x/go.mod"}}},
			nil, "", "x/go.mod", 1},
		{"fetcher without diff", analyzed, []string{store.FullScan}, plainFetcher{}, nil, "", "cannot diff", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Analyzer{Fetcher: c.fetcher}
			rep := &Report{}
			got := a.changedFiles(context.Background(), c.repo, c.pending, &source.Checkout{}, rep)
			if (got == nil) != (c.want == nil) {
				t.Fatalf("got %v want %v (reason %q)", got, c.want, rep.FullReason)
			}
			keys := make([]string, 0, len(got))
			for k := range got {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if c.want != nil && strings.Join(keys, ",") != strings.Join(c.want, ",") {
				t.Fatalf("changed %v want %v", keys, c.want)
			}
			if rep.DiffSource != c.source || !strings.Contains(rep.FullReason, c.reason) || c.reason == "" && rep.FullReason != "" {
				t.Fatalf("source %q reason %q", rep.DiffSource, rep.FullReason)
			}
			if d, ok := c.fetcher.(*fakeDiffer); ok && d.calls != c.diffCalls {
				t.Fatalf("diff calls %d", d.calls)
			}
		})
	}
}
