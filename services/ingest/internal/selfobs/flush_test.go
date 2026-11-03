package selfobs_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	trooper "github.com/makifbaysal/tasktrooper-telemetry/sdks/go"

	"github.com/tasktrooper/telemetry/services/ingest/internal/selfobs"
	"github.com/tasktrooper/telemetry/services/ingest/internal/selfobs/selfobstest"
)

func spanAttrs(s sdktrace.ReadOnlySpan) map[string]string {
	out := map[string]string{}
	for _, kv := range s.Attributes() {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

func TestStartFlushPerTopic(t *testing.T) {
	tests := []struct {
		name       string
		records    [][2]string // topic, project
		wantTopics []string    // destination of each ended flush span, parent first
		wantCounts []string
	}{
		{name: "one topic", records: [][2]string{{"tt.otlp.traces", "9"}, {"tt.otlp.traces", "9"}},
			wantTopics: []string{"tt.otlp.traces"}, wantCounts: []string{"2"}},
		{name: "two topics, largest is the parent", records: [][2]string{{"tt.otlp.logs", "9"}, {"tt.otlp.traces", "9"}, {"tt.otlp.traces", "9"}},
			wantTopics: []string{"tt.otlp.traces", "tt.otlp.logs"}, wantCounts: []string{"2", "1"}},
		{name: "self-only topic gets no span", records: [][2]string{{"tt.otlp.logs", "2"}, {"tt.otlp.traces", "9"}},
			wantTopics: []string{"tt.otlp.traces"}, wantCounts: []string{"1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := selfobstest.Install(t)
			tel := selfobs.New(nil, 2)
			var st selfobs.FlushStats
			for _, r := range tt.records {
				st.Add(r[0], tel.IsSelfHeader([]byte(r[1])))
			}
			ctx, flush := tel.StartFlush(context.Background(), "tt-ingest-writer", st)
			_, ins := selfobs.InsertSpan(ctx, "spans", 1)
			selfobs.EndSpan(ins, nil)
			flush.End(nil)
			var gotTopics, gotCounts []string
			var parentID string
			for _, s := range rec.Spans.Ended() {
				switch s.Name() {
				case "INSERT spans":
					parentID = s.Parent().SpanID().String()
				case "process flush":
					a := spanAttrs(s)
					if a["messaging.consumer.group.name"] != "tt-ingest-writer" || a["trooper.process"] != "flush" {
						t.Fatalf("flush attrs %v", a)
					}
					gotTopics = append(gotTopics, a["messaging.destination.name"])
					gotCounts = append(gotCounts, a["messaging.batch.message_count"])
					if len(gotTopics) == 1 && s.SpanContext().SpanID().String() != parentID {
						t.Fatal("INSERT must be a child of the largest topic's span")
					}
					if len(gotTopics) > 1 && len(s.Links()) != 1 {
						t.Fatal("secondary flush spans must link to the parent")
					}
				}
			}
			if !slices.Equal(gotTopics, tt.wantTopics) || !slices.Equal(gotCounts, tt.wantCounts) {
				t.Fatalf("topics %q counts %q, want %q %q", gotTopics, gotCounts, tt.wantTopics, tt.wantCounts)
			}
		})
	}
}

func TestEndSpanRecordsStack(t *testing.T) {
	rec := selfobstest.Install(t)
	_, span := rec.TP.Tracer("t").Start(context.Background(), "op")
	selfobs.EndSpan(span, errors.New("boom"))
	s := rec.Spans.Ended()[0]
	if s.Status().Code != codes.Error || len(s.Events()) != 1 {
		t.Fatalf("status %v events %v", s.Status(), s.Events())
	}
	var stack string
	for _, kv := range s.Events()[0].Attributes {
		if kv.Key == "exception.stacktrace" {
			stack = kv.Value.AsString()
		}
	}
	if !strings.Contains(stack, "TestEndSpanRecordsStack") {
		t.Fatalf("missing stack trace: %q", stack)
	}
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestHTTPClient(t *testing.T) {
	rec := selfobstest.Install(t)
	base := doerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	})
	c := selfobs.HTTPClient(base, "s3")
	// no recording parent: no span
	req, _ := http.NewRequest(http.MethodPut, "http://seaweed:8333/tt-replay/1/session/0.json?x-id=PutObject", nil)
	if _, err := c.Do(req); err != nil {
		t.Fatal(err)
	}
	if n := len(rec.Spans.Ended()); n != 0 {
		t.Fatalf("spans without parent = %d", n)
	}
	ctx, parent := rec.TP.Tracer("t").Start(context.Background(), "flush")
	if _, err := c.Do(req.WithContext(ctx)); err != nil {
		t.Fatal(err)
	}
	parent.End()
	s := rec.Spans.Ended()[0]
	a := spanAttrs(s)
	if s.Name() != "PUT" || a["peer.service"] != "s3" || a["url.full"] != "http://seaweed:8333" || a["http.response.status_code"] != "200" {
		t.Fatalf("span %s attrs %v", s.Name(), a)
	}
	if _, ok := req.Header["Traceparent"]; ok {
		t.Fatal("signed requests must not get trace headers")
	}
}

func TestLogHandlerEntryRoute(t *testing.T) {
	rec := selfobstest.Install(t)
	log := slog.New(selfobs.LogHandler(slog.NewJSONHandler(io.Discard, nil), "tt-test"))
	ctx, span := rec.TP.Tracer("t").Start(context.Background(), "POST /v1/traces")
	ctx, entry := trooper.BeginEntry(ctx, "POST", "/v1/traces", nil)
	log.WarnContext(ctx, "in request")
	log.WarnContext(context.Background(), "outside")
	entry.Finish("")
	span.End()
	if a := rec.Logs.Attrs("in request"); a["trooper.entry.route"] != "/v1/traces" || a["trooper.entry.method"] != "POST" {
		t.Fatalf("request log attrs %v", a)
	}
	if a := rec.Logs.Attrs("outside"); a == nil || a["trooper.entry.route"] != "" {
		t.Fatalf("log without entry attrs %v", a)
	}
}
