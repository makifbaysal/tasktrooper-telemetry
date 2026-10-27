package store

import "testing"

func TestUsesRepoWebhook(t *testing.T) {
	for _, tc := range []struct {
		provider string
		inst     int64
		want     bool
	}{
		{"gitlab", 0, true}, {"bitbucket", 0, true}, {"github", 0, true}, {"github", 42, false}, {"local", 0, false},
	} {
		if got := usesRepoWebhook(tc.provider, tc.inst); got != tc.want {
			t.Errorf("%s/%d: %v", tc.provider, tc.inst, got)
		}
	}
}

func TestBranchName(t *testing.T) {
	for name, ok := range map[string]bool{"main": true, "release/2.x": true, "feature_1.2": true, "-x": false, "a b": false, "": false, "/x": false} {
		if branchName.MatchString(name) != ok {
			t.Errorf("%q", name)
		}
	}
}
