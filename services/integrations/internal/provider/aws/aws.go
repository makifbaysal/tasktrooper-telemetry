// Package aws discovers RDS/Aurora, ElastiCache, MSK and SQS resources and reads their CloudWatch metrics
// with GetMetricData (contracts §5, §10; mapping table in README.md).
package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	"github.com/aws/aws-sdk-go-v2/service/kafka"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// Config is integrations.config for provider aws.
type Config struct {
	RoleArn     string   `json:"roleArn"`
	ExternalID  string   `json:"externalId"`
	Regions     []string `json:"regions"`
	Services    []string `json:"services"` // rds | elasticache | msk | sqs (empty = all)
	Env         string   `json:"env"`
	SessionName string   `json:"sessionName"`
	// SQSConsumers maps a queue name to the consumer group shown in the UI (default: queue name).
	SQSConsumers map[string]string `json:"sqsConsumers"`
	// InstanceMemoryGB overrides/extends the RDS instance class → memory table (trooper.db.memory).
	InstanceMemoryGB map[string]float64 `json:"instanceMemoryGb"`
	// MaxQueues caps SQS queues per region (default 1000).
	MaxQueues int `json:"maxQueues"`
}

// AccessKey is the JSON stored behind credentials_ref for access-key auth.
type AccessKey struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	SessionToken    string `json:"sessionToken"`
}

// Options are runtime settings (tests).
type Options struct {
	// Endpoint overrides every service endpoint (fake AWS in tests); static test credentials are used.
	Endpoint   string
	HTTPClient *http.Client
}

type clients struct {
	region string
	rds    *rds.Client
	ec     *elasticache.Client
	msk    *kafka.Client
	sqs    *sqs.Client
	cw     *cloudwatch.Client
}

// Provider implements model.Provider.
type Provider struct {
	cfg     Config
	regions []*clients
}

// New resolves credentials (access key JSON and/or AssumeRole with external id) and builds per-region clients.
func New(ctx context.Context, raw json.RawMessage, secret []byte, o Options) (*Provider, error) {
	var cfg Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("aws config: %w", err)
		}
	}
	if cfg.MaxQueues <= 0 {
		cfg.MaxQueues = 1000
	}
	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRetryer(func() sdk.Retryer {
			return retry.NewStandard(func(so *retry.StandardOptions) { so.MaxAttempts = 5; so.MaxBackoff = 20 * time.Second })
		}),
	}
	if o.HTTPClient != nil {
		loadOpts = append(loadOpts, awsconfig.WithHTTPClient(o.HTTPClient))
	}
	switch {
	case o.Endpoint != "":
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")))
	case len(secret) > 0:
		var k AccessKey
		if err := json.Unmarshal(secret, &k); err != nil || k.AccessKeyID == "" || k.SecretAccessKey == "" {
			return nil, errors.New(`aws: credentials must be JSON {"accessKeyId","secretAccessKey"}`)
		}
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(k.AccessKeyID, k.SecretAccessKey, k.SessionToken)))
	}
	if len(cfg.Regions) > 0 {
		loadOpts = append(loadOpts, awsconfig.WithRegion(cfg.Regions[0]))
	}
	base, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	if len(cfg.Regions) == 0 {
		if base.Region == "" {
			return nil, errors.New("aws: config.regions is empty and no default region is set")
		}
		cfg.Regions = []string{base.Region}
	}
	if cfg.RoleArn != "" && o.Endpoint == "" {
		stsClient := sts.NewFromConfig(base)
		name := cfg.SessionName
		if name == "" {
			name = "tasktrooper-cloud"
		}
		base.Credentials = sdk.NewCredentialsCache(stscreds.NewAssumeRoleProvider(stsClient, cfg.RoleArn, func(ao *stscreds.AssumeRoleOptions) {
			ao.RoleSessionName = name
			if cfg.ExternalID != "" {
				ao.ExternalID = sdk.String(cfg.ExternalID)
			}
		}))
	}
	p := &Provider{cfg: cfg}
	for _, region := range cfg.Regions {
		c := base.Copy()
		c.Region = region
		var ep *string
		if o.Endpoint != "" {
			ep = sdk.String(o.Endpoint)
		}
		p.regions = append(p.regions, &clients{
			region: region,
			rds:    rds.NewFromConfig(c, func(x *rds.Options) { x.BaseEndpoint = ep }),
			ec:     elasticache.NewFromConfig(c, func(x *elasticache.Options) { x.BaseEndpoint = ep }),
			msk:    kafka.NewFromConfig(c, func(x *kafka.Options) { x.BaseEndpoint = ep }),
			sqs:    sqs.NewFromConfig(c, func(x *sqs.Options) { x.BaseEndpoint = ep }),
			cw:     cloudwatch.NewFromConfig(c, func(x *cloudwatch.Options) { x.BaseEndpoint = ep }),
		})
	}
	return p, nil
}

func (p *Provider) enabled(s string) bool {
	return len(p.cfg.Services) == 0 || slices.Contains(p.cfg.Services, s)
}

// wrap prefixes an AWS error with region/service (AWS errors never contain credentials).
func wrap(region, what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s %s: %s", region, what, strings.TrimSpace(err.Error()))
}
