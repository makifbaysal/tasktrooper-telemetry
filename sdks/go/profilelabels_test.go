package trooper_test

import (
	"bytes"
	"context"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
	"github.com/tasktrooper/telemetry-go/troopertest"
)

func goroutineDump() string {
	var b bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&b, 1)
	return b.String()
}

func TestProfileLabelsOnEntries(t *testing.T) {
	troopertest.New(t, "svc")
	trooper.EnableProfileLabels(true)
	t.Cleanup(func() { trooper.EnableProfileLabels(false) })

	t.Run("http entry", func(t *testing.T) {
		ctx, span := otel.Tracer("t").Start(context.Background(), "GET", trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		ctx, entry := trooper.BeginEntry(ctx, "GET", "/users/{id}", nil)
		if v, _ := pprof.Label(ctx, trooper.ProfileLabelRoute); v != "/users/:id" {
			t.Errorf("ctx route label = %q", v)
		}
		if v, _ := pprof.Label(ctx, trooper.ProfileLabelMethod); v != "GET" {
			t.Errorf("ctx method label = %q", v)
		}
		// Goroutines started from the request goroutine inherit the labels.
		release, started, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
		go func() { defer close(exited); close(started); <-release }()
		<-started
		if d := goroutineDump(); strings.Count(d, `"trooper.route":"/users/:id"`) < 2 {
			t.Errorf("request goroutine and child not labelled:\n%s", d)
		}
		close(release)
		<-exited
		entry.Finish("")
		for i := 0; strings.Contains(goroutineDump(), `"trooper.route":"/users/:id"`); i++ {
			if i == 50 {
				t.Fatalf("labels not restored after Finish:\n%s", goroutineDump())
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	t.Run("consumer", func(t *testing.T) {
		var seen string
		err := trooper.ProcessMessage(context.Background(), nil, trooper.Message{System: "kafka", Destination: "orders", ConsumerGroup: "billing"},
			trooper.PanicRecover, func(ctx context.Context) error {
				route, _ := pprof.Label(ctx, trooper.ProfileLabelRoute)
				method, _ := pprof.Label(ctx, trooper.ProfileLabelMethod)
				seen = route + " " + method
				if !strings.Contains(goroutineDump(), `"trooper.route":"billing"`) {
					t.Error("consumer goroutine not labelled")
				}
				return nil
			})
		if err != nil || seen != "billing CONSUME" {
			t.Errorf("labels = %q, err %v", seen, err)
		}
		if strings.Contains(goroutineDump(), `"trooper.route":"billing"`) {
			t.Error("consumer labels not restored")
		}
	})

	t.Run("disabled costs nothing", func(t *testing.T) {
		trooper.EnableProfileLabels(false)
		defer trooper.EnableProfileLabels(true)
		ctx, entry := trooper.BeginEntry(context.Background(), "GET", "/x", nil)
		defer entry.Finish("")
		if _, ok := pprof.Label(ctx, trooper.ProfileLabelRoute); ok {
			t.Error("labels set while disabled")
		}
	})
}
