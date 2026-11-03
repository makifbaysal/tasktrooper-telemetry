package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/tasktrooper/telemetry/services/api/internal/auth"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
)

type fakePasswordStore struct {
	hash string // current hash of the user ("" = none / unknown)
	got  *store.SetPasswordInput
}

func (f *fakePasswordStore) PasswordHash(context.Context, string) (int64, string, error) {
	if f.hash == "" {
		return 0, "", store.ErrNotFound
	}
	return 9, f.hash, nil
}

func (f *fakePasswordStore) SetPassword(_ context.Context, in store.SetPasswordInput) (*store.SetPasswordResult, error) {
	f.got = &in
	return &store.SetPasswordResult{UserID: 9, OrgID: 3, Role: "viewer", PasswordChanged: in.Hash != ""}, nil
}

func TestSetPasswordUsage(t *testing.T) {
	for _, tt := range []struct {
		name  string
		args  []string
		stdin string
	}{
		{name: "no args"},
		{name: "no stdin flag", args: []string{"--email", "a@b.c"}, stdin: "long enough password"},
		{name: "no email", args: []string{"--password-stdin"}, stdin: "long enough password"},
		{name: "owner role", args: []string{"--email", "a@b.c", "--role", "owner", "--password-stdin"}, stdin: "long enough password"},
		{name: "org and project", args: []string{"--email", "a@b.c", "--org", "x", "--project", "4", "--password-stdin"}, stdin: "long enough password"},
		{name: "short password", args: []string{"--email", "a@b.c", "--password-stdin"}, stdin: "short\n"},
		{name: "empty stdin", args: []string{"--email", "a@b.c", "--password-stdin"}},
		{name: "extra arg", args: []string{"--email", "a@b.c", "--password-stdin", "pw"}, stdin: "long enough password"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := setPasswordMain(tt.args, strings.NewReader(tt.stdin), &out, &errb); code != 2 || out.Len() != 0 {
				t.Errorf("exit %d, stdout %q, stderr %q", code, out.String(), errb.String())
			}
			if strings.Contains(errb.String(), "long enough") {
				t.Errorf("stderr leaks the password: %q", errb.String())
			}
		})
	}
}

func TestReadPassword(t *testing.T) {
	for in, want := range map[string]string{"pw\n": "pw", "pw\r\n": "pw", "pw": "pw", "p w \nsecond line": "p w "} {
		if got, err := readPassword(strings.NewReader(in)); err != nil || got != want {
			t.Errorf("readPassword(%q) = %q, %v", in, got, err)
		}
	}
}

func TestRunSetPassword(t *testing.T) {
	const pw = "correct horse battery"
	existing, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		current     string
		opts        setPasswordOpts
		pw          string
		wantErr     bool
		wantNewHash bool
		wantOut     string
	}{
		{name: "new user", opts: setPasswordOpts{Email: "demo@example.com", Name: "Demo", Org: "TaskTrooper", Role: "viewer"}, pw: pw,
			wantNewHash: true, wantOut: "user_id=9\norg_id=3\nrole=viewer\npassword=changed\n"},
		{name: "same password keeps the hash", current: existing, opts: setPasswordOpts{Email: "demo@example.com"}, pw: pw,
			wantOut: "user_id=9\norg_id=3\nrole=viewer\npassword=unchanged\n"},
		{name: "new password replaces the hash", current: existing, opts: setPasswordOpts{Email: "demo@example.com"}, pw: "another long password", wantNewHash: true},
		{name: "too short", opts: setPasswordOpts{Email: "demo@example.com"}, pw: "short", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakePasswordStore{hash: tt.current}
			var out, errb bytes.Buffer
			err := runSetPassword(context.Background(), f, tt.opts, tt.pw, &out, &errb)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err %v", err)
			}
			if tt.wantErr {
				if f.got != nil {
					t.Error("store written despite the error")
				}
				return
			}
			if f.got.Email != tt.opts.Email || f.got.Org != tt.opts.Org || f.got.Role != tt.opts.Role || f.got.Name != tt.opts.Name {
				t.Errorf("input: %+v", f.got)
			}
			if (f.got.Hash != "") != tt.wantNewHash {
				t.Errorf("hash set = %v, want %v", f.got.Hash != "", tt.wantNewHash)
			}
			if tt.wantNewHash && auth.CheckPassword(f.got.Hash, tt.pw) != nil {
				t.Error("stored hash does not verify the password")
			}
			if tt.wantOut != "" && out.String() != tt.wantOut {
				t.Errorf("stdout %q, want %q", out.String(), tt.wantOut)
			}
			if strings.Contains(out.String()+errb.String(), tt.pw) {
				t.Error("output leaks the password")
			}
		})
	}
}
