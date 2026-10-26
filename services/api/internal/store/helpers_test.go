package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestWindowSum(t *testing.T) {
	for _, tc := range []struct {
		name      string
		in        []float64
		step, win float64
		want      []float64
	}{
		{"rolling 1m buckets, 3m window", []float64{1, 2, 3, 4}, 1, 3, []float64{1, 3, 6, 9}},
		{"bucket equals window", []float64{5, 7}, 5, 5, []float64{5, 7}},
		{"bucket longer than window", []float64{60}, 15, 5, []float64{20}},
	} {
		got := WindowSum(tc.in, tc.step, tc.win)
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Errorf("%s: %v want %v", tc.name, got, tc.want)
				break
			}
		}
	}
}

func TestRetention(t *testing.T) {
	for in, want := range map[float64]string{604800000: "7d", 43200000: "12h", 1800000: "30m", 129600000: "1.5d", -1: "∞", 0: "", 45000: "45s"} {
		if got := Retention(in); got != want {
			t.Errorf("Retention(%v) = %q want %q", in, got, want)
		}
	}
}

func TestBrokerKindAndSystem(t *testing.T) {
	for sys, kind := range map[string]string{"aws_sqs": "sqs", "gcp_pubsub": "pubsub", "rabbitmq": "rabbitmq", "kafka": "kafka", "": ""} {
		if got := BrokerKind(sys); got != kind {
			t.Errorf("BrokerKind(%q) = %q", sys, got)
		}
	}
	if MessagingSystem("sqs") != "aws_sqs" || MessagingSystem("kafka") != "kafka" {
		t.Error("MessagingSystem")
	}
}

func TestDegradedRun(t *testing.T) {
	type b struct{ ts, good, total float64 }
	run := func(bs []b) int64 {
		return DegradedRun(func(yield func(int64, float64, float64) bool) {
			for _, x := range bs {
				if !yield(int64(x.ts), x.good, x.total) {
					return
				}
			}
		}, 0.99)
	}
	// newest first: 3 bad buckets, an empty one inside the run, then a good one
	if got := run([]b{{400, 90, 100}, {300, 0, 0}, {200, 95, 100}, {100, 100, 100}}); got != 200_000 {
		t.Errorf("run = %d", got)
	}
	if got := run([]b{{400, 100, 100}, {300, 1, 100}}); got != 0 {
		t.Errorf("healthy now = %d", got)
	}
}

func TestParseWindowAndPercentile(t *testing.T) {
	for in, want := range map[any]time.Duration{"15m": 15 * time.Minute, 10.0: 10 * time.Minute, nil: 5 * time.Minute, "1h": time.Hour, "x": 5 * time.Minute} {
		if got := ParseWindow(in); got != want {
			t.Errorf("ParseWindow(%v) = %v", in, got)
		}
	}
	for in, want := range map[string]float64{"p95": 0.95, "75": 0.75, "0.9": 0.9, "": 0.75} {
		if got := ParsePercentile(in, 0.75); got != want {
			t.Errorf("ParsePercentile(%q) = %v", in, got)
		}
	}
}

func TestRedactConfigAndPatternID(t *testing.T) {
	out := redactConfig(map[string]any{"roleArn": "arn", "secret": "x", "apiKey": "k", "drainSecretRef": "secret:3"})
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if m["secret"] != nil || m["apiKey"] != nil || m["roleArn"] != "arn" || m["drainSecretRef"] != "secret:3" {
		t.Errorf("redacted %v", m)
	}
	var q PreviewQuery
	if err := json.Unmarshal([]byte(`{"patternId":"18446744073709551615"}`), &q); err != nil || q.PatternID != 18446744073709551615 {
		t.Errorf("patternId %v %v", q.PatternID, err)
	}
}

func TestSecretID(t *testing.T) {
	if id, ok := secretID("secret:12"); !ok || id != 12 {
		t.Error("secret:12")
	}
	for _, r := range []string{"env:X", "secret:", "secret:abc", ""} {
		if _, ok := secretID(r); ok {
			t.Errorf("%q parsed", r)
		}
	}
	if !IsRef("env:SLACK") || IsRef("https://hooks.slack.com/x") {
		t.Error("IsRef")
	}
}

func TestSplitVercelSecret(t *testing.T) {
	sec, cfg, err := splitVercelSecret("vercel", `{"token":"tok","drainSecret":"dr"}`, json.RawMessage(`{"teamId":"t"}`))
	if err != nil || sec != "tok" || !strings.Contains(string(cfg), `"drainSecret":"dr"`) || !strings.Contains(string(cfg), `"teamId":"t"`) {
		t.Fatalf("split: %q %s %v", sec, cfg, err)
	}
	if sec, cfg, _ = splitVercelSecret("vercel", "plain", nil); sec != "plain" || cfg != nil {
		t.Errorf("plain token changed")
	}
	if sec, _, _ = splitVercelSecret("vercel", `{"token":"tok"}`, nil); sec != "tok" {
		t.Errorf("token only: %q", sec)
	}
	if _, _, err = splitVercelSecret("vercel", `{"drainSecret":"x"}`, nil); err == nil {
		t.Errorf("missing token accepted")
	}
	if sec, _, _ = splitVercelSecret("gcp", `{"type":"service_account"}`, nil); sec != `{"type":"service_account"}` {
		t.Errorf("gcp json secret altered")
	}
	m := dbMetaPublic(map[string]any{"missing": true, "cuNow": 0.25, "password": "x"})
	if len(m) != 1 || m["cuNow"] != 0.25 {
		t.Errorf("db meta: %v", m)
	}
}
