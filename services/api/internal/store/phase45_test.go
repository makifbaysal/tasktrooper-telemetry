package store

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestHistogramQuantile(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    map[float64]uint64
		q    float64
		want float64
	}{
		{"empty", nil, 0.95, 0},
		{"all zero", map[float64]uint64{10: 0}, 0.95, 0},
		{"single bucket interpolates from 0", map[float64]uint64{100: 10}, 0.5, 50},
		{"second bucket", map[float64]uint64{10: 50, 20: 50}, 0.95, 19},
		{"empty buckets skipped", map[float64]uint64{10: 100, 20: 0, 50: 0}, 0.95, 9.5},
		{"+Inf reports lower bound", map[float64]uint64{10: 1, math.Inf(1): 99}, 0.95, 10},
	} {
		if got := HistogramQuantile(tc.h, tc.q); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNormalizeHost(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		err      bool
	}{
		{"shop.example.com", "shop.example.com", false},
		{"https://Shop.Example.com:443/checkout", "shop.example.com", false},
		{"shop.example.com:8080", "shop.example.com", false},
		{"shop.example.com.", "shop.example.com", false},
		{"", "", true},
		{"*.example.com", "", true},
		{"user@example.com", "", true},
		{"shop.example.com/x", "", true},
	} {
		got, err := NormalizeHost(tc.in)
		if (err != nil) != tc.err || got != tc.want {
			t.Errorf("NormalizeHost(%q) = %q, %v", tc.in, got, err)
		}
		var inv ErrInvalid
		if err != nil && !errors.As(err, &inv) {
			t.Errorf("NormalizeHost(%q): error %v is not ErrInvalid", tc.in, err)
		}
	}
}

func TestUICriticality(t *testing.T) {
	for in, want := range map[string]string{"critical": "critical", "high": "important", "medium": "important", "important": "important", "low": "low", "": "low"} {
		if got := UICriticality(in); got != want {
			t.Errorf("UICriticality(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepoStatus(t *testing.T) {
	t0 := time.Unix(1000, 0)
	t1 := t0.Add(time.Minute)
	for _, tc := range []struct {
		name               string
		status, err        string
		requested, analyzd *time.Time
		want               string
	}{
		{"new repo", "pending", "", &t0, nil, "analyzing"},
		{"analyzed", "ready", "", &t0, &t1, "ready"},
		{"re-analysis requested", "ready", "", &t1, &t0, "analyzing"},
		{"request wins over old error", "error", "boom", &t1, &t0, "analyzing"},
		{"error", "error", "boom", &t0, &t1, "error"},
		{"running", "analyzing", "", nil, &t0, "analyzing"},
		{"disconnected wins", "disconnected", "", &t1, &t0, "disconnected"},
	} {
		if got := repoStatus(tc.status, tc.err, tc.requested, tc.analyzd); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestRoles(t *testing.T) {
	if !RoleAtLeast("owner", "admin") || !RoleAtLeast("member", "member") || RoleAtLeast("viewer", "member") || RoleAtLeast("admin", "bogus") || RoleAtLeast("", "viewer") {
		t.Error("RoleAtLeast ordering")
	}
	if ValidRole("root") || !ValidRole("viewer") {
		t.Error("ValidRole")
	}
}
