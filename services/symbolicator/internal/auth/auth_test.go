package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestAuthenticator(t *testing.T) {
	calls := 0
	good := sha256.Sum256([]byte("tt_dev_api_key"))
	down := false
	a := New(func(_ context.Context, h []byte) (int64, error) {
		calls++
		if down {
			return 0, errors.New("db down")
		}
		if string(h) == string(good[:]) {
			return 1, nil
		}
		return 0, ErrUnauthorized
	}, time.Minute)
	now := time.Unix(1000, 0)
	a.now = func() time.Time { return now }
	ctx := context.Background()

	for _, tc := range []struct {
		key       string
		project   int64
		err       error
		wantCalls int
	}{
		{"tt_dev_api_key", 1, nil, 1},
		{"tt_dev_api_key", 1, nil, 1}, // cached
		{"nope", 0, ErrUnauthorized, 2},
		{"nope", 0, ErrUnauthorized, 2}, // negative cached
		{"", 0, ErrUnauthorized, 2},
	} {
		p, err := a.Project(ctx, tc.key)
		if p != tc.project || !errors.Is(err, tc.err) && err != tc.err || calls != tc.wantCalls {
			t.Errorf("Project(%q) = %d %v calls=%d", tc.key, p, err, calls)
		}
	}
	now = now.Add(2 * time.Minute)
	down = true
	if _, err := a.Project(ctx, "tt_dev_api_key"); err == nil || errors.Is(err, ErrUnauthorized) {
		t.Errorf("infra error not surfaced: %v", err)
	}
}

func TestBearerKey(t *testing.T) {
	for in, want := range map[string]string{"Bearer abc": "abc", "bearer  abc ": "abc", "Basic abc": "", "": ""} {
		r, _ := http.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", in)
		if got := BearerKey(r); got != want {
			t.Errorf("BearerKey(%q) = %q", in, got)
		}
	}
}
