// Package statemachine implements the monitor state machine:
//
//	ok ──breach──▶ pending ──held forMin──▶ firing ──clear for recover──▶ ok (incident resolved)
//
// no_data is entered when the evaluator has nothing to compare; a firing monitor stays firing on no_data
// (a dead collector must not auto-resolve an incident). Pure: no clocks, no I/O.
package statemachine

import (
	"time"

	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
)

// Transition is what the engine must persist besides the new snapshot.
type Transition int

const (
	None    Transition = iota
	Pending            // ok/no_data → pending
	Fire               // → firing: open incident, notify
	Resolve            // firing → ok: resolve incident, notify
	Cleared            // pending → ok (never fired)
	NoData             // → no_data
)

func (t Transition) String() string {
	return [...]string{"none", "pending", "fire", "resolve", "cleared", "no_data"}[t]
}

// Snapshot is a monitor's state. ClearSince is in-memory only (recovery hysteresis).
type Snapshot struct {
	State      model.State
	Since      time.Time
	ClearSince time.Time
}

// Obs is one evaluation result.
type Obs struct {
	Breached bool
	NoData   bool
	// HeldFor is how long the evaluator saw the breach in its own series (e.g. consumer lag replays the
	// last minutes); lets a monitor fire right after an engine restart without waiting forMin again.
	HeldFor time.Duration
}

// Config holds the hold and recovery durations.
type Config struct {
	For     time.Duration
	Recover time.Duration
}

// MinRecover is the shortest recovery hysteresis.
const MinRecover = time.Minute

// ConfigFor derives the config from a monitor condition: recovery needs half of forMin of clean data
// (alerting.md § Incident state machine), at least MinRecover.
func ConfigFor(forDur time.Duration) Config {
	r := forDur / 2
	if r < MinRecover {
		r = MinRecover
	}
	return Config{For: forDur, Recover: r}
}

// Step advances s with o at now.
func Step(s Snapshot, o Obs, c Config, now time.Time) (Snapshot, Transition) {
	if o.NoData {
		switch s.State {
		case model.StateFiring, model.StateNoData:
			return s, None
		}
		return Snapshot{State: model.StateNoData, Since: now}, NoData
	}

	if o.Breached {
		switch s.State {
		case model.StateFiring:
			s.ClearSince = time.Time{}
			return s, None
		case model.StatePending:
			if now.Sub(s.Since) >= c.For || o.HeldFor >= c.For {
				return Snapshot{State: model.StateFiring, Since: now}, Fire
			}
			return s, None
		default: // ok, no_data
			if c.For <= 0 || o.HeldFor >= c.For {
				return Snapshot{State: model.StateFiring, Since: now}, Fire
			}
			since := now
			if o.HeldFor > 0 {
				since = now.Add(-o.HeldFor)
			}
			return Snapshot{State: model.StatePending, Since: since}, Pending
		}
	}

	switch s.State {
	case model.StateFiring:
		if s.ClearSince.IsZero() {
			s.ClearSince = now
		}
		if now.Sub(s.ClearSince) >= c.Recover {
			return Snapshot{State: model.StateOK, Since: now}, Resolve
		}
		return s, None
	case model.StatePending:
		return Snapshot{State: model.StateOK, Since: now}, Cleared
	case model.StateNoData:
		return Snapshot{State: model.StateOK, Since: now}, Cleared
	}
	return s, None
}
