package scrape

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/twmb/franz-go/pkg/sasl"
	saslaws "github.com/twmb/franz-go/pkg/sasl/aws"
	"github.com/twmb/franz-go/pkg/sasl/oauth"

	"github.com/tasktrooper/telemetry/services/collector/internal/config"
)

// awsIAMMechanism builds the AWS_MSK_IAM SASL mechanism. Credentials:
//   - credentialsRef resolving to "accessKeyId:secretAccessKey[:sessionToken]" (static), else
//   - the default AWS chain (env, shared config / awsProfile, web identity (IRSA), ECS, EC2 IMDS).
//
// With roleArn the base credentials assume that role (STS, cached and refreshed by the SDK).
// The signing region is derived by franz-go from the broker host name (b-1.x.kafka.<region>.amazonaws.com).
func awsIAMMechanism(t config.Broker) (sasl.Mechanism, error) {
	secret, err := config.ResolveSecret(t.CredentialsRef)
	if err != nil {
		return nil, err
	}
	var provider awssdk.CredentialsProvider
	if secret != "" {
		parts := strings.SplitN(secret, ":", 3)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return nil, errors.New("kafka iam: credentialsRef must be accessKeyId:secretAccessKey[:sessionToken] (or leave it empty for the default AWS chain)")
		}
		st := awssdk.Credentials{AccessKeyID: parts[0], SecretAccessKey: parts[1], Source: "trooper-agent"}
		if len(parts) == 3 {
			st.SessionToken = parts[2]
		}
		provider = awssdk.CredentialsProviderFunc(func(context.Context) (awssdk.Credentials, error) { return st, nil })
	}
	var opts []func(*awsconfig.LoadOptions) error
	if t.Auth.AWSRegion != "" {
		opts = append(opts, awsconfig.WithRegion(t.Auth.AWSRegion))
	}
	if t.Auth.AWSProfile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(t.Auth.AWSProfile))
	}
	if provider != nil {
		opts = append(opts, awsconfig.WithCredentialsProvider(provider))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("kafka iam: aws config: %w", err)
	}
	creds := cfg.Credentials
	if t.Auth.RoleARN != "" {
		creds = awssdk.NewCredentialsCache(stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), t.Auth.RoleARN,
			func(o *stscreds.AssumeRoleOptions) { o.RoleSessionName = "trooper-agent" }))
	}
	if creds == nil {
		return nil, errors.New("kafka iam: no AWS credentials found (credentialsRef or default chain)")
	}
	return saslaws.ManagedStreamingIAM(func(ctx context.Context) (saslaws.Auth, error) {
		c, err := creds.Retrieve(ctx)
		if err != nil {
			return saslaws.Auth{}, fmt.Errorf("aws credentials: %w", err)
		}
		return saslaws.Auth{AccessKey: c.AccessKeyID, SecretKey: c.SecretAccessKey, SessionToken: c.SessionToken, UserAgent: "trooper-agent"}, nil
	}), nil
}

// oauthMechanism builds the OAUTHBEARER mechanism: a static token (credentialsRef) or the OAuth 2
// client-credentials grant against auth.tokenUrl (credentialsRef "clientId:clientSecret"), cached
// until 30 s before expiry.
func oauthMechanism(t config.Broker) (sasl.Mechanism, error) {
	secret, err := config.ResolveSecret(t.CredentialsRef)
	if err != nil {
		return nil, err
	}
	ext := t.Auth.Extensions
	if t.Auth.TokenURL == "" {
		if secret == "" {
			return nil, errors.New("kafka oauthbearer: credentialsRef (token) or auth.tokenUrl is required")
		}
		return oauth.Auth{Token: secret, Extensions: ext}.AsMechanism(), nil
	}
	id, sec, ok := strings.Cut(secret, ":")
	if !ok || id == "" {
		return nil, errors.New("kafka oauthbearer: credentialsRef must be clientId:clientSecret when tokenUrl is set")
	}
	ts := &tokenSource{url: t.Auth.TokenURL, clientID: id, secret: sec, scopes: t.Auth.Scopes, hc: &http.Client{Timeout: 15 * time.Second}}
	return oauth.Oauth(func(ctx context.Context) (oauth.Auth, error) {
		tok, err := ts.Token(ctx)
		if err != nil {
			return oauth.Auth{}, err
		}
		return oauth.Auth{Token: tok, Extensions: ext}, nil
	}), nil
}

// tokenSource implements the client-credentials grant (RFC 6749 §4.4) with caching.
type tokenSource struct {
	url, clientID, secret string
	scopes                []string
	hc                    *http.Client
	now                   func() time.Time

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// Token returns a cached token or fetches a new one.
func (s *tokenSource) Token(ctx context.Context) (string, error) {
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && now().Before(s.expiry.Add(-30*time.Second)) {
		return s.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	if len(s.scopes) > 0 {
		form.Set("scope", strings.Join(s.scopes, " "))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(url.QueryEscape(s.clientID), url.QueryEscape(s.secret))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("oauth token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oauth token: %s %s", resp.Status, strings.TrimSpace(string(body[:min(len(body), 256)])))
	}
	var tr struct {
		AccessToken string  `json:"access_token"`
		ExpiresIn   float64 `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" {
		return "", fmt.Errorf("oauth token: no access_token in response (%v)", err)
	}
	if tr.ExpiresIn <= 0 {
		tr.ExpiresIn = 300
	}
	s.token = tr.AccessToken
	s.expiry = now().Add(time.Duration(tr.ExpiresIn * float64(time.Second)))
	return s.token, nil
}
