package aiquota

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParsePrices(t *testing.T) {
	tests := []struct {
		name, list, def string
		model           string
		want            Price
		wantErr         bool
	}{
		{"defaults", "", "", "any", Price{3, 15}, false},
		{"exact", "claude-a=1/5,claude-b=15/75", "", "claude-b", Price{15, 75}, false},
		{"prefix", "claude-a=1/5", "", "claude-a-20250101", Price{1, 5}, false},
		{"longest prefix", "claude=9/9,claude-a=1/5", "", "claude-a-x", Price{1, 5}, false},
		{"unknown → default", "claude-a=1/5", "0.8/4", "other", Price{0.8, 4}, false},
		{"spaces", " m = 2 / 10 , ", "", "m", Price{2, 10}, false},
		{"bad entry", "m", "", "", Price{}, true},
		{"bad price", "m=1", "", "", Price{}, true},
		{"negative", "m=-1/2", "", "", Price{}, true},
		{"bad default", "", "x", "", Price{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := ParsePrices(tt.list, tt.def)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err %v", err)
			}
			if err == nil && p.For(tt.model) != tt.want {
				t.Fatalf("got %+v want %+v", p.For(tt.model), tt.want)
			}
		})
	}
}

func TestCredits(t *testing.T) {
	p, _ := ParsePrices("m=3/15", "")
	tests := []struct {
		name string
		got  float64
		want float64
	}{
		{"1M in", p.Credits("m", 1_000_000, 0), 300},
		{"1M out", p.Credits("m", 0, 1_000_000), 1500},
		{"mixed", p.Credits("m", 10_000, 2_000), 6},                                                  // 0.03 + 0.03 USD
		{"cache", p.CreditsFor("m", Tokens{Input: 1000, CacheWrite: 1000, CacheRead: 10000}), 0.975}, // (3000+3750+3000)/1e6 USD
		{"estimate", p.Estimate("m", 4001, 1000), round4((1001*3 + 1000*15) / 1e6 / 0.01)},
		{"zero", p.Credits("m", 0, 0), 0},
	}
	for _, tt := range tests {
		if math.Abs(tt.got-tt.want) > 1e-9 {
			t.Errorf("%s: got %v want %v", tt.name, tt.got, tt.want)
		}
	}
}

func TestPlans(t *testing.T) {
	def, err := ParsePlans("")
	if err != nil {
		t.Fatal(err)
	}
	custom, err := ParsePlans("free=50, Team=1000")
	if err != nil {
		t.Fatal(err)
	}
	ptr := func(v int64) *int64 { return &v }
	tests := []struct {
		name     string
		plans    Plans
		plan     string
		override *int64
		want     int64
	}{
		{"free", def, "free", nil, 100},
		{"team", def, "team", nil, 5000},
		{"enterprise unlimited", def, "enterprise", nil, Unlimited},
		{"unknown → free", def, "gold", nil, 100},
		{"override", def, "free", ptr(250), 250},
		{"override unlimited", def, "team", ptr(-1), Unlimited},
		{"override zero", def, "team", ptr(0), 0},
		{"custom", custom, "team", nil, 1000},
		{"custom keeps enterprise default", custom, "enterprise", nil, Unlimited},
	}
	for _, tt := range tests {
		if got := tt.plans.OrgLimit(tt.plan, tt.override); got != tt.want {
			t.Errorf("%s: got %d want %d", tt.name, got, tt.want)
		}
	}
	for _, bad := range []string{"free", "free=x", "=5", "free=-2"} {
		if _, err := ParsePlans(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
	if MemberLimit(nil) != Unlimited || MemberLimit(ptr(-1)) != Unlimited || MemberLimit(ptr(7)) != 7 {
		t.Error("member limit")
	}
	if LimitPtr(-1) != nil || *LimitPtr(5) != 5 {
		t.Error("limit ptr")
	}
}

func TestPeriod(t *testing.T) {
	loc := time.FixedZone("x", 3*3600)
	tests := []struct {
		in            time.Time
		start, resets string
		period        string
	}{
		{time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC), "2026-10-01", "2026-11-01", "2026-10"},
		{time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC), "2026-12-01", "2027-01-01", "2026-12"},
		{time.Date(2026, 11, 1, 1, 0, 0, 0, loc), "2026-10-01", "2026-11-01", "2026-10"}, // still October in UTC
	}
	for _, tt := range tests {
		if got := MonthStart(tt.in).Format(time.DateOnly); got != tt.start {
			t.Errorf("%v start %s", tt.in, got)
		}
		if got := ResetsAt(tt.in).Format(time.DateOnly); got != tt.resets {
			t.Errorf("%v resets %s", tt.in, got)
		}
		if got := Period(tt.in); got != tt.period {
			t.Errorf("%v period %s", tt.in, got)
		}
	}
	if p, err := ParsePeriod("2026-02"); err != nil || !p.Equal(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("parse %v %v", p, err)
	}
	if _, err := ParsePeriod("2026-13"); err == nil {
		t.Fatal("want error")
	}
}

func TestDecideAndCrossed(t *testing.T) {
	tests := []struct {
		name    string
		s       Status
		est     float64
		allowed bool
		reason  string
	}{
		{"under", Status{OrgUsed: 10, OrgLimit: 100, UserLimit: Unlimited}, 5, true, ""},
		{"exactly fits", Status{OrgUsed: 95, OrgLimit: 100, UserLimit: Unlimited}, 5, true, ""},
		{"org over", Status{OrgUsed: 96, OrgLimit: 100, UserLimit: Unlimited}, 5, false, ReasonOrgLimit},
		{"org unlimited", Status{OrgUsed: 1e9, OrgLimit: Unlimited, UserLimit: Unlimited}, 5, true, ""},
		{"user over", Status{OrgUsed: 10, OrgLimit: 100, UserUsed: 9, UserLimit: 10}, 2, false, ReasonUserLimit},
		{"org wins", Status{OrgUsed: 100, OrgLimit: 100, UserUsed: 9, UserLimit: 10}, 2, false, ReasonOrgLimit},
		{"zero limit", Status{OrgLimit: 0, UserLimit: Unlimited}, 0.01, false, ReasonOrgLimit},
	}
	for _, tt := range tests {
		d := Decide(tt.s, tt.est)
		if d.Allowed != tt.allowed || d.Reason != tt.reason {
			t.Errorf("%s: %+v", tt.name, d)
		}
	}
	crossed := []struct {
		used  float64
		limit int64
		want  string
	}{
		{79.9, 100, ""}, {80, 100, "80"}, {4012, 5000, "80"}, {100, 100, "80,100"}, {130, 100, "80,100"},
		{1, 0, "80,100"}, {0, 0, ""}, {1e9, Unlimited, ""},
	}
	for _, c := range crossed {
		var s []string
		for _, x := range Crossed(c.used, c.limit) {
			s = append(s, strconv.Itoa(x))
		}
		if got := strings.Join(s, ","); got != c.want {
			t.Errorf("crossed(%v, %d) = %q want %q", c.used, c.limit, got, c.want)
		}
	}
}

func TestFormatCredits(t *testing.T) {
	tests := map[float64]string{0: "0", 999: "999", 1000: "1,000", 4012.4: "4,012", 1234567: "1,234,567", -1500: "-1,500"}
	for in, want := range tests {
		if got := FormatCredits(in); got != want {
			t.Errorf("%v: %q want %q", in, got, want)
		}
	}
}

// fakeDB answers UsageSQL from fixed values and records Exec calls.
type fakeDB struct {
	plan        string
	orgOverride *int64
	orgUsed     float64
	memberCap   *int64
	userUsed    float64
	noOrg       bool
	execs       []string
	args        [][]any
}

type fakeRow struct {
	vals []any
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *string:
			*p = r.vals[i].(string)
		case *float64:
			*p = r.vals[i].(float64)
		case **int64:
			*p = r.vals[i].(*int64)
		default:
			return errors.New("unexpected dest")
		}
	}
	return nil
}

func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) error {
	f.execs = append(f.execs, sql)
	f.args = append(f.args, args)
	return nil
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, args ...any) Row {
	if f.noOrg {
		return fakeRow{err: errors.New("no rows in result set")}
	}
	var cap_ *int64
	user := 0.0
	if args[2].(*int64) != nil {
		cap_, user = f.memberCap, f.userUsed
	}
	return fakeRow{vals: []any{f.plan, f.orgOverride, f.orgUsed, cap_, user}}
}

func TestReserveAndRecord(t *testing.T) {
	cfg, err := Parse("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Now = func() time.Time { return time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC) }
	uid := int64(7)
	cap5 := int64(5)
	tests := []struct {
		name    string
		db      *fakeDB
		user    *int64
		est     float64
		allowed bool
		reason  string
		err     error
	}{
		{"free plan fits", &fakeDB{plan: "free", orgUsed: 50}, nil, 10, true, "", nil},
		{"free plan full", &fakeDB{plan: "free", orgUsed: 95}, nil, 10, false, ReasonOrgLimit, nil},
		{"automatic work ignores member cap", &fakeDB{plan: "team", memberCap: &cap5, userUsed: 5}, nil, 1, true, "", nil},
		{"member cap", &fakeDB{plan: "team", memberCap: &cap5, userUsed: 5}, &uid, 1, false, ReasonUserLimit, nil},
		{"unknown org", &fakeDB{noOrg: true}, nil, 1, false, "", ErrOrgNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := cfg.Reserve(context.Background(), tt.db, Request{OrgID: 3, UserID: tt.user, Feature: FeatureCodeintelEnrich, EstimateCredits: tt.est})
			if !errors.Is(err, tt.err) {
				t.Fatalf("err %v", err)
			}
			if err != nil {
				return
			}
			if d.Allowed != tt.allowed || d.Reason != tt.reason {
				t.Fatalf("%+v", d)
			}
			if len(tt.db.execs) != 1 || !strings.Contains(tt.db.execs[0], "pg_advisory_xact_lock") || tt.db.args[0][0] != LockClass {
				t.Fatalf("lock not taken: %v", tt.db.execs)
			}
		})
	}
	db := &fakeDB{}
	if err := Record(context.Background(), db, Usage{OrgID: 3, ProjectID: 9, UserID: &uid, Model: "m", InputTokens: 10, OutputTokens: 5, Credits: 1.23456, Ref: "repo:1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(db.execs[0], "INSERT INTO ai_usage") || db.args[0][3] != FeatureOther || db.args[0][7] != 1.2346 {
		t.Fatalf("record %v %v", db.execs, db.args)
	}
	if err := Record(context.Background(), db, Usage{}); err == nil {
		t.Fatal("org id required")
	}
}
