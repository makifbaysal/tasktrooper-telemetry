package store

import (
	"testing"
	"time"
)

func TestSuppressionReason(t *testing.T) {
	s := Suppression{
		Muted: map[int64]time.Time{7: time.Now().Add(time.Hour)},
		Windows: []Maintenance{
			{ID: 1, ProjectID: 10}, // whole project 10
			{ID: 2, ProjectID: 20, Services: []string{"orders-api"}}, // one service of project 20
			{ID: 3, ProjectID: 20, MonitorIDs: []int64{99}},
		},
	}
	tests := []struct {
		name    string
		project int64
		monitor int64
		service string
		want    string
	}{
		{"muted wins", 10, 7, "x", "muted"},
		{"project-wide window", 10, 8, "", "maintenance:1"},
		{"kpi in project-wide window", 10, 0, "orders-api", "maintenance:1"},
		{"service window, case-insensitive", 20, 5, "Orders-API", "maintenance:2"},
		{"service window, other service", 20, 5, "pay-api", ""},
		{"monitor window", 20, 99, "pay-api", "maintenance:3"},
		{"kpi not matched by monitor list", 20, 0, "pay-api", ""},
		{"other project", 30, 7, "orders-api", "muted"},
		{"nothing", 30, 8, "orders-api", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.Reason(tc.project, tc.monitor, tc.service); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}
