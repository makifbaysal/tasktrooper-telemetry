package cloudcheck

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const token = "tok_SUPERSECRET_123"

func bearerOK(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+token }

func TestVercel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !bearerOK(r) {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"error":{"code":"forbidden","message":"Not authorized: tok_SUPERSECRET_123"}}`)
			return
		}
		switch {
		case r.URL.Path == "/v2/user":
			fmt.Fprint(w, `{"user":{"username":"deniz"}}`)
		case r.URL.Path == "/v2/teams/team_x":
			fmt.Fprint(w, `{"slug":"shop"}`)
		case r.URL.Path == "/v9/projects":
			if r.URL.Query().Get("teamId") != "team_x" {
				t.Errorf("teamId missing: %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"projects":[{"name":"shop-web","framework":"nextjs"},{"name":"docs","framework":null}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	VercelBase = srv.URL
	tests := []struct {
		name, secret string
		ok           bool
		want         string
		resources    int
	}{
		{"ok", token, true, "deniz (team shop)", 2},
		{"rejected", "bad_token", false, "credentials rejected (HTTP 403)", 0},
		{"missing", "", false, "token is required", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Run(context.Background(), "vercel", json.RawMessage(`{"teamId":"team_x"}`), tc.secret)
			if r.OK != tc.ok || !strings.Contains(r.Message, tc.want) || len(r.Resources) != tc.resources {
				t.Fatalf("%+v", r)
			}
			if tc.ok && (r.Resources[0].Detail != "nextjs" || r.Resources[1].Detail != "Vercel project") {
				t.Fatalf("details %+v", r.Resources)
			}
		})
	}
}

func TestNeon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !bearerOK(r) {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"message":"authentication failed"}`)
			return
		}
		if r.URL.Path != "/api/v2/projects" || r.URL.Query().Get("limit") != "1" || r.URL.Query().Get("org_id") != "org-x" {
			t.Errorf("unexpected %s", r.URL)
		}
		fmt.Fprint(w, `{"projects":[{"id":"bold-fog-123","name":"analytics","region_id":"aws-eu-central-1"}]}`)
	}))
	defer srv.Close()
	NeonBase = srv.URL
	for _, tc := range []struct {
		secret string
		ok     bool
		want   string
	}{{token, true, "accepted"}, {"nope", false, "credentials rejected (HTTP 401) — authentication failed"}} {
		r := Run(context.Background(), "neon", json.RawMessage(`{"orgId":"org-x"}`), tc.secret)
		if r.OK != tc.ok || !strings.Contains(r.Message, tc.want) {
			t.Fatalf("%+v", r)
		}
		if tc.ok && (len(r.Resources) != 1 || r.Resources[0].Detail != "Neon · aws-eu-central-1") {
			t.Fatalf("%+v", r.Resources)
		}
	}
}

func saKey(t *testing.T, tokenURI string) string {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: must(x509.MarshalPKCS8PrivateKey(k))})
	b, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "shop-prod", "client_email": "tt@shop-prod.iam.gserviceaccount.com",
		"private_key_id": "abc", "private_key": string(p), "token_uri": tokenURI,
	})
	return string(b)
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

func TestGCP(t *testing.T) {
	var reject bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if reject {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":"invalid_grant","error_description":"Invalid JWT Signature."}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"ya29.x","token_type":"Bearer","expires_in":3600}`)
		case "/v1/projects/shop-prod", "/v1/projects/other":
			if r.Header.Get("Authorization") != "Bearer ya29.x" {
				w.WriteHeader(401)
				return
			}
			if strings.HasSuffix(r.URL.Path, "other") {
				w.WriteHeader(403)
				fmt.Fprint(w, `{"error":{"message":"permission denied"}}`)
				return
			}
			fmt.Fprint(w, `{"projectId":"shop-prod","name":"Shop","lifecycleState":"ACTIVE"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	GCPResourceManagerBase = srv.URL
	key := saKey(t, srv.URL+"/token")
	var sa saJSON
	_ = json.Unmarshal([]byte(key), &sa)

	tests := []struct {
		name, cfg, secret string
		reject, ok        bool
		want              string
	}{
		{"ok", `{}`, key, false, true, "Authenticated as tt@shop-prod"},
		{"forbidden project", `{"projectIds":["other"]}`, key, false, false, "credentials rejected (HTTP 403) — permission denied"},
		{"bad grant", `{}`, key, true, false, "token exchange"},
		{"not json", `{}`, "xyz", false, false, "not valid service account JSON"},
		{"wrong type", `{}`, `{"type":"authorized_user"}`, false, false, "type service_account"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reject = tc.reject
			r := Run(context.Background(), "gcp", json.RawMessage(tc.cfg), tc.secret)
			if r.OK != tc.ok || !strings.Contains(r.Message, tc.want) {
				t.Fatalf("%+v", r)
			}
			if strings.Contains(r.Message, "PRIVATE KEY") || strings.Contains(r.Message, sa.PrivateKey) {
				t.Fatal("private key leaked")
			}
			if tc.ok && (len(r.Resources) != 1 || r.Resources[0].Name != "shop-prod") {
				t.Fatalf("%+v", r.Resources)
			}
		})
	}
}

const stsNS = "https://sts.amazonaws.com/doc/2011-06-15/"

func TestAWS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		auth := r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/xml")
		if strings.Contains(auth, "Credential=AKIDBAD/") {
			w.WriteHeader(403)
			fmt.Fprintf(w, `<ErrorResponse xmlns="%s"><Error><Type>Sender</Type><Code>InvalidClientTokenId</Code><Message>The security token included in the request is invalid.</Message></Error><RequestId>1</RequestId></ErrorResponse>`, stsNS)
			return
		}
		switch r.Form.Get("Action") {
		case "GetCallerIdentity":
			arn := "arn:aws:iam::123456789012:user/tt"
			if strings.Contains(auth, "Credential=ASIAROLE/") {
				arn = "arn:aws:sts::123456789012:assumed-role/TaskTrooperReadOnly/tasktrooper-test"
			}
			fmt.Fprintf(w, `<GetCallerIdentityResponse xmlns="%s"><GetCallerIdentityResult><Arn>%s</Arn><UserId>X</UserId><Account>123456789012</Account></GetCallerIdentityResult><ResponseMetadata><RequestId>1</RequestId></ResponseMetadata></GetCallerIdentityResponse>`, stsNS, arn)
		case "AssumeRole":
			if r.Form.Get("ExternalId") != "ext-1" || r.Form.Get("RoleSessionName") != "tasktrooper-test" {
				w.WriteHeader(403)
				fmt.Fprintf(w, `<ErrorResponse xmlns="%s"><Error><Type>Sender</Type><Code>AccessDenied</Code><Message>not authorized to perform sts:AssumeRole</Message></Error><RequestId>1</RequestId></ErrorResponse>`, stsNS)
				return
			}
			fmt.Fprintf(w, `<AssumeRoleResponse xmlns="%s"><AssumeRoleResult><Credentials><AccessKeyId>ASIAROLE</AccessKeyId><SecretAccessKey>ROLESECRETvalue</SecretAccessKey><SessionToken>ROLESESSIONtoken</SessionToken><Expiration>2030-01-01T00:00:00Z</Expiration></Credentials><AssumedRoleUser><Arn>a</Arn><AssumedRoleId>b</AssumedRoleId></AssumedRoleUser></AssumeRoleResult><ResponseMetadata><RequestId>1</RequestId></ResponseMetadata></AssumeRoleResponse>`, stsNS)
		default:
			w.WriteHeader(400)
		}
	}))
	defer srv.Close()
	AWSEndpoint = srv.URL
	defer func() { AWSEndpoint = "" }()

	good := `{"accessKeyId":"AKIDGOOD","secretAccessKey":"wJalrSECRETkey"}`
	tests := []struct {
		name, cfg, secret string
		ok                bool
		want              string
	}{
		{"keys", `{"regions":["eu-central-1"]}`, good, true, "Authenticated as arn:aws:iam::123456789012:user/tt"},
		{"colon form", `{}`, "AKIDGOOD:wJalrSECRETkey", true, "user/tt"},
		{"assume role", `{"roleArn":"arn:aws:iam::123456789012:role/TaskTrooperReadOnly","externalId":"ext-1"}`, good, true, "assumed-role/TaskTrooperReadOnly/tasktrooper-test"},
		{"assume denied", `{"roleArn":"arn:aws:iam::123456789012:role/X","externalId":"wrong"}`, good, false, "AccessDenied"},
		{"bad key", `{}`, `{"accessKeyId":"AKIDBAD","secretAccessKey":"wJalrSECRETkey"}`, false, "InvalidClientTokenId"},
		{"malformed", `{}`, "garbage", false, "secret must be JSON"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Run(context.Background(), "aws", json.RawMessage(tc.cfg), tc.secret)
			if r.OK != tc.ok || !strings.Contains(r.Message, tc.want) {
				t.Fatalf("%+v", r)
			}
			for _, s := range []string{"wJalrSECRETkey", "ROLESECRETvalue", "ROLESESSIONtoken"} {
				if strings.Contains(r.Message, s) {
					t.Fatalf("secret %s leaked: %s", s, r.Message)
				}
			}
		})
	}
}

func TestUnknownProviderAndScrub(t *testing.T) {
	if r := Run(context.Background(), "oracle", nil, "x"); r.OK || !strings.Contains(r.Message, "unknown provider") {
		t.Fatalf("%+v", r)
	}
	if r := Run(context.Background(), "neon", json.RawMessage(`[1]`), token); r.OK || !strings.Contains(r.Message, "config") {
		t.Fatalf("%+v", r)
	}
	// a provider echoing the token back must never surface it
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprintf(w, `{"message":"bad token %s"}`, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	}))
	defer srv.Close()
	NeonBase = srv.URL
	r := Run(context.Background(), "neon", nil, token)
	if r.OK || strings.Contains(r.Message, token) || !strings.Contains(r.Message, "[redacted]") {
		t.Fatalf("%+v", r)
	}
}

func TestAzure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/tenant-1/oauth2/v2.0/token":
			_ = r.ParseForm()
			if r.Form.Get("client_secret") != "s3cret" || r.Form.Get("client_id") != "app-1" || r.Form.Get("grant_type") != "client_credentials" {
				w.WriteHeader(401)
				fmt.Fprint(w, `{"error_description":"AADSTS7000215: Invalid client secret s3cret-wrong"}`)
				return
			}
			fmt.Fprint(w, `{"access_token":"`+token+`"}`)
		case r.URL.Path == "/subscriptions" && bearerOK(r):
			fmt.Fprint(w, `{"value":[{"subscriptionId":"sub-1","displayName":"Prod","state":"Enabled"}]}`)
		default:
			w.WriteHeader(403)
		}
	}))
	defer srv.Close()
	oldL, oldA := AzureLoginBase, AzureARMBase
	AzureLoginBase, AzureARMBase = srv.URL, srv.URL
	defer func() { AzureLoginBase, AzureARMBase = oldL, oldA }()
	cfg := json.RawMessage(`{"tenantId":"tenant-1","clientId":"app-1","subscriptionIds":["sub-1"]}`)
	res := Run(context.Background(), "azure", cfg, "s3cret")
	if !res.OK || len(res.Resources) != 1 || res.Resources[0].Name != "Prod" {
		t.Fatalf("ok: %+v", res)
	}
	res = Run(context.Background(), "azure", cfg, "s3cret-wrong")
	if res.OK || strings.Contains(res.Message, "s3cret-wrong") || !strings.Contains(res.Message, "AADSTS7000215") {
		t.Fatalf("bad secret: %+v", res)
	}
	res = Run(context.Background(), "azure", json.RawMessage(`{"tenantId":"tenant-1","clientId":"app-1","subscriptionIds":["nope"]}`), "s3cret")
	if res.OK {
		t.Fatalf("unknown subscription: %+v", res)
	}
}
