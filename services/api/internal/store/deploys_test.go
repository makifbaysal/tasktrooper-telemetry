package store

import (
	"errors"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
)

func TestDeployRows(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		in      model.DeployInput
		wantErr bool
		n       int
		check   func(DeployRow) bool
	}{
		{"defaults", model.DeployInput{Service: "api", CommitSHA: "abc123"}, false, 1, func(r DeployRow) bool {
			return r.Version == "abc123" && r.Env == "production" && r.Ts.Equal(now) && r.ExternalID == "api@production@abc123" && r.Message == ""
		}},
		{"services deduped", model.DeployInput{Service: "api", Services: []string{"api", " web ", ""}, Version: "v1"}, false, 2, nil},
		{"sha appended to message", model.DeployInput{Service: "api", Version: "v1.2", CommitSHA: "deadbeef", Message: "fix"}, false, 1,
			func(r DeployRow) bool { return r.Message == "fix (deadbeef)" }},
		{"ts ms", model.DeployInput{Service: "api", Version: "v1", Ts: now.Add(-time.Hour).UnixMilli()}, false, 1,
			func(r DeployRow) bool { return r.Ts.Equal(now.Add(-time.Hour)) }},
		{"time rfc3339 + env", model.DeployInput{Service: "api", Version: "v1", Time: "2026-11-03T10:00:00Z", Environment: "staging"}, false, 1,
			func(r DeployRow) bool { return r.Ts.Hour() == 10 && r.Env == "staging" }},
		{"no service", model.DeployInput{Version: "v1"}, true, 0, nil},
		{"no version", model.DeployInput{Service: "api"}, true, 0, nil},
		{"bad time", model.DeployInput{Service: "api", Version: "v1", Time: "yesterday"}, true, 0, nil},
		{"future", model.DeployInput{Service: "api", Version: "v1", Ts: now.Add(2 * time.Hour).UnixMilli()}, true, 0, nil},
		{"bad url", model.DeployInput{Service: "api", Version: "v1", URL: "javascript:alert(1)"}, true, 0, nil},
	} {
		rows, err := DeployRows(tc.in, now)
		var inv ErrInvalid
		if tc.wantErr {
			if !errors.As(err, &inv) {
				t.Errorf("%s: err %v, want ErrInvalid", tc.name, err)
			}
			continue
		}
		if err != nil || len(rows) != tc.n {
			t.Errorf("%s: %v rows=%d", tc.name, err, len(rows))
			continue
		}
		if tc.check != nil && !tc.check(rows[0]) {
			t.Errorf("%s: %+v", tc.name, rows[0])
		}
	}
}
