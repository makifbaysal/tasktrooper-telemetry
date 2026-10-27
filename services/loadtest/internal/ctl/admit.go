package ctl

import (
	"fmt"
	"math"
	"net"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/loadtest/internal/spec"
	"github.com/tasktrooper/telemetry/services/loadtest/internal/store"
)

// Limits are the plan limits (TT_LOADTEST_MAX_VUS, TT_LOADTEST_MAX_DURATION, TT_LOADTEST_MONTHLY_VU_MINUTES).
type Limits struct {
	MaxVUs         int
	MaxDuration    time.Duration
	MonthlyMinutes float64 // 0 = unlimited
	AllowLocal     bool
}

// AdmitInput is what an admission decision needs besides the plan.
type AdmitInput struct {
	Domains []store.Domain // the project's verified domains
	UsedMin float64        // VU-minutes used this month
	Regions []string       // default regions when the spec has none
}

// Admit validates a run and splits it into region shards. Rejections are user-facing messages.
func Admit(raw []byte, lim Limits, in AdmitInput) (store.Admission, *spec.Plan) {
	plan, err := spec.Parse(raw)
	if err != nil {
		return store.Admission{Reject: "invalid scenario: " + strings.TrimPrefix(err.Error(), "spec: ")}, nil
	}
	// script scenarios declare their target too; k6 itself cannot be restricted to it (documented gap)
	if plan.Target == nil {
		return store.Admission{Reject: "scenario has no target"}, nil
	}
	if !HostAllowed(plan.Host(), in.Domains, lim.AllowLocal) {
		return store.Admission{Reject: fmt.Sprintf("target host %s is not a verified domain of this project (add it under Load tests → Domains and verify it)", plan.Host())}, nil
	}
	dur := spec.Duration(plan.Stages)
	if lim.MaxDuration > 0 && dur > lim.MaxDuration {
		return store.Admission{Reject: fmt.Sprintf("duration %s exceeds the plan limit of %s", dur, lim.MaxDuration)}, nil
	}
	if plan.Script == "" && lim.MaxVUs > 0 && plan.PeakVUs() > lim.MaxVUs {
		return store.Admission{Reject: fmt.Sprintf("peak of %d VUs exceeds the plan limit of %d", plan.PeakVUs(), lim.MaxVUs)}, nil
	}
	minutes := 0.0
	if plan.Script == "" {
		minutes = math.Round(plan.VUMinutes()*1000) / 1000
	}
	if lim.MonthlyMinutes > 0 && in.UsedMin+minutes > lim.MonthlyMinutes {
		return store.Admission{Reject: fmt.Sprintf("monthly VU-minutes exceeded: %.0f used + %.0f planned > %.0f", in.UsedMin, minutes, lim.MonthlyMinutes)}, nil
	}
	regions := plan.Regions
	if len(regions) == 0 {
		regions = in.Regions
	}
	if len(regions) == 0 || plan.Script != "" {
		// scripts carry their own load shape: one runner
		r := "local"
		if len(regions) > 0 {
			r = regions[0]
		}
		regions = []string{r}
	}
	shards := make([]store.ShardInit, len(regions))
	for i, r := range regions {
		shards[i] = store.ShardInit{Region: r, Share: 1 / float64(len(regions))}
	}
	return store.Admission{Shards: shards, VUMinutes: minutes}, plan
}

// IsLocal reports whether host is a loopback/dev host exempt with TT_LOADTEST_ALLOW_LOCAL.
func IsLocal(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// HostAllowed reports whether a target host may be load tested: a verified domain of the project (exact, or
// a subdomain of a DNS-verified domain), or a local host when allowLocal.
func HostAllowed(host string, domains []store.Domain, allowLocal bool) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	if allowLocal && IsLocal(host) {
		return true
	}
	for _, d := range domains {
		dh := strings.ToLower(strings.TrimSuffix(d.Host, "."))
		if !d.Verified || dh == "" {
			continue
		}
		if host == dh || (d.Method == "dns" && strings.HasSuffix(host, "."+dh)) {
			return true
		}
	}
	return false
}
