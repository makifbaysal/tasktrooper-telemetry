package symbolicate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

func TestSymbolicateAuth(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_ = json.NewEncoder(w).Encode(Result{Stack: "sym", Frames: []model.Frame{{Fn: "f"}}})
	}))
	defer srv.Close()
	ten := tenant.With(context.Background(), tenant.Tenant{OrgID: 7, ProjectID: 42})
	for _, tc := range []struct {
		name, authz, token string
		ctx                context.Context
		wantAuthz, wantInt string
		wantProject        string
		wantErr            bool
	}{
		{"bearer forwarded", "Bearer tt_key", "secret", ten, "Bearer tt_key", "", "", false},
		{"session → internal credential", "", "secret", ten, "", "secret", "42", false},
		{"non-bearer header ignored → internal", "Basic x", "secret", ten, "", "secret", "42", false},
		{"no token, no bearer → anonymous", "", "", ten, "", "", "", false},
		{"internal without tenant fails", "", "secret", context.Background(), "", "", "", true},
	} {
		got = nil
		c := New(srv.URL)
		c.InternalToken = tc.token
		_, _, ok, err := c.Symbolicate(tc.ctx, tc.authz, Request{Stack: "raw"})
		if tc.wantErr {
			if err == nil || ok || got != nil {
				t.Errorf("%s: want error before the call, ok=%v err=%v", tc.name, ok, err)
			}
			continue
		}
		if err != nil || !ok {
			t.Errorf("%s: ok=%v err=%v", tc.name, ok, err)
			continue
		}
		if got.Get("Authorization") != tc.wantAuthz || got.Get(HeaderInternal) != tc.wantInt || got.Get(HeaderProject) != tc.wantProject {
			t.Errorf("%s: headers %v", tc.name, got)
		}
	}
}
