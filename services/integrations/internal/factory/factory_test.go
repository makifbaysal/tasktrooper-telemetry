package factory

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/integrations/internal/fakes"
	"github.com/tasktrooper/telemetry/services/integrations/internal/model"
)

// The api stores the Vercel token and the drain secret as two plain sealed secrets (not a JSON blob).
func TestVercelTokenAndDrainSecret(t *testing.T) {
	t.Setenv("TT_TEST_VERCEL_TOKEN", "tok_123\n")
	t.Setenv("TT_TEST_DRAIN_SECRET", "drain-s3cret")
	var auth, drainBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		switch {
		case r.URL.Path == "/v10/projects":
			_, _ = io.WriteString(w, `{"projects":[{"id":"p","name":"web"}],"pagination":{"next":null}}`)
		case r.URL.Path == "/v1/drains" && r.Method == "GET":
			_, _ = io.WriteString(w, `{"drains":[]}`)
		case r.URL.Path == "/v1/drains":
			b, _ := io.ReadAll(r.Body)
			drainBody = string(b)
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer srv.Close()
	f := &Factory{Endpoints: Endpoints{Vercel: srv.URL}, IngestKeys: map[int64]string{3: "ik"}, PublicIngestURL: "https://in.example"}
	p, err := f.Build(context.Background(), model.Integration{ID: 9, ProjectID: 3, Provider: "vercel", CredentialsRef: "env:TT_TEST_VERCEL_TOKEN",
		Config: []byte(`{"teamId":"t","createDrain":true,"drainSecretRef":"env:TT_TEST_DRAIN_SECRET","syncRequestedAt":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tok_123" {
		t.Fatalf("auth %q", auth)
	}
	if want := `"secret":"drain-s3cret"`; !strings.Contains(drainBody, want) {
		t.Fatalf("drain body %s", drainBody)
	}
}

func TestAzureProvider(t *testing.T) {
	f := fakes.NewAzure(t, time.Now())
	defer f.Close()
	t.Setenv("TT_TEST_AZURE_SECRET", "s3cret-value")
	fac := &Factory{Endpoints: Endpoints{AzureLogin: f.URL, AzureARM: f.URL}}
	p, err := fac.Build(context.Background(), model.Integration{ID: 1, ProjectID: 3, Provider: "azure", CredentialsRef: "env:TT_TEST_AZURE_SECRET",
		Config: []byte(`{"tenantId":"tenant-1","clientId":"client-1","subscriptionIds":["sub-1"]}`)})
	if err != nil {
		t.Fatal(err)
	}
	inv, err := p.Discover(context.Background())
	if err != nil || len(inv.Functions) != 5 { // 2 apps + 3 App Insights function rows
		t.Fatalf("inv %v err %v", inv, err)
	}
}
