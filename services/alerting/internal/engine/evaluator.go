package engine

import (
	"github.com/tasktrooper/telemetry/services/alerting/internal/eval"
	"github.com/tasktrooper/telemetry/services/alerting/internal/kpi"
	"github.com/tasktrooper/telemetry/services/alerting/internal/store"
)

// newEvaluator wires the ClickHouse querier and, when a store is configured, the serverless_functions lookup used by
// faas monitors.
func newEvaluator(ch kpi.CH, st *store.Store) *eval.Evaluator {
	ev := &eval.Evaluator{CH: ch}
	if st != nil {
		ev.Functions = st
	}
	return ev
}
