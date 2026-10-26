// Package factory builds a provider for an integration row: resolves credentials_ref (secret: / env: /
// file:, services/pkg/secrets) and applies endpoint overrides.
package factory

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tasktrooper/telemetry/services/pkg/secrets"

	"github.com/tasktrooper/telemetry/services/integrations/internal/model"
	"github.com/tasktrooper/telemetry/services/integrations/internal/provider/aws"
	"github.com/tasktrooper/telemetry/services/integrations/internal/provider/gcp"
	"github.com/tasktrooper/telemetry/services/integrations/internal/provider/neon"
	"github.com/tasktrooper/telemetry/services/integrations/internal/provider/vercel"
)

// Endpoints override provider API bases (fake providers in tests; empty = real APIs).
type Endpoints struct {
	AWS    string // every AWS service
	GCP    string // <base>/sql, /redis, /pubsub, /mon
	Neon   string
	Vercel string
}

// Factory resolves credentials and builds providers.
type Factory struct {
	Key       []byte        // TT_SECRETS_KEY
	Store     secrets.Store // secrets table
	Endpoints Endpoints
	// IngestKeys / PublicIngestURL are used to create Vercel drains (config.createDrain).
	IngestKeys      map[int64]string
	PublicIngestURL string
}

func (f *Factory) resolve(ctx context.Context, ref string, project int64) ([]byte, error) {
	if ref == "" {
		return nil, nil
	}
	b, err := secrets.Resolve(ctx, ref, project, f.Key, f.Store)
	if err != nil {
		// Never include the plaintext; the ref itself (secret:<id>, env:NAME) is not secret.
		return nil, fmt.Errorf("credentials %s: %w", ref, err)
	}
	return b, nil
}

// Build implements runner.Factory.
func (f *Factory) Build(ctx context.Context, in model.Integration) (model.Provider, error) {
	cred, err := f.resolve(ctx, in.CredentialsRef, in.ProjectID)
	if err != nil {
		return nil, err
	}
	switch in.Provider {
	case "aws":
		return aws.New(ctx, in.Config, cred, aws.Options{Endpoint: f.Endpoints.AWS})
	case "gcp":
		ep := gcp.Endpoints{}
		if b := f.Endpoints.GCP; b != "" {
			ep = gcp.Endpoints{SQLAdmin: b + "/sql", Redis: b + "/redis", PubSub: b + "/pubsub", Monitoring: b + "/mon", NoAuth: true}
		}
		return gcp.New(ctx, in.Config, cred, ep)
	case "neon":
		return neon.New(in.Config, string(cred), f.Endpoints.Neon)
	case "vercel":
		var cfg vercel.Config
		_ = json.Unmarshal(in.Config, &cfg)
		o := vercel.Options{Base: f.Endpoints.Vercel, IntegrationID: in.ID, PublicIngestURL: f.PublicIngestURL, IngestKey: f.IngestKeys[in.ProjectID]}
		if cfg.CreateDrain && cfg.DrainSecretRef != "" {
			s, err := f.resolve(ctx, cfg.DrainSecretRef, in.ProjectID)
			if err != nil {
				return nil, fmt.Errorf("drain secret: %w", err)
			}
			o.DrainSecret = string(s)
		}
		return vercel.New(in.Config, string(cred), o)
	}
	return nil, fmt.Errorf("unknown provider %q", in.Provider)
}
