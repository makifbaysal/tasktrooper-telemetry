package secrets

import (
	"context"
	"testing"
)

type memStore struct{ n, c []byte }

func (m memStore) LoadSecret(context.Context, int64, int64) ([]byte, []byte, error) { return m.n, m.c, nil }

func TestRoundTrip(t *testing.T) {
	n, c, err := Seal(devKey, 7, []byte("tok"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(context.Background(), "secret:1", 7, devKey, memStore{n, c})
	if err != nil || string(got) != "tok" {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := Open(devKey, 8, n, c); err == nil {
		t.Fatal("other project must not decrypt")
	}
}

func TestEnvAndBadRef(t *testing.T) {
	t.Setenv("TT_X", "v")
	for _, tc := range []struct {
		ref, want string
		ok        bool
	}{{"env:TT_X", "v", true}, {"env:NOPE_TT", "", false}, {"vault:x", "", false}, {"x", "", false}} {
		got, err := Resolve(context.Background(), tc.ref, 1, devKey, nil)
		if (err == nil) != tc.ok || string(got) != tc.want {
			t.Errorf("%s: %q %v", tc.ref, got, err)
		}
	}
}
