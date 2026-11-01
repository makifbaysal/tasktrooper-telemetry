package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"
)

type memStore struct{ n, c []byte }

func (m memStore) LoadSecret(context.Context, int64, int64) ([]byte, []byte, error) {
	return m.n, m.c, nil
}

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

func TestKeyProductionFailsClosed(t *testing.T) {
	dev := base64.StdEncoding.EncodeToString(devKey)
	real := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	for _, tc := range []struct {
		name, env, key string
		wantErr        error // nil = success
	}{
		{"dev unset falls back", "", "", nil},
		{"dev explicit dev key", "development", dev, nil},
		{"prod unset", "production", "", ErrDevKeyInProduction},
		{"prod dev key", "Production", dev, ErrDevKeyInProduction},
		{"prod real key", "production", real, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TT_ENV", tc.env)
			t.Setenv("TT_SECRETS_KEY", tc.key)
			k, err := Key()
			if !errors.Is(err, tc.wantErr) || (err == nil && len(k) != 32) {
				t.Fatalf("Key() = %d bytes, %v; want err %v", len(k), err, tc.wantErr)
			}
		})
	}
}
