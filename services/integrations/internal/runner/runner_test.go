package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/integrations/internal/model"
	"github.com/tasktrooper/telemetry/services/integrations/internal/otlp"
)

type fakeStore struct {
	mu      sync.Mutex
	list    []model.Integration
	status  map[int64]string
	errs    map[int64]string
	res     map[int64]int
	deploys int
	// taken slugs belong to a manual row; SyncDatabases does not return them.
	taken map[string]bool
}

func (s *fakeStore) Integrations(context.Context) ([]model.Integration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Integration(nil), s.list...), nil
}

func (s *fakeStore) SetStatus(_ context.Context, id int64, status, msg string, n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[id], s.errs[id] = status, msg
	if n >= 0 {
		s.res[id] = n
	}
	return nil
}

func (s *fakeStore) SyncDatabases(_ context.Context, in model.Integration, dbs []model.Database) ([]string, error) {
	var out []string
	for _, d := range dbs {
		if sl := model.Slug(in.Provider, d.Name); !s.taken[sl] {
			out = append(out, sl)
		}
	}
	return out, nil
}

func (s *fakeStore) SyncBrokers(context.Context, model.Integration, []model.Broker) ([]string, error) {
	return nil, nil
}

func (s *fakeStore) SyncFunctions(_ context.Context, _ model.Integration, fns []model.Function) ([]string, error) {
	var out []string
	for _, f := range fns {
		if !s.taken[f.Slug] {
			out = append(out, f.Slug)
		}
	}
	return out, nil
}

func (s *fakeStore) PatchDatabaseMeta(context.Context, model.Integration, map[string]map[string]any) error {
	return nil
}

func (s *fakeStore) UpsertDeploys(_ context.Context, _ model.Integration, ds []model.DeployMarker) error {
	s.mu.Lock()
	s.deploys += len(ds)
	s.mu.Unlock()
	return nil
}

type fakeSender struct {
	mu   sync.Mutex
	sent map[int64][]model.Sample
}

func (f *fakeSender) Send(_ context.Context, t otlp.Target, ss []model.Sample) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent[t.IntegrationID] = append(f.sent[t.IntegrationID], ss...)
	return nil
}

type fakeProvider struct {
	discover func() (*model.Inventory, error)
	collect  func() (*model.Result, error)
}

func (p *fakeProvider) Discover(context.Context) (*model.Inventory, error) { return p.discover() }
func (p *fakeProvider) Collect(context.Context, *model.Inventory, time.Time) (*model.Result, error) {
	return p.collect()
}

func TestIsolationAndOwnership(t *testing.T) {
	st := &fakeStore{
		list: []model.Integration{
			{ID: 1, ProjectID: 9, Provider: "aws", Name: "panics"},
			{ID: 2, ProjectID: 9, Provider: "aws", Name: "good"},
			{ID: 3, ProjectID: 9, Provider: "aws", Name: "badcreds"},
		},
		status: map[int64]string{}, errs: map[int64]string{}, res: map[int64]int{},
		taken: map[string]bool{"aws-manual": true},
	}
	snd := &fakeSender{sent: map[int64][]model.Sample{}}
	ts := time.Now().Truncate(time.Minute)
	factory := func(_ context.Context, in model.Integration) (model.Provider, error) {
		switch in.ID {
		case 1:
			return &fakeProvider{discover: func() (*model.Inventory, error) { panic("boom") }}, nil
		case 3:
			return nil, errors.New("credentials secret:7: secret not found")
		}
		return &fakeProvider{
			discover: func() (*model.Inventory, error) {
				return &model.Inventory{Databases: []model.Database{{Name: "orders"}, {Name: "manual"}}}, nil
			},
			collect: func() (*model.Result, error) {
				return &model.Result{
					Samples: []model.Sample{
						{Name: model.DBCPU, Value: 0.5, TS: ts, Attrs: map[string]string{"trooper.db.id": "aws-orders"}},
						{Name: model.DBCPU, Value: 0.9, TS: ts, Attrs: map[string]string{"trooper.db.id": "aws-manual"}},
					},
					Deploys: []model.DeployMarker{{ExternalID: "d1"}},
				}, nil
			},
		}, nil
	}
	r := New(Options{Store: st, Sender: snd, Factory: factory, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Reload: time.Hour, Metrics: 20 * time.Millisecond, RetryDelay: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snd.mu.Lock()
		n := len(snd.sent[2])
		snd.mu.Unlock()
		st.mu.Lock()
		ready := n >= 2 && st.status[1] != "" && st.status[3] != ""
		st.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.status[2] != "ok" || st.res[2] != 2 {
		t.Errorf("good integration: status %q resources %d err %q", st.status[2], st.res[2], st.errs[2])
	}
	if st.status[1] != "error" || st.errs[1] == "" {
		t.Errorf("panicking integration: %q %q", st.status[1], st.errs[1])
	}
	if st.status[3] != "error" || st.errs[3] != "credentials secret:7: secret not found" {
		t.Errorf("bad creds: %q %q", st.status[3], st.errs[3])
	}
	for _, s := range snd.sent[2] {
		if s.Attrs["trooper.db.id"] == "aws-manual" {
			t.Fatal("sample for a manually added (unowned) row was sent")
		}
	}
	if len(snd.sent[1]) != 0 || st.deploys == 0 {
		t.Errorf("sent[1]=%d deploys=%d", len(snd.sent[1]), st.deploys)
	}
}

func TestReconcileStopsRemoved(t *testing.T) {
	st := &fakeStore{list: []model.Integration{{ID: 5, Provider: "neon"}}, status: map[int64]string{}, errs: map[int64]string{}, res: map[int64]int{}}
	block := make(chan struct{})
	factory := func(ctx context.Context, _ model.Integration) (model.Provider, error) {
		return &fakeProvider{discover: func() (*model.Inventory, error) { <-block; return &model.Inventory{}, nil },
			collect: func() (*model.Result, error) { return &model.Result{}, nil }}, nil
	}
	r := New(Options{Store: st, Sender: &fakeSender{sent: map[int64][]model.Sample{}}, Factory: factory, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx := context.Background()
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.workers) != 1 {
		t.Fatal("worker not started")
	}
	close(block)
	st.mu.Lock()
	st.list = nil
	st.mu.Unlock()
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.workers) != 0 {
		t.Fatal("worker not stopped")
	}
}

func TestSplitSyncRequest(t *testing.T) {
	want := time.UnixMilli(1761393600000)
	tests := []struct {
		in       string
		wantT    time.Time
		wantRest string
	}{
		{`{"teamId":"t","syncRequestedAt":1761393600000}`, want, `{"teamId":"t"}`},
		{`{"syncRequestedAt":"1761393600000","teamId":"t"}`, want, `{"teamId":"t"}`},
		{`{"teamId":"t","syncRequestedAt":"2025-10-25T12:00:00Z"}`, want, `{"teamId":"t"}`},
		{`{"teamId":"t","syncRequestedAt":"2025-10-25T15:00:00.000+03:00"}`, want, `{"teamId":"t"}`},
		{`{"teamId":"t"}`, time.Time{}, `{"teamId":"t"}`},
		{`{"teamId":"t","syncRequestedAt":null}`, time.Time{}, `{"teamId":"t"}`},
	}
	for _, tt := range tests {
		rest, got := SplitSyncRequest([]byte(tt.in))
		if !got.Equal(tt.wantT) || string(rest) != tt.wantRest {
			t.Errorf("%s → %s %v", tt.in, rest, got)
		}
	}
}

func TestSyncRequestTriggersWithoutRestart(t *testing.T) {
	last := time.Now().Add(-time.Hour)
	st := &fakeStore{list: []model.Integration{{ID: 7, Provider: "vercel", Config: []byte(`{"teamId":"t"}`), LastSyncAt: &last}},
		status: map[int64]string{}, errs: map[int64]string{}, res: map[int64]int{}}
	var mu sync.Mutex
	builds, discovers := 0, 0
	factory := func(context.Context, model.Integration) (model.Provider, error) {
		mu.Lock()
		builds++
		mu.Unlock()
		return &fakeProvider{
			discover: func() (*model.Inventory, error) { mu.Lock(); discovers++; mu.Unlock(); return &model.Inventory{}, nil },
			collect:  func() (*model.Result, error) { return &model.Result{}, nil },
		}, nil
	}
	r := New(Options{Store: st, Sender: &fakeSender{sent: map[int64][]model.Sample{}}, Factory: factory,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: time.Hour, Discover: time.Hour})
	defer r.StopAll()
	ctx := context.Background()
	count := func() (int, int) { mu.Lock(); defer mu.Unlock(); return builds, discovers }
	waitFor := func(d int) {
		for i := 0; i < 200; i++ {
			if _, got := count(); got >= d {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	_ = r.Reconcile(ctx)
	waitFor(1)
	// Older than last_sync_at → ignored.
	st.mu.Lock()
	st.list[0].Config = []byte(fmt.Sprintf(`{"teamId":"t","syncRequestedAt":%d}`, last.Add(-time.Minute).UnixMilli()))
	st.mu.Unlock()
	_ = r.Reconcile(ctx)
	time.Sleep(50 * time.Millisecond)
	if b, d := count(); b != 1 || d != 1 {
		t.Fatalf("stale request: builds %d discovers %d", b, d)
	}
	// Newer (ISO) → immediate discovery, same worker; repeated reloads don't re-trigger.
	st.mu.Lock()
	st.list[0].Config = []byte(`{"teamId":"t","syncRequestedAt":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`)
	st.mu.Unlock()
	_ = r.Reconcile(ctx)
	waitFor(2)
	_ = r.Reconcile(ctx)
	time.Sleep(50 * time.Millisecond)
	if b, d := count(); b != 1 || d != 2 {
		t.Fatalf("sync request: builds %d discovers %d (want 1, 2)", b, d)
	}
}

func TestRunOnceFiltersUnownedFunctions(t *testing.T) {
	st := &fakeStore{status: map[int64]string{}, errs: map[int64]string{}, res: map[int64]int{}, taken: map[string]bool{"azure-manual": true}}
	snd := &fakeSender{sent: map[int64][]model.Sample{}}
	factory := func(context.Context, model.Integration) (model.Provider, error) {
		return &fakeProvider{
			discover: func() (*model.Inventory, error) {
				return &model.Inventory{Functions: []model.Function{{Slug: "azure-app", Name: "app"}, {Slug: "azure-manual", Name: "manual"}}}, nil
			},
			collect: func() (*model.Result, error) {
				return &model.Result{Samples: []model.Sample{
					{Name: model.FaaSInvocations, Value: 1, Attrs: model.FaaSAttrs("azure-app", "app")},
					{Name: model.FaaSInvocations, Value: 2, Attrs: model.FaaSAttrs("azure-manual", "manual")},
				}}, nil
			},
		}, nil
	}
	r := New(Options{Store: st, Sender: snd, Factory: factory, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err := r.RunOnce(context.Background(), model.Integration{ID: 4, Provider: "azure"}); err != nil {
		t.Fatal(err)
	}
	if len(snd.sent[4]) != 1 || snd.sent[4][0].Attrs["trooper.faas.id"] != "azure-app" || st.res[4] != 2 {
		t.Fatalf("sent %v resources %d", snd.sent[4], st.res[4])
	}
}
