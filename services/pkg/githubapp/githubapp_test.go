package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	return k, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func TestJWTVerifies(t *testing.T) {
	k, p := testKey(t)
	c, err := New(42, p, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	c.Now = func() time.Time { return now }
	jwt, err := c.JWT()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatal(jwt)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&k.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(b, &claims)
	if claims["iss"] != "42" || int64(claims["iat"].(float64)) != now.Unix()-60 || int64(claims["exp"].(float64)) != now.Add(9*time.Minute).Unix() {
		t.Fatalf("claims %v", claims)
	}
	// PKCS#1 and escaped newlines also parse.
	p1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	if _, err := ParseKey([]byte(strings.ReplaceAll(string(p1), "\n", `\n`))); err != nil {
		t.Fatal(err)
	}
}

func TestInstallationTokenCacheAndRepos(t *testing.T) {
	_, p := testKey(t)
	var tokenCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/7/access_tokens":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				w.WriteHeader(401)
				return
			}
			tokenCalls.Add(1)
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case r.URL.Path == "/installation/repositories":
			if r.Header.Get("Authorization") != "token ghs_x" {
				w.WriteHeader(401)
				return
			}
			if r.URL.Query().Get("page") == "1" {
				var repos []string
				for i := range 100 {
					repos = append(repos, fmt.Sprintf(`{"id":%d,"full_name":"acme/r%d","default_branch":"main"}`, i, i))
				}
				fmt.Fprintf(w, `{"total_count":101,"repositories":[%s]}`, strings.Join(repos, ","))
				return
			}
			fmt.Fprint(w, `{"total_count":101,"repositories":[{"id":100,"full_name":"acme/last","default_branch":"dev","private":true}]}`)
		default:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		}
	}))
	defer srv.Close()
	c, _ := New(1, p, srv.URL)
	ctx := context.Background()
	for range 3 {
		if tok, err := c.InstallationToken(ctx, 7); err != nil || tok.Token != "ghs_x" {
			t.Fatal(tok, err)
		}
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("token not cached: %d calls", tokenCalls.Load())
	}
	repos, err := c.InstallationRepos(ctx, 7)
	if err != nil || len(repos) != 101 || repos[100].FullName != "acme/last" || !repos[100].Private {
		t.Fatalf("repos %d %v", len(repos), err)
	}
	if _, err := c.InstallationToken(ctx, 8); err == nil {
		t.Fatal("unknown installation must fail")
	} else if ae, ok := err.(*APIError); !ok || ae.Status != 404 {
		t.Fatalf("err %v", err)
	}
}

func TestSignatureAndCloneURL(t *testing.T) {
	body := []byte(`{"zen":"x"}`)
	h := Sign([]byte("s3cret"), body)
	if !VerifySignature([]byte("s3cret"), body, h) || VerifySignature([]byte("other"), body, h) || VerifySignature(nil, body, h) ||
		VerifySignature([]byte("s3cret"), body, "sha1=abc") {
		t.Fatal("signature")
	}
	if CloneURL("", "acme/shop") != "https://github.com/acme/shop.git" || CloneURL("https://ghe.corp/api/v3", "a/b") != "https://ghe.corp/a/b.git" {
		t.Fatal("clone url")
	}
	if !strings.HasPrefix(GitAuthHeader("t"), "Authorization: Basic ") {
		t.Fatal("auth header")
	}
}
