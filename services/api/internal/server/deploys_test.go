package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/auth"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

type deployStore struct {
	fakeStore
	got    model.DeployInput
	gotTen tenant.Tenant
}

func (f *deployStore) CreateDeploys(ctx context.Context, now time.Time, in model.DeployInput) ([]model.DeployMarker, error) {
	f.got = in
	f.gotTen, _ = tenant.From(ctx)
	if _, err := store.DeployRows(in, now); err != nil {
		return nil, err
	}
	return []model.DeployMarker{{Service: in.Service, Version: in.Version, Source: "ci", Env: "production"}}, nil
}

func TestPostDeploys(t *testing.T) {
	for _, tc := range []struct {
		name, key, body string
		want            int
	}{
		{"created", "good", `{"service":"api","version":"v1","commitSha":"abc","author":"ci"}`, 201},
		{"no auth", "", `{"service":"api","version":"v1"}`, 401},
		{"invalid", "good", `{"version":"v1"}`, 400},
		{"bad json", "good", `{`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &deployStore{}
			fs.t = t
			s := (&Server{Store: fs, Auth: auth.Static{"good": {OrgID: 7, ProjectID: 42}}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
				Now: func() time.Time { return testNow }}).Handler()
			w := do(s, "POST", "/api/v1/deploys", tc.key, tc.body)
			if w.Code != tc.want {
				t.Fatalf("status %d want %d: %s", w.Code, tc.want, w.Body)
			}
			if tc.want != 201 {
				return
			}
			var out []model.DeployMarker
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out) != 1 || out[0].Source != "ci" {
				t.Errorf("body %s", w.Body)
			}
			if fs.gotTen != (tenant.Tenant{OrgID: 7, ProjectID: 42}) || fs.got.CommitSHA != "abc" {
				t.Errorf("tenant %+v in %+v", fs.gotTen, fs.got)
			}
		})
	}
}
