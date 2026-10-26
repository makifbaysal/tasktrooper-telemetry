package trooper

import (
	"strconv"
	"strings"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// TraceStateKey is the W3C tracestate list member carrying the head sampling rate of the trace's
// root ("tt=r:<rate>", .ai/contracts.md §4).
const TraceStateKey = "tt"

// NewSampler returns ParentBased(TraceIDRatioBased(rate)) that records the rate in the W3C
// tracestate of root spans as tt=r:<rate>. Downstream entry spans read it so trooper.sample_rate
// reflects the rate that actually decided the trace.
func NewSampler(rate float64) sdktrace.Sampler {
	if rate <= 0 || rate > 1 {
		rate = 1
	}
	return rateSampler{inner: sdktrace.ParentBased(sdktrace.TraceIDRatioBased(rate)), rate: rate}
}

type rateSampler struct {
	inner sdktrace.Sampler
	rate  float64
}

func (s rateSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	res := s.inner.ShouldSample(p)
	if !trace.SpanContextFromContext(p.ParentContext).IsValid() {
		if ts, err := res.Tracestate.Insert(TraceStateKey, "r:"+formatRate(s.rate)); err == nil {
			res.Tracestate = ts
		}
	}
	return res
}

func (s rateSampler) Description() string {
	return "TrooperSampler{" + s.inner.Description() + "}"
}

func formatRate(r float64) string { return strconv.FormatFloat(r, 'g', -1, 64) }

// rateFromTraceState parses tt=r:<rate> (sub-fields separated by ';').
func rateFromTraceState(ts trace.TraceState) (float64, bool) {
	v := ts.Get(TraceStateKey)
	for _, f := range strings.Split(v, ";") {
		if strings.HasPrefix(f, "r:") {
			if r, err := strconv.ParseFloat(f[2:], 64); err == nil && r > 0 && r <= 1 {
				return r, true
			}
		}
	}
	return 0, false
}
