// Package cloudcheck runs real, minimal credential checks for cloud integrations (ADR-012): AWS STS
// GetCallerIdentity (+ AssumeRole), a GCP service-account token + one Resource Manager call, Vercel /v2/user and
// Neon /api/v2/projects. Secrets are never echoed: every message is scrubbed before it is returned.
package cloudcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
)

// Overridable endpoints (tests).
var (
	VercelBase             = "https://api.vercel.com"
	NeonBase               = "https://console.neon.tech"
	GCPResourceManagerBase = "https://cloudresourcemanager.googleapis.com"
	// AzureLoginBase / AzureARMBase: Entra ID token endpoint and Azure Resource Manager (TT_AZURE_LOGIN_URL,
	// TT_AZURE_ARM_URL override them for tests).
	AzureLoginBase = envOr("TT_AZURE_LOGIN_URL", "https://login.microsoftonline.com")
	AzureARMBase   = envOr("TT_AZURE_ARM_URL", "https://management.azure.com")
	// AWSEndpoint is the STS BaseEndpoint; "" = the SDK default for the region.
	AWSEndpoint = ""
	// Timeout bounds one check.
	Timeout = 10 * time.Second
)

type cfg struct {
	RoleArn    string   `json:"roleArn"`
	ExternalID string   `json:"externalId"`
	Regions    []string `json:"regions"`
	ProjectIDs []string `json:"projectIds"`
	TeamID     string   `json:"teamId"`
	OrgID      string   `json:"orgId"`
	// azure
	TenantID        string   `json:"tenantId"`
	ClientID        string   `json:"clientId"`
	SubscriptionIDs []string `json:"subscriptionIds"`
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return strings.TrimRight(v, "/")
	}
	return def
}

// Run performs a real, minimal credential check for a provider. Timeout 10 s. Never echoes the secret.
func Run(ctx context.Context, provider string, config json.RawMessage, secret string) model.IntegrationTestResult {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	var c cfg
	if len(config) > 0 && string(config) != "null" {
		if err := json.Unmarshal(config, &c); err != nil {
			return fail("config must be a JSON object")
		}
	}
	secrets := []string{secret}
	var res model.IntegrationTestResult
	switch provider {
	case "aws":
		res = checkAWS(ctx, c, secret, &secrets)
	case "gcp":
		res = checkGCP(ctx, c, secret, &secrets)
	case "vercel":
		res = checkVercel(ctx, c, secret)
	case "neon":
		res = checkNeon(ctx, c, secret)
	case "azure":
		res = checkAzure(ctx, c, secret)
	default:
		return fail(fmt.Sprintf("unknown provider %q (aws|gcp|vercel|neon|azure)", provider))
	}
	res.Message = scrub(res.Message, secrets)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) && !res.OK {
		res.Message = "timed out after " + Timeout.String() + ": " + res.Message
	}
	return res
}

func fail(msg string) model.IntegrationTestResult {
	return model.IntegrationTestResult{OK: false, Message: msg}
}

// scrub removes every secret fragment (≥ 4 chars) from msg.
func scrub(msg string, secrets []string) string {
	for _, s := range secrets {
		s = strings.TrimSpace(s)
		if len(s) >= 4 {
			msg = strings.ReplaceAll(msg, s, "[redacted]")
		}
	}
	return msg
}

func statusMsg(code int) string {
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		return fmt.Sprintf("credentials rejected (HTTP %d)", code)
	}
	return fmt.Sprintf("HTTP %d", code)
}

// ---------------------------------------------------------------- AWS

type awsSecret struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	SessionToken    string `json:"sessionToken"`
}

func parseAWSSecret(s string) (awsSecret, error) {
	var k awsSecret
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "{") {
		if err := json.Unmarshal([]byte(s), &k); err != nil {
			return k, errors.New("secret must be JSON {accessKeyId, secretAccessKey, sessionToken?} or ACCESS_KEY_ID:SECRET")
		}
	} else if id, sec, ok := strings.Cut(s, ":"); ok {
		k.AccessKeyID, k.SecretAccessKey = id, sec
	} else {
		return k, errors.New("secret must be JSON {accessKeyId, secretAccessKey, sessionToken?} or ACCESS_KEY_ID:SECRET")
	}
	if k.AccessKeyID == "" || k.SecretAccessKey == "" {
		return k, errors.New("secret needs accessKeyId and secretAccessKey")
	}
	return k, nil
}

func awsErr(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		msg := ae.ErrorCode()
		if m := ae.ErrorMessage(); m != "" {
			msg += ": " + truncate(m, 200)
		}
		var re *smithyhttp.ResponseError
		if errors.As(err, &re) && (re.HTTPStatusCode() == 401 || re.HTTPStatusCode() == 403) {
			return statusMsg(re.HTTPStatusCode()) + " — " + msg
		}
		return msg
	}
	return truncate(err.Error(), 300)
}

func checkAWS(ctx context.Context, c cfg, secret string, secrets *[]string) model.IntegrationTestResult {
	region := "us-east-1"
	if len(c.Regions) > 0 && c.Regions[0] != "" {
		region = c.Regions[0]
	}
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	if strings.TrimSpace(secret) != "" {
		k, err := parseAWSSecret(secret)
		if err != nil {
			return fail(err.Error())
		}
		*secrets = append(*secrets, k.SecretAccessKey, k.SessionToken)
		opts = append(opts, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(k.AccessKeyID, k.SecretAccessKey, k.SessionToken)))
	}
	ac, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return fail("aws config: " + err.Error())
	}
	stsOpts := func(o *sts.Options) {
		if AWSEndpoint != "" {
			o.BaseEndpoint = aws.String(AWSEndpoint)
		}
	}
	client := sts.NewFromConfig(ac, stsOpts)
	id, err := client.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fail("STS GetCallerIdentity failed: " + awsErr(err))
	}
	who := aws.ToString(id.Arn)
	if c.RoleArn == "" {
		return model.IntegrationTestResult{OK: true, Message: "Authenticated as " + who}
	}
	in := &sts.AssumeRoleInput{RoleArn: aws.String(c.RoleArn), RoleSessionName: aws.String("tasktrooper-test"), DurationSeconds: aws.Int32(900)}
	if c.ExternalID != "" {
		in.ExternalId = aws.String(c.ExternalID)
	}
	ar, err := client.AssumeRole(ctx, in)
	if err != nil {
		return fail(fmt.Sprintf("Authenticated as %s, but AssumeRole %s failed: %s", who, c.RoleArn, awsErr(err)))
	}
	cr := ar.Credentials
	*secrets = append(*secrets, aws.ToString(cr.SecretAccessKey), aws.ToString(cr.SessionToken))
	ac2 := ac.Copy()
	ac2.Credentials = credentials.NewStaticCredentialsProvider(aws.ToString(cr.AccessKeyId), aws.ToString(cr.SecretAccessKey), aws.ToString(cr.SessionToken))
	id2, err := sts.NewFromConfig(ac2, stsOpts).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fail("AssumeRole succeeded but GetCallerIdentity with the role failed: " + awsErr(err))
	}
	return model.IntegrationTestResult{OK: true, Message: "Authenticated as " + aws.ToString(id2.Arn)}
}

// ---------------------------------------------------------------- GCP

type saJSON struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
}

func checkGCP(ctx context.Context, c cfg, secret string, secrets *[]string) model.IntegrationTestResult {
	if strings.TrimSpace(secret) == "" {
		return fail("a service account JSON key is required")
	}
	var sa saJSON
	if err := json.Unmarshal([]byte(secret), &sa); err != nil {
		return fail("secret is not valid service account JSON")
	}
	*secrets = append(*secrets, sa.PrivateKey)
	if sa.Type != "service_account" || sa.ClientEmail == "" || sa.PrivateKey == "" {
		return fail("secret must be a service account key (type service_account with client_email and private_key)")
	}
	creds, err := google.CredentialsFromJSON(ctx, []byte(secret), "https://www.googleapis.com/auth/cloud-platform.read-only")
	if err != nil {
		return fail("service account key rejected: " + truncate(err.Error(), 200))
	}
	if _, err := creds.TokenSource.Token(); err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.Response != nil {
			msg := statusMsg(re.Response.StatusCode)
			if re.ErrorCode != "" {
				msg += " — " + re.ErrorCode
			}
			return fail("token exchange for " + sa.ClientEmail + " failed: " + msg)
		}
		return fail("token exchange for " + sa.ClientEmail + " failed: " + truncate(err.Error(), 200))
	}
	project := sa.ProjectID
	if len(c.ProjectIDs) > 0 && c.ProjectIDs[0] != "" {
		project = c.ProjectIDs[0]
	}
	if project == "" {
		return model.IntegrationTestResult{OK: true, Message: "Authenticated as " + sa.ClientEmail + " (no project to check)"}
	}
	hc := oauth2.NewClient(ctx, creds.TokenSource)
	var p struct {
		ProjectID      string `json:"projectId"`
		Name           string `json:"name"`
		LifecycleState string `json:"lifecycleState"`
	}
	code, err := getJSON(ctx, hc, GCPResourceManagerBase+"/v1/projects/"+url.PathEscape(project), "", &p)
	if err != nil {
		return fail(fmt.Sprintf("Authenticated as %s, but reading project %s failed: %s", sa.ClientEmail, project, errMsg(code, err)))
	}
	detail := "Google Cloud project"
	if p.Name != "" {
		detail += " · " + p.Name
	}
	if p.ProjectID == "" {
		p.ProjectID = project
	}
	return model.IntegrationTestResult{OK: true, Message: "Authenticated as " + sa.ClientEmail,
		Resources: []model.DiscoveredResource{{Kind: "project", Name: p.ProjectID, Detail: detail}}}
}

// ---------------------------------------------------------------- Vercel

func checkVercel(ctx context.Context, c cfg, secret string) model.IntegrationTestResult {
	if strings.TrimSpace(secret) == "" {
		return fail("a Vercel access token is required")
	}
	hc := http.DefaultClient
	var u struct {
		User struct {
			Username string `json:"username"`
			Email    string `json:"email"`
		} `json:"user"`
	}
	if code, err := getJSON(ctx, hc, VercelBase+"/v2/user", secret, &u); err != nil {
		return fail("Vercel token check failed: " + errMsg(code, err))
	}
	who := u.User.Username
	if who == "" {
		who = u.User.Email
	}
	q := url.Values{"limit": {"20"}}
	if c.TeamID != "" {
		var t struct {
			Name string `json:"name"`
			Slug string `json:"slug"`
		}
		if code, err := getJSON(ctx, hc, VercelBase+"/v2/teams/"+url.PathEscape(c.TeamID), secret, &t); err != nil {
			return fail(fmt.Sprintf("Authenticated as %s, but team %s is not accessible: %s", who, c.TeamID, errMsg(code, err)))
		}
		who += " (team " + firstNonEmpty(t.Slug, t.Name, c.TeamID) + ")"
		q.Set("teamId", c.TeamID)
	}
	var ps struct {
		Projects []struct {
			Name      string  `json:"name"`
			Framework *string `json:"framework"`
		} `json:"projects"`
	}
	if code, err := getJSON(ctx, hc, VercelBase+"/v9/projects?"+q.Encode(), secret, &ps); err != nil {
		return fail(fmt.Sprintf("Authenticated as %s, but listing projects failed: %s", who, errMsg(code, err)))
	}
	res := model.IntegrationTestResult{OK: true, Message: fmt.Sprintf("Authenticated as %s · %d projects", who, len(ps.Projects))}
	for _, p := range ps.Projects {
		d := "Vercel project"
		if p.Framework != nil && *p.Framework != "" {
			d = *p.Framework
		}
		res.Resources = append(res.Resources, model.DiscoveredResource{Kind: "project", Name: p.Name, Detail: d})
	}
	return res
}

// ---------------------------------------------------------------- Neon

func checkNeon(ctx context.Context, c cfg, secret string) model.IntegrationTestResult {
	if strings.TrimSpace(secret) == "" {
		return fail("a Neon API key is required")
	}
	q := url.Values{"limit": {"1"}}
	if c.OrgID != "" {
		q.Set("org_id", c.OrgID)
	}
	var ps struct {
		Projects []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			RegionID string `json:"region_id"`
		} `json:"projects"`
	}
	if code, err := getJSON(ctx, http.DefaultClient, NeonBase+"/api/v2/projects?"+q.Encode(), secret, &ps); err != nil {
		return fail("Neon API key check failed: " + errMsg(code, err))
	}
	res := model.IntegrationTestResult{OK: true, Message: "Neon API key accepted"}
	for _, p := range ps.Projects {
		res.Resources = append(res.Resources, model.DiscoveredResource{Kind: "project", Name: firstNonEmpty(p.Name, p.ID), Detail: "Neon · " + p.RegionID})
	}
	return res
}

// checkAzure gets a client_credentials token for the service principal and reads each configured subscription
// (Reader is enough).
func checkAzure(ctx context.Context, c cfg, secret string) model.IntegrationTestResult {
	if strings.TrimSpace(c.TenantID) == "" || strings.TrimSpace(c.ClientID) == "" {
		return fail("config.tenantId and config.clientId are required")
	}
	if strings.TrimSpace(secret) == "" {
		return fail("the service principal client secret is required")
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {c.ClientID}, "client_secret": {secret},
		"scope": {"https://management.azure.com/.default"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, AzureLoginBase+"/"+url.PathEscape(c.TenantID)+"/oauth2/v2.0/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return fail(err.Error())
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fail("Azure sign-in failed: " + truncate(err.Error(), 200))
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &tok)
	if resp.StatusCode/100 != 2 || tok.AccessToken == "" {
		msg := statusMsg(resp.StatusCode)
		if tok.Error != "" {
			msg += " — " + truncate(tok.Error, 200)
		}
		return fail("Azure sign-in failed: " + msg)
	}
	var subs struct {
		Value []struct {
			SubscriptionID string `json:"subscriptionId"`
			DisplayName    string `json:"displayName"`
			State          string `json:"state"`
		} `json:"value"`
	}
	if code, err := getJSON(ctx, http.DefaultClient, AzureARMBase+"/subscriptions?api-version=2022-12-01", tok.AccessToken, &subs); err != nil {
		return fail("listing subscriptions failed: " + errMsg(code, err))
	}
	visible := map[string]bool{}
	res := model.IntegrationTestResult{OK: true, Message: "Azure service principal accepted"}
	for _, s := range subs.Value {
		visible[s.SubscriptionID] = true
		res.Resources = append(res.Resources, model.DiscoveredResource{Kind: "subscription", RefID: s.SubscriptionID,
			Name: firstNonEmpty(s.DisplayName, s.SubscriptionID), Detail: "Azure · " + s.State})
	}
	for _, want := range c.SubscriptionIDs {
		if !visible[want] {
			return fail("subscription " + want + " is not visible to the service principal (assign Reader + Monitoring Reader)")
		}
	}
	if len(res.Resources) == 0 {
		return fail("the service principal can read no subscription (assign Reader + Monitoring Reader)")
	}
	return res
}

// ---------------------------------------------------------------- HTTP helpers

type httpErr struct {
	code int
	msg  string
}

func (e httpErr) Error() string { return e.msg }

func errMsg(code int, err error) string {
	var he httpErr
	if errors.As(err, &he) {
		s := statusMsg(he.code)
		if he.msg != "" {
			s += " — " + he.msg
		}
		return s
	}
	_ = code
	return truncate(err.Error(), 200)
}

// getJSON GETs u (Bearer token when set) and decodes a 2xx body into v.
func getJSON(ctx context.Context, hc *http.Client, u, token string, v any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return 0, fmt.Errorf("%s %s: %w", ue.Op, redactURL(ue.URL), ue.Err)
		}
		return 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, httpErr{code: resp.StatusCode, msg: providerMessage(body)}
	}
	if v != nil {
		if err := json.Unmarshal(body, v); err != nil {
			return resp.StatusCode, errors.New("unexpected response (not JSON)")
		}
	}
	return resp.StatusCode, nil
}

func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return "request"
	}
	u.RawQuery = ""
	return u.String()
}

// providerMessage extracts a short error message from common JSON error shapes.
func providerMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
		Error   any    `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	if e.Message != "" {
		return truncate(e.Message, 160)
	}
	switch x := e.Error.(type) {
	case string:
		return truncate(x, 160)
	case map[string]any:
		if m, ok := x["message"].(string); ok {
			return truncate(m, 160)
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
