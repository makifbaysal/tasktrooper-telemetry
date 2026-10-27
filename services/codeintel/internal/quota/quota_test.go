package quota

import (
	"context"
	"errors"
	"testing"

	"github.com/tasktrooper/telemetry/services/pkg/aiquota"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/enrich"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
)

type fakeStore struct {
	decisions []aiquota.Decision
	err       error
	reqs      []aiquota.Request
	usages    []aiquota.Usage
}

func (f *fakeStore) Reserve(_ context.Context, _ *aiquota.Config, r aiquota.Request) (aiquota.Decision, error) {
	f.reqs = append(f.reqs, r)
	if f.err != nil {
		return aiquota.Decision{}, f.err
	}
	d := f.decisions[0]
	if len(f.decisions) > 1 {
		f.decisions = f.decisions[1:]
	}
	return d, nil
}

func (f *fakeStore) Record(_ context.Context, u aiquota.Usage) error {
	f.usages = append(f.usages, u)
	return nil
}

func TestBudget(t *testing.T) {
	cfg, err := aiquota.Parse("m=3/15", "", "")
	if err != nil {
		t.Fatal(err)
	}
	user := int64(42)
	repo := model.Repo{ID: 12, OrgID: 3, ProjectID: 9, AnalyzeRequestedBy: &user}
	tests := []struct {
		name    string
		store   *fakeStore
		want    []bool
		reasons []string
		checks  int // Store.Reserve calls (a denial is sticky)
	}{
		{"allowed", &fakeStore{decisions: []aiquota.Decision{{Allowed: true}}}, []bool{true, true}, []string{"", ""}, 2},
		{"denied is sticky", &fakeStore{decisions: []aiquota.Decision{{Allowed: true}, {Reason: aiquota.ReasonUserLimit}}},
			[]bool{true, false, false}, []string{"", "user_limit", "user_limit"}, 2},
		{"check failure fails closed", &fakeStore{err: errors.New("db down")}, []bool{false, false}, []string{"check_failed", "check_failed"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &Gate{Store: tt.store, Config: cfg}
			b := g.For(repo, aiquota.FeatureCodeintelEnrich)
			for i, want := range tt.want {
				ok, reason := b.Reserve(context.Background(), "m", 10_000, 2_000)
				if ok != want || reason != tt.reasons[i] {
					t.Fatalf("call %d: %v %q", i, ok, reason)
				}
			}
			if len(tt.store.reqs) != tt.checks {
				t.Fatalf("checks %d", len(tt.store.reqs))
			}
			r := tt.store.reqs[0]
			if r.OrgID != 3 || r.ProjectID != 9 || r.UserID == nil || *r.UserID != 42 || r.Feature != "codeintel.enrich" || r.EstimateCredits != 6 {
				t.Fatalf("request %+v", r)
			}
		})
	}
	st := &fakeStore{}
	b := (&Gate{Store: st, Config: cfg}).For(model.Repo{ID: 5, OrgID: 3}, aiquota.FeatureCodeintelEnrich)
	if c := b.Record(context.Background(), "m", enrich.Tokens{Input: 10_000, Output: 2_000}); c != 6 {
		t.Fatalf("credits %v", c)
	}
	u := st.usages[0]
	if u.Ref != "repo:5" || u.UserID != nil || u.InputTokens != 10_000 || u.OutputTokens != 2_000 || u.Credits != 6 || u.Model != "m" {
		t.Fatalf("usage %+v", u)
	}
	var nilGate *Gate
	if nilGate.For(repo, "x") != nil {
		t.Fatal("nil gate → nil budget")
	}
}
