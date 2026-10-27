package timerange

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    Range
		wantErr bool
	}{
		{"", R1h, false}, {"15m", R15m, false}, {"30m", R30m, false}, {"1h", R1h, false}, {"6h", R6h, false}, {"24h", R24h, false}, {"7d", R7d, false},
		{"30d", "", true}, {"1H", "", true},
	} {
		got, err := Parse(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("Parse(%q) = %q, %v", tc.in, got, err)
		}
	}
}

func TestWindowBuckets(t *testing.T) {
	now := time.Date(2026, 10, 25, 13, 47, 31, 0, time.UTC)
	for _, tc := range []struct {
		r      Range
		points int
		step   time.Duration
		last   time.Time
		hourly bool
	}{
		{R15m, 15, time.Minute, time.Date(2026, 10, 25, 13, 47, 0, 0, time.UTC), false},
		{R30m, 30, time.Minute, time.Date(2026, 10, 25, 13, 47, 0, 0, time.UTC), false},
		{R1h, 60, time.Minute, time.Date(2026, 10, 25, 13, 47, 0, 0, time.UTC), false},
		{R6h, 72, 5 * time.Minute, time.Date(2026, 10, 25, 13, 45, 0, 0, time.UTC), false},
		{R24h, 96, 15 * time.Minute, time.Date(2026, 10, 25, 13, 45, 0, 0, time.UTC), false},
		{R7d, 84, 2 * time.Hour, time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC), true},
	} {
		w := tc.r.Window(now)
		ts := w.Timestamps()
		if len(ts) != tc.points || w.Step != tc.step || w.Hourly() != tc.hourly {
			t.Fatalf("%s: points=%d step=%v hourly=%v", tc.r, len(ts), w.Step, w.Hourly())
		}
		if got := time.UnixMilli(ts[len(ts)-1]).UTC(); !got.Equal(tc.last) {
			t.Errorf("%s: last bucket %v, want %v", tc.r, got, tc.last)
		}
		if !w.To.After(now) || w.Index(now) != tc.points-1 {
			t.Errorf("%s: now must fall in the last bucket (idx %d)", tc.r, w.Index(now))
		}
		if ts[1]-ts[0] != tc.step.Milliseconds() {
			t.Errorf("%s: step %d", tc.r, ts[1]-ts[0])
		}
		if w.Index(w.From.Add(-time.Second)) != -1 || w.Index(w.To) != -1 || w.Index(w.From) != 0 {
			t.Errorf("%s: index bounds", tc.r)
		}
		if p := w.Prev(); !p.To.Equal(w.From) || p.Duration() != w.Duration() {
			t.Errorf("%s: prev window %v..%v", tc.r, p.From, p.To)
		}
	}
}

func TestBuilderZeroFill(t *testing.T) {
	w := R1h.Window(time.Date(2026, 1, 1, 10, 0, 30, 0, time.UTC))
	b := NewBuilder(w, "rpm", "errors")
	b.Set("rpm", 0, 5)
	b.Set("rpm", 59, 7)
	b.Set("rpm", 60, 99) // out of range: ignored
	b.Set("rpm", -1, 99) // ignored
	b.Add("late", 3, 1)  // created on demand
	b.Add("late", 3, 2)
	b.Label("late", "Late arrivals")
	s := b.Build()
	if len(s.Timestamps) != 60 || len(s.Series) != 3 {
		t.Fatalf("got %d ts, %d series", len(s.Timestamps), len(s.Series))
	}
	ids := []string{s.Series[0].ID, s.Series[1].ID, s.Series[2].ID}
	if ids[0] != "rpm" || ids[1] != "errors" || ids[2] != "late" {
		t.Errorf("order %v", ids)
	}
	for i, d := range s.Series {
		if len(d.Data) != 60 {
			t.Errorf("series %d len %d", i, len(d.Data))
		}
	}
	if s.Series[0].Data[0] != 5 || s.Series[0].Data[59] != 7 || s.Series[0].Data[30] != 0 {
		t.Errorf("rpm %v", s.Series[0].Data)
	}
	if s.Series[2].Data[3] != 3 || s.Series[2].Label != "Late arrivals" {
		t.Errorf("late %+v", s.Series[2])
	}
}

func TestRatio(t *testing.T) {
	got := Ratio([]float64{1, 2, 3}, []float64{2, 0, 3})
	if got[0] != 0.5 || got[1] != 0 || got[2] != 1 {
		t.Errorf("%v", got)
	}
}

func TestBucketExpr(t *testing.T) {
	w := Window{From: time.Unix(1000, 0), Step: time.Minute}
	if got := w.BucketExpr("minute"); got != "intDiv(toInt64(toUnixTimestamp(minute)) - 1000, 60)" {
		t.Error(got)
	}
}
