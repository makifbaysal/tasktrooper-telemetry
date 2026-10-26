// Package lag turns per-minute broker data into the value a consumer_lag rule compares.
// It mirrors apps/web/src/lib/lagAlerts.ts (ruleSeries / evaluate) so the editor's "would have fired"
// replay and the engine agree; the only difference is that the server has real per-minute produce rate,
// partition max and rebalance counts where the mock derives them.
package lag

import (
	"fmt"
	"math"
)

// Metric is QueueRule.metric.
type Metric string

const (
	Lag          Metric = "lag"
	TimeBehind   Metric = "time_behind"
	PartitionLag Metric = "partition_lag"
	Growing      Metric = "growing"
	NotDrained   Metric = "not_drained"
	Rebalances   Metric = "rebalances"
	// OldestAge is the age of the oldest waiting message in seconds (SQS / Pub/Sub / RabbitMQ only:
	// broker_lag_1m.oldest_age_s). Kafka has no such gauge; use time_behind there.
	OldestAge Metric = "oldest_age"
)

// Valid reports whether m is a known metric.
func (m Metric) Valid() bool {
	switch m {
	case Lag, TimeBehind, PartitionLag, Growing, NotDrained, Rebalances, OldestAge:
		return true
	}
	return false
}

// Inputs are aligned per-step series (oldest first) for one group (+ optional topic).
type Inputs struct {
	Lag          []float64 // total lag across partitions (and topics)
	ProduceRate  []float64 // msgs/s summed over partitions
	MaxPartition []float64 // max single-partition lag
	Rebalances   []float64 // per hour, as reported
	OldestAge    []float64 // seconds, max over queues/partitions
	StepMin      float64   // minutes per point (1 for *_1m rollups)
}

const (
	smoothW = 5 // growing: moving-average width
	slopeK  = 4 // growing: slope distance in points
)

// Series returns the compared value at every point.
func Series(m Metric, in Inputs) ([]float64, error) {
	step := in.StepMin
	if step <= 0 {
		step = 1
	}
	lag := in.Lag
	out := make([]float64, len(lag))
	switch m {
	case Lag:
		copy(out, lag)
	case TimeBehind:
		for i, v := range lag {
			rate := at(in.ProduceRate, i)
			if rate == 0 {
				rate = 1 // same guard as the UI
			}
			out[i] = v / rate
		}
	case PartitionLag:
		for i := range lag {
			out[i] = at(in.MaxPartition, i)
		}
	case Growing:
		// trend of the smoothed lag (msgs/min), so single noisy points don't count as growth
		smooth := make([]float64, len(lag))
		for i := range lag {
			lo := max(0, i-smoothW+1)
			s := 0.0
			for _, x := range lag[lo : i+1] {
				s += x
			}
			smooth[i] = s / float64(i+1-lo)
		}
		for i := range smooth {
			if i < slopeK+smoothW {
				out[i] = 0
				continue
			}
			out[i] = (smooth[i] - smooth[i-slopeK]) / (slopeK * step)
		}
	case NotDrained:
		// hours since lag was last below 5% of its peak
		peak := 0.0
		for _, v := range lag {
			peak = math.Max(peak, v)
		}
		if peak == 0 {
			peak = 1
		}
		since := 0.0
		for i, v := range lag {
			if v < peak*0.05 {
				since = 0
			} else {
				since += step / 60
			}
			out[i] = since
		}
	case Rebalances:
		for i := range lag {
			out[i] = math.Max(0, at(in.Rebalances, i))
		}
	case OldestAge:
		for i := range lag {
			out[i] = math.Max(0, at(in.OldestAge, i))
		}
	default:
		return nil, fmt.Errorf("lag: unknown metric %q", m)
	}
	return out, nil
}

// Threshold is the value the series is compared to; growing fires above 0 msgs/min.
func Threshold(m Metric, th float64) float64 {
	if m == Growing {
		return 0
	}
	return th
}

// Result mirrors the TS evaluate() return value plus the trailing breach run.
type Result struct {
	Episodes  int
	FiringNow bool
	Now       float64
	Run       int // trailing points above the threshold
	Need      int
}

// Evaluate finds breach episodes: values above th for at least forMin.
func Evaluate(data []float64, th, forMin, stepMin float64) Result {
	if stepMin <= 0 {
		stepMin = 1
	}
	need := max(1, int(math.Ceil(forMin/stepMin)))
	r := Result{Need: need}
	run := 0
	for i, v := range data {
		if v > th {
			run++
		} else {
			run = 0
		}
		if run == need {
			r.Episodes++
		}
		if i == len(data)-1 {
			r.FiringNow = run >= need
			r.Now = v
		}
	}
	r.Run = run
	return r
}

func at(xs []float64, i int) float64 {
	if i < len(xs) {
		return xs[i]
	}
	return 0
}
