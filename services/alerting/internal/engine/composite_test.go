package engine

import (
	"testing"

	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
)

func TestCompositeResult(t *testing.T) {
	F, O, N := model.StateFiring, model.StateOK, model.StateNoData
	tests := []struct {
		name             string
		op               string
		ids              []int64
		states           map[int64]model.State
		breached, nodata bool
		value            float64
	}{
		{"and all firing", "and", []int64{1, 2}, map[int64]model.State{1: F, 2: F}, true, false, 2},
		{"and one ok", "and", []int64{1, 2}, map[int64]model.State{1: F, 2: O}, false, false, 1},
		{"default op is and", "", []int64{1, 2}, map[int64]model.State{1: F, 2: model.StatePending}, false, false, 1},
		{"and missing child", "and", []int64{1, 2}, map[int64]model.State{1: F}, false, true, 0},
		{"or any firing", "OR", []int64{1, 2}, map[int64]model.State{1: N, 2: F}, true, false, 1},
		{"or none firing", "or", []int64{1, 2}, map[int64]model.State{1: O, 2: N}, false, false, 0},
		{"or missing child ok", "or", []int64{1, 2}, map[int64]model.State{2: F}, true, false, 1},
		{"nothing known", "or", []int64{1}, map[int64]model.State{}, false, true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := CompositeResult(tc.op, tc.ids, tc.states)
			if r.Breached != tc.breached || r.NoData != tc.nodata || (!r.NoData && r.Value != tc.value) {
				t.Fatalf("got %+v", r)
			}
		})
	}
}
