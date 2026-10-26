package rate

import (
	"strings"
	"sync"
	"time"
)

// RebalanceCounter counts consumer group rebalances in a sliding window (1 h by default).
//
// Kafka's DescribeGroups does not return the generation id, so a "generation" is identified by
// the group's member set (member ids, which the coordinator re-issues on every join) observed
// while the group is settled (Stable/Empty). One rebalance is counted when:
//   - the settled member set differs from the previous settled observation, or
//   - a transient state (PreparingRebalance, CompletingRebalance, Assigning, Reconciling) was
//     seen since the previous settled observation even if the member set came back unchanged
//     (static members re-joining with the same instance id).
//
// Transient observations alone never count more than once per rebalance.
type RebalanceCounter struct {
	Window time.Duration

	mu     sync.Mutex
	groups map[string]*groupGen
}

type groupGen struct {
	init         bool
	generation   string
	sawRebalance bool
	events       []time.Time
	lastSeen     time.Time
}

// NewRebalanceCounter returns a counter with a 1 hour window.
func NewRebalanceCounter() *RebalanceCounter {
	return &RebalanceCounter{Window: time.Hour, groups: map[string]*groupGen{}}
}

// IsTransientState reports whether a group state means a rebalance is in progress.
func IsTransientState(state string) bool {
	switch strings.ToLower(state) {
	case "preparingrebalance", "completingrebalance", "assigning", "reconciling":
		return true
	}
	return false
}

// Observe records the group's state and generation key (e.g. sorted member ids joined) at now
// and returns the number of rebalances inside the window ending at now.
func (r *RebalanceCounter) Observe(group, state, generation string, now time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.groups[group]
	if g == nil {
		g = &groupGen{}
		r.groups[group] = g
	}
	g.lastSeen = now
	if IsTransientState(state) {
		if g.init { // a rebalance on the very first observation has no baseline: ignore
			g.sawRebalance = true
		}
	} else {
		if g.init && (generation != g.generation || g.sawRebalance) {
			g.events = append(g.events, now)
		}
		g.init = true
		g.generation = generation
		g.sawRebalance = false
	}
	return r.trim(g, now)
}

func (r *RebalanceCounter) trim(g *groupGen, now time.Time) int {
	w := r.Window
	if w <= 0 {
		w = time.Hour
	}
	cut := now.Add(-w)
	i := 0
	for i < len(g.events) && !g.events[i].After(cut) {
		i++
	}
	g.events = g.events[i:]
	return len(g.events)
}

// Prune forgets groups not observed since before cutoff.
func (r *RebalanceCounter) Prune(cutoff time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, g := range r.groups {
		if g.lastSeen.Before(cutoff) {
			delete(r.groups, k)
		}
	}
}
