package config

import (
	"strings"
	"testing"
)

// TestLoad is the table for ADR-025 fail-fast: every config mistake must be
// caught here with a message naming the variable, not later as a tool error.
func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string // "" = must load
		check   func(t *testing.T, c Config)
	}{
		{
			name: "defaults with a key",
			env:  map[string]string{"TT_API_KEY": "tt_api_x"},
			check: func(t *testing.T, c Config) {
				if c.APIURL != "http://localhost:8080" {
					t.Errorf("APIURL = %q", c.APIURL)
				}
			},
		},
		{
			name:    "missing key",
			env:     map[string]string{},
			wantErr: "TT_API_KEY",
		},
		{
			name:    "bad URL",
			env:     map[string]string{"TT_API_KEY": "k", "TT_API_URL": "not a url"},
			wantErr: "TT_API_URL",
		},
		{
			name: "trailing slash trimmed",
			env:  map[string]string{"TT_API_KEY": "k", "TT_API_URL": "http://api:8080/"},
			check: func(t *testing.T, c Config) {
				if c.APIURL != "http://api:8080" {
					t.Errorf("APIURL = %q", c.APIURL)
				}
			},
		},
		{
			name:    "bad org id",
			env:     map[string]string{"TT_API_KEY": "k", "TT_ORG_ID": "abc"},
			wantErr: "TT_ORG_ID",
		},
		{
			name:    "bad project id",
			env:     map[string]string{"TT_API_KEY": "k", "TT_PROJECT_ID": "-1"},
			wantErr: "TT_PROJECT_ID",
		},
		{
			name: "ids parsed when set",
			env:  map[string]string{"TT_API_KEY": "k", "TT_ORG_ID": "1", "TT_PROJECT_ID": "7"},
			check: func(t *testing.T, c Config) {
				if c.OrgID != 1 || c.ProjectID != 7 {
					t.Errorf("OrgID, ProjectID = %d, %d", c.OrgID, c.ProjectID)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Only the TT_* keys this test manages are cleared, so a developer's
			// other env (PATH, GOCACHE) stays intact.
			for k := range tc.env {
				t.Setenv(k, tc.env[k])
			}
			for _, k := range []string{"TT_API_KEY", "TT_API_URL", "TT_ORG_ID", "TT_PROJECT_ID", "TT_LOG_LEVEL"} {
				if _, ok := tc.env[k]; !ok {
					t.Setenv(k, "")
				}
			}
			c, err := Load()
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error mentioning %s, got config %+v", tc.wantErr, c)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not mention %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if tc.check != nil {
				tc.check(t, c)
			}
		})
	}
}
