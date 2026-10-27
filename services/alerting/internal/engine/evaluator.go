package engine

import (
	"time"

	"github.com/tasktrooper/telemetry/services/alerting/internal/eval"
	"github.com/tasktrooper/telemetry/services/alerting/internal/kpi"
	"github.com/tasktrooper/telemetry/services/alerting/internal/store"
	"github.com/tasktrooper/telemetry/services/pkg/llmprice"
)

// llmPriceTTL: how long an org's llm_prices overrides are cached (api edits show up within a minute).
const llmPriceTTL = time.Minute

// newEvaluator wires the ClickHouse querier and, when a store is configured, the serverless_functions lookup used by
// faas monitors and the llm_prices overrides used by llm cost metrics.
func newEvaluator(ch kpi.CH, st *store.Store) *eval.Evaluator {
	ev := &eval.Evaluator{CH: ch}
	if st != nil {
		ev.Functions = st
		ev.Prices = llmprice.NewCache(st.LLMPrices, llmPriceTTL)
	}
	return ev
}
