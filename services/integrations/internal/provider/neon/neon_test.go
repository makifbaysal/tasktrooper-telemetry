package neon

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const projectsJSON = `{"projects":[{"id":"proj-1","name":"shop","region_id":"aws-eu-central-1","pg_version":16,
 "active_time_seconds":3600,"compute_time_seconds":900,"cpu_used_sec":800,"written_data_bytes":1000,"data_transfer_bytes":2000,
 "synthetic_storage_size":5000,"settings":{"quota":{"logical_size_bytes":10737418240}}}],"pagination":{"cursor":"proj-1"}}`
const projectJSON = `{"project":{"id":"proj-1","name":"shop","region_id":"aws-eu-central-1","pg_version":16,"compute_time_seconds":960,
 "settings":{"quota":{"logical_size_bytes":10737418240}}}}`
const branchesJSON = `{"branches":[{"id":"br-main","name":"main","default":true,"logical_size":2147483648,"current_state":"ready"},
 {"id":"br-dev","name":"dev","logical_size":0}]}`
const endpointsJSON = `{"endpoints":[
 {"id":"ep-rw","host":"ep-rw.eu-central-1.aws.neon.tech","branch_id":"br-main","type":"read_write","current_state":"active","autoscaling_limit_min_cu":0.25,"autoscaling_limit_max_cu":2,"region_id":"aws-eu-central-1"},
 {"id":"ep-ro","host":"ep-ro.eu-central-1.aws.neon.tech","branch_id":"br-main","type":"read_only","current_state":"idle","autoscaling_limit_min_cu":1,"autoscaling_limit_max_cu":1,"region_id":"aws-eu-central-1"},
 {"id":"ep-dev","host":"ep-dev.eu-central-1.aws.neon.tech","branch_id":"br-dev","type":"read_write","current_state":"idle","autoscaling_limit_min_cu":0.25,"autoscaling_limit_max_cu":0.25,"region_id":"aws-eu-central-1"}]}`

func fakeNeon(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer neon-key" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/projects":
			if r.URL.Query().Get("org_id") != "org-1" {
				t.Errorf("org_id missing: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(projectsJSON))
		case "/projects/proj-1":
			_, _ = w.Write([]byte(projectJSON))
		case "/projects/proj-1/branches":
			_, _ = w.Write([]byte(branchesJSON))
		case "/projects/proj-1/endpoints":
			_, _ = w.Write([]byte(endpointsJSON))
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestDiscoverAndCollect(t *testing.T) {
	srv := fakeNeon(t)
	defer srv.Close()
	p, err := New([]byte(`{"orgId":"org-1"}`), "neon-key\n", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Databases) != 3 {
		t.Fatalf("databases: %d", len(inv.Databases))
	}
	tests := []struct {
		i                          int
		name, class, state, branch string
		disk                       float64
	}{
		{0, "shop-main", "0.25–2 CU", "active", "main", 2},
		{1, "shop-main-ro-ep-ro", "1 CU", "idle", "main", 2},
		{2, "shop-dev", "0.25 CU", "idle", "dev", 0},
	}
	for _, tt := range tests {
		d := inv.Databases[tt.i]
		if d.Name != tt.name || d.InstanceClass != tt.class || d.Meta["state"] != tt.state || d.Meta["branch"] != tt.branch {
			t.Errorf("db %d: %+v", tt.i, d)
		}
		if d.Engine != "postgres" || d.Port != 5432 || d.Source != "Neon · aws-eu-central-1" || d.Version != "PostgreSQL 16" {
			t.Errorf("db %d: %+v", tt.i, d)
		}
		if (tt.disk == 0) != (d.DiskGB == nil) || (d.DiskGB != nil && *d.DiskGB != tt.disk) {
			t.Errorf("db %d disk: %v", tt.i, d.DiskGB)
		}
	}

	now := time.Date(2026, 10, 25, 12, 0, 30, 0, time.UTC)
	res, err := p.Collect(context.Background(), inv, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, s := range res.Samples {
		if !s.TS.Equal(now.Truncate(time.Minute)) {
			t.Fatalf("ts %v", s.TS)
		}
		got[s.Name+"|"+s.Attrs["trooper.db.id"]] = s.Value
	}
	want := map[string]float64{
		"trooper.db.disk|neon-shop-main":                       0.2,
		"trooper.db.disk|neon-shop-main-ro-ep-ro":              0.2,
		"trooper.cloud.neon.compute_active|neon-shop-main":     1,
		"trooper.cloud.neon.compute_active|neon-shop-dev":      0,
		"trooper.cloud.neon.compute_cu_max|neon-shop-main":     2,
		"trooper.cloud.neon.compute_time_seconds|":             960,
		"trooper.cloud.neon.logical_size_bytes|neon-shop-main": 2147483648,
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || math.Abs(g-v) > 1e-9 {
			t.Errorf("%s = %v (ok=%v), want %v", k, g, ok, v)
		}
	}
	if _, ok := got["trooper.db.disk|neon-shop-dev"]; ok {
		t.Error("dev branch has no size; disk must not be reported")
	}
	if res.DBMeta["neon-shop-main-ro-ep-ro"]["state"] != "idle" {
		t.Errorf("meta %v", res.DBMeta)
	}
}

func TestNewRequiresKey(t *testing.T) {
	if _, err := New(nil, "", ""); err == nil {
		t.Fatal("want error")
	}
}
