package statemachine

import (
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
)

func TestStepSequences(t *testing.T) {
	t0 := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	type step struct {
		at    time.Duration
		obs   Obs
		state model.State
		tr    Transition
	}
	tests := []struct {
		name  string
		cfg   Config
		steps []step
	}{
		{"fires after forMin", ConfigFor(5 * time.Minute), []step{
			{0, Obs{Breached: true}, model.StatePending, Pending},
			{2 * time.Minute, Obs{Breached: true}, model.StatePending, None},
			{5 * time.Minute, Obs{Breached: true}, model.StateFiring, Fire},
			{6 * time.Minute, Obs{Breached: true}, model.StateFiring, None},
		}},
		{"zero forMin fires immediately", ConfigFor(0), []step{
			{0, Obs{Breached: true}, model.StateFiring, Fire},
		}},
		{"pending clears without incident", ConfigFor(5 * time.Minute), []step{
			{0, Obs{Breached: true}, model.StatePending, Pending},
			{time.Minute, Obs{}, model.StateOK, Cleared},
		}},
		{"held in series fires at once", ConfigFor(5 * time.Minute), []step{
			{0, Obs{Breached: true, HeldFor: 6 * time.Minute}, model.StateFiring, Fire},
		}},
		{"partial history lands in pending, fires early", ConfigFor(5 * time.Minute), []step{
			{0, Obs{Breached: true, HeldFor: 3 * time.Minute}, model.StatePending, Pending},
			{2 * time.Minute, Obs{Breached: true}, model.StateFiring, Fire},
		}},
		{"recovery hysteresis = forMin/2", ConfigFor(10 * time.Minute), []step{
			{0, Obs{Breached: true, HeldFor: 10 * time.Minute}, model.StateFiring, Fire},
			{time.Minute, Obs{}, model.StateFiring, None},
			{3 * time.Minute, Obs{}, model.StateFiring, None},
			{4 * time.Minute, Obs{Breached: true}, model.StateFiring, None}, // flap resets the clock
			{5 * time.Minute, Obs{}, model.StateFiring, None},
			{9 * time.Minute, Obs{}, model.StateFiring, None},
			{10 * time.Minute, Obs{}, model.StateOK, Resolve},
		}},
		{"min recovery 1m", ConfigFor(0), []step{
			{0, Obs{Breached: true}, model.StateFiring, Fire},
			{30 * time.Second, Obs{}, model.StateFiring, None},
			{60 * time.Second, Obs{}, model.StateFiring, None},
			{90 * time.Second, Obs{}, model.StateOK, Resolve},
		}},
		{"no data from ok, then recovers", ConfigFor(time.Minute), []step{
			{0, Obs{NoData: true}, model.StateNoData, NoData},
			{time.Minute, Obs{NoData: true}, model.StateNoData, None},
			{2 * time.Minute, Obs{}, model.StateOK, Cleared},
		}},
		{"firing ignores no data", ConfigFor(0), []step{
			{0, Obs{Breached: true}, model.StateFiring, Fire},
			{time.Minute, Obs{NoData: true}, model.StateFiring, None},
		}},
		{"no data then breach", ConfigFor(0), []step{
			{0, Obs{NoData: true}, model.StateNoData, NoData},
			{time.Minute, Obs{Breached: true}, model.StateFiring, Fire},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := Snapshot{State: model.StateOK, Since: t0.Add(-time.Hour)}
			for i, st := range tc.steps {
				var tr Transition
				s, tr = Step(s, st.obs, tc.cfg, t0.Add(st.at))
				if s.State != st.state || tr != st.tr {
					t.Fatalf("step %d: got %s/%s want %s/%s", i, s.State, tr, st.state, st.tr)
				}
			}
		})
	}
}
