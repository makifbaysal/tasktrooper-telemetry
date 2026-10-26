package tq

import (
	"context"
	"errors"
	"testing"

	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

func TestBind(t *testing.T) {
	ctx := tenant.With(context.Background(), tenant.Tenant{OrgID: 3, ProjectID: 7})
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		flavor  Flavor
		sql     string
		want    string
		wantErr error
	}{
		{"ch plain", ctx, ClickHouse, "SELECT 1 FROM red_1m WHERE {{tenant}} AND x = ?", "SELECT 1 FROM red_1m WHERE org_id = 3 AND project_id = 7 AND x = ?", nil},
		{"ch alias twice", ctx, ClickHouse, "SELECT * FROM a JOIN b ON 1 WHERE {{tenant:a}} AND {{tenant:b}}",
			"SELECT * FROM a JOIN b ON 1 WHERE a.org_id = 3 AND a.project_id = 7 AND b.org_id = 3 AND b.project_id = 7", nil},
		{"pg", ctx, Postgres, "SELECT * FROM monitors WHERE {{tenant}} AND id = $1", "SELECT * FROM monitors WHERE project_id = 7 AND id = $1", nil},
		{"pg alias", ctx, Postgres, "SELECT * FROM incidents i WHERE {{tenant:i}}", "SELECT * FROM incidents i WHERE i.project_id = 7", nil},
		{"pg insert literal", ctx, Postgres, "INSERT INTO kpis (project_id, name) VALUES ({{project}}, $1)", "INSERT INTO kpis (project_id, name) VALUES (7, $1)", nil},
		{"ch insert literal", ctx, ClickHouse, "INSERT INTO t VALUES ({{org}}, {{project}})", "INSERT INTO t VALUES (3, 7)", nil},
		{"missing marker", ctx, ClickHouse, "SELECT * FROM spans", "", ErrNoTenantFilter},
		{"missing marker pg", ctx, Postgres, "DELETE FROM monitors WHERE id = $1", "", ErrNoTenantFilter},
		{"no tenant", context.Background(), ClickHouse, "SELECT 1 WHERE {{tenant}}", "", tenant.ErrNoTenant},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Bind(tc.ctx, tc.flavor, tc.sql)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestBindZeroProject(t *testing.T) {
	ctx := tenant.With(context.Background(), tenant.Tenant{OrgID: 1})
	if _, err := Bind(ctx, Postgres, "SELECT 1 WHERE {{tenant}}"); err == nil {
		t.Fatal("expected error for project 0")
	}
}

func TestInList(t *testing.T) {
	if got := InList(3, 3); got != "$3,$4,$5" {
		t.Error(got)
	}
}
