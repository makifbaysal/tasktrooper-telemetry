package lag

import (
	"encoding/json"
	"math"
	"testing"
)

// golden is the output of the verbatim TS formulas from apps/web/src/lib/lagAlerts.ts run in node on the
// inputs built in buildInputs (generator: testdata/golden.mjs).
const golden = `[{"metric":"lag","series":[100,120,90,400,800,1200,1500,1900,2500,3000,3400,3900,4500,5100,5600,6000],"episodes":1,"firingNow":true,"now":6000},{"metric":"time_behind","series":[100,10.909090909090908,7.5,400,57.142857142857146,80,1500,111.76470588235294,138.88888888888889,3000,170,185.71428571428572,4500,221.7391304347826,233.33333333333334,6000],"episodes":1,"firingNow":true,"now":6000},{"metric":"partition_lag","series":[40,48,36,160,320,480,600,760,1000,1200,1360,1560,1800,2040,2240,2400],"episodes":1,"firingNow":true,"now":2400},{"metric":"growing","series":[0,0,0,0,0,0,0,0,0,374.5,415.5,445,470,490,510,520],"episodes":1,"firingNow":true,"now":520},{"metric":"growing","series":[0,0,0,0,0,0,0,0,0,1040,1835,2435,2835,2995,3000,3000,3000,3000,3000,3000],"episodes":1,"firingNow":true,"now":3000},{"metric":"not_drained","series":[0.016666666666666666,0.03333333333333333,0.05,0,0,0,0.016666666666666666,0.03333333333333333,0.05,0.06666666666666667,0.08333333333333333,0.09999999999999999,0.11666666666666665,0.13333333333333333,0.15,0.16666666666666666,0.18333333333333332,0.19999999999999998,0.21666666666666665,0.2333333333333333],"episodes":1,"firingNow":true,"now":0.2333333333333333},{"metric":"rebalances","series":[0,0,1,2,0,0,1,2,0,0,1,2,0,0,1,2],"episodes":0,"firingNow":false,"now":2}]`

type goldenCase struct {
	Metric    string    `json:"metric"`
	Series    []float64 `json:"series"`
	Episodes  int       `json:"episodes"`
	FiringNow bool      `json:"firingNow"`
	Now       float64   `json:"now"`
}

var (
	lagA = []float64{100, 120, 90, 400, 800, 1200, 1500, 1900, 2500, 3000, 3400, 3900, 4500, 5100, 5600, 6000}
	lagB = []float64{5000, 4000, 3000, 2000, 200, 100, 3000, 6000, 9000, 12000, 15000, 18000, 21000, 24000, 27000, 30000, 33000, 36000, 39000, 42000}
)

// buildInputs mirrors the generator: rate 0 every third minute, partition max = lag × part.
func buildInputs(l []float64, part float64) Inputs {
	in := Inputs{Lag: l, StepMin: 1}
	for i, v := range l {
		r := float64(10 + i)
		if i%3 == 0 {
			r = 0
		}
		in.ProduceRate = append(in.ProduceRate, r)
		in.MaxPartition = append(in.MaxPartition, v*part)
		in.Rebalances = append(in.Rebalances, float64(i%4-1))
	}
	return in
}

func TestMatchesTypeScript(t *testing.T) {
	var want []goldenCase
	if err := json.Unmarshal([]byte(golden), &want); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		metric     Metric
		lag        []float64
		part       float64
		th, forMin float64
	}{
		{Lag, lagA, 0.4, 1000, 5}, {TimeBehind, lagA, 0.4, 200, 3}, {PartitionLag, lagA, 0.4, 1500, 2},
		{Growing, lagA, 0.4, 0, 3}, {Growing, lagB, 0.5, 0, 5}, {NotDrained, lagB, 0.5, 0.2, 0}, {Rebalances, lagA, 0.4, 1, 2},
	}
	for i, tc := range cases {
		t.Run(string(tc.metric), func(t *testing.T) {
			s, err := Series(tc.metric, buildInputs(tc.lag, tc.part))
			if err != nil {
				t.Fatal(err)
			}
			w := want[i]
			if len(s) != len(w.Series) {
				t.Fatalf("len %d want %d", len(s), len(w.Series))
			}
			for j := range s {
				if math.Abs(s[j]-w.Series[j]) > 1e-9 {
					t.Fatalf("point %d: %v want %v", j, s[j], w.Series[j])
				}
			}
			r := Evaluate(s, Threshold(tc.metric, tc.th), tc.forMin, 1)
			if r.Episodes != w.Episodes || r.FiringNow != w.FiringNow || math.Abs(r.Now-w.Now) > 1e-9 {
				t.Fatalf("evaluate = %+v, want episodes=%d firing=%v now=%v", r, w.Episodes, w.FiringNow, w.Now)
			}
		})
	}
}

func TestEvaluateRuns(t *testing.T) {
	tests := []struct {
		name     string
		data     []float64
		th, for_ float64
		episodes int
		firing   bool
		run      int
	}{
		{"empty", nil, 1, 1, 0, false, 0},
		{"forMin 0 needs one point", []float64{0, 2}, 1, 0, 1, true, 1},
		{"two episodes", []float64{2, 2, 0, 2, 2, 2}, 1, 2, 2, true, 3},
		{"equal is not above", []float64{1, 1, 1}, 1, 1, 0, false, 0},
		{"recovered", []float64{5, 5, 5, 0}, 1, 3, 1, false, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Evaluate(tc.data, tc.th, tc.for_, 1)
			if r.Episodes != tc.episodes || r.FiringNow != tc.firing || r.Run != tc.run {
				t.Fatalf("got %+v", r)
			}
		})
	}
}

func TestUnknownMetric(t *testing.T) {
	if _, err := Series("bogus", Inputs{}); err == nil {
		t.Fatal("want error")
	}
	if Metric("bogus").Valid() || !TimeBehind.Valid() {
		t.Fatal("Valid")
	}
}
