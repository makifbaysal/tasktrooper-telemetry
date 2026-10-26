package writer

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"path/filepath"
	"testing"

	"github.com/tasktrooper/telemetry/services/ingest/internal/flatten"
	"github.com/tasktrooper/telemetry/services/ingest/internal/geo"
	"github.com/tasktrooper/telemetry/services/ingest/internal/geo/geotest"
	"github.com/tasktrooper/telemetry/services/pkg/rumpb"
	"github.com/tasktrooper/telemetry/services/pkg/topics"
)

func tid(n uint64) []byte {
	b := make([]byte, 16)
	binary.BigEndian.PutUint64(b[8:], n)
	binary.BigEndian.PutUint64(b[:8], n*0x9E3779B97F4A7C15)
	return b
}

// trace builds an entry span + one child; errEntry marks the entry as failed.
func trace(n uint64, durMs uint64, errEntry bool, weight float32) []flatten.SpanRow {
	e := flatten.SpanRow{TraceID: tid(n), IsEntry: 1, Kind: "server", DurationNs: durMs * 1e6, SampleWeight: weight}
	if errEntry {
		e.IsError = 1
	}
	c := flatten.SpanRow{TraceID: tid(n), Kind: "client", DurationNs: 1e6, SampleWeight: weight}
	return []flatten.SpanRow{e, c}
}

// red sums what red_1m would: count, errors (weighted, entry spans only).
func red(rows []flatten.SpanRow) (count, errs float64) {
	for _, r := range rows {
		if r.IsEntry == 1 {
			count += float64(r.SampleWeight)
			if r.IsError == 1 {
				errs += float64(r.SampleWeight)
			}
		}
	}
	return
}

func TestTailSamplingKeepsREDInExpectation(t *testing.T) {
	s := TailSampler{Rate: 0.1, SlowNs: 500e6}
	rng := rand.New(rand.NewPCG(1, 2))
	var in []flatten.SpanRow
	for n := range uint64(50_000) {
		w := float32(1)
		if n%4 == 0 {
			w = 2 // SDK-side head sampling at 50 % on a quarter of the traffic
		}
		dur := uint64(20 + rng.IntN(100))
		if n%97 == 0 {
			dur = 900 // slow
		}
		in = append(in, trace(n, dur, n%53 == 0, w)...)
	}
	wantCount, wantErr := red(in)
	out := s.Apply(append([]flatten.SpanRow(nil), in...))
	gotCount, gotErr := red(out)

	if gotErr != wantErr {
		t.Errorf("errors must be exact (all error traces kept): got %v want %v", gotErr, wantErr)
	}
	if rel := math.Abs(gotCount-wantCount) / wantCount; rel > 0.03 {
		t.Errorf("weighted count %v vs %v (%.2f%% off)", gotCount, wantCount, rel*100)
	}
	if len(out) > len(in)/5 {
		t.Errorf("kept %d of %d spans; sampling at 10%% should drop most", len(out), len(in))
	}
	// Whole traces only: entry and child share the decision.
	perTrace := map[[16]byte]int{}
	for _, r := range out {
		perTrace[[16]byte(r.TraceID)]++
		switch {
		case r.IsError == 1 || (r.IsEntry == 1 && r.DurationNs >= s.SlowNs):
			if r.SampleWeight != 1 && r.SampleWeight != 2 {
				t.Fatalf("forced-keep span re-weighted: %v", r.SampleWeight)
			}
		}
	}
	for id, n := range perTrace {
		if n != 2 {
			t.Fatalf("trace %x split: %d spans kept", id, n)
		}
	}
}

func TestTailSamplingDeterministicAcrossBatches(t *testing.T) {
	s := TailSampler{Rate: 0.3}
	var a, b []flatten.SpanRow
	for n := range uint64(2000) {
		tr := trace(n, 10, false, 1)
		a = append(a, tr[0]) // entry spans arrive in one batch …
		b = append(b, tr[1]) // … children in another
	}
	ka, kb := s.Apply(a), s.Apply(b)
	if len(ka) != len(kb) {
		t.Fatalf("batches disagree: %d vs %d", len(ka), len(kb))
	}
	for i := range ka {
		if string(ka[i].TraceID) != string(kb[i].TraceID) {
			t.Fatalf("decision differs for trace %x", ka[i].TraceID)
		}
		if math.Abs(float64(ka[i].SampleWeight)-1/0.3) > 1e-4 {
			t.Fatalf("weight %v", ka[i].SampleWeight)
		}
	}
}

func TestTailSamplingDisabledAndIdempotentFlush(t *testing.T) {
	in := trace(1, 10, false, 1)
	if out := (TailSampler{Rate: 1}).Apply(in); len(out) != 2 || out[0].SampleWeight != 1 {
		t.Errorf("rate 1 must keep everything unchanged: %+v", out)
	}
	// A flush that fails keeps its rows; the retry must not sample (and re-weight) them again.
	b := NewBatches()
	s := TailSampler{Rate: 0.5}
	for n := range uint64(200) {
		b.Spans = append(b.Spans, trace(n, 10, false, 1)...)
	}
	b.Sample(s)
	first := len(b.Spans)
	w := b.Spans[0].SampleWeight
	b.Sample(s)
	if len(b.Spans) != first || b.Spans[0].SampleWeight != w {
		t.Errorf("second Sample changed the batch: %d→%d, weight %v→%v", first, len(b.Spans), w, b.Spans[0].SampleWeight)
	}
	b.Spans = append(b.Spans, trace(1000, 10, true, 1)...) // new rows after the failed flush are sampled
	b.Sample(s)
	if len(b.Spans) != first+2 {
		t.Errorf("new error trace not kept: %d", len(b.Spans))
	}
	b.Reset()
	if b.sampled != 0 {
		t.Error("Reset must clear the sampled mark")
	}
}

func TestRumCountryFromGeoIP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "geo.mmdb")
	if err := geotest.Write(path, map[string]string{"81.214.0.0/16": "TR"}); err != nil {
		t.Fatal(err)
	}
	db, err := geo.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b := NewBatches()
	b.Geo = db
	batch := &rumpb.RumBatch{Resource: &rumpb.Resource{App: "web"}, Events: []*rumpb.RumEvent{
		{Type: rumpb.EventType_VIEW_START, TsMs: 1_700_000_000_000},
		{Type: rumpb.EventType_VITAL, TsMs: 1_700_000_000_000, Attributes: map[string]string{"geo.country": "de"}},
	}}
	if err := b.Add(rec(topics.RUM, batch, append(append([]string{}, hdrs...), topics.HeaderClientIP, "81.214.3.4")...)); err != nil {
		t.Fatal(err)
	}
	if b.Rum[0].Country != "TR" || b.Rum[1].Country != "DE" {
		t.Errorf("countries: %q %q", b.Rum[0].Country, b.Rum[1].Country)
	}
	for _, r := range b.Rum {
		for k, v := range r.Attrs {
			if v == "81.214.3.4" {
				t.Errorf("client ip stored in attrs[%s]", k)
			}
		}
	}
	// No database → empty country.
	b2 := NewBatches()
	_ = b2.Add(rec(topics.RUM, batch, append(append([]string{}, hdrs...), topics.HeaderClientIP, "81.214.3.4")...))
	if b2.Rum[0].Country != "" {
		t.Errorf("country without db: %q", b2.Rum[0].Country)
	}
}

func BenchmarkTailSampler(b *testing.B) {
	var in []flatten.SpanRow
	for n := range uint64(10_000) {
		in = append(in, trace(n, 10, n%50 == 0, 1)...)
	}
	s := TailSampler{Rate: 0.1, SlowNs: 1e9}
	buf := make([]flatten.SpanRow, len(in))
	b.ReportAllocs()
	for b.Loop() {
		copy(buf, in)
		s.Apply(buf)
	}
}
