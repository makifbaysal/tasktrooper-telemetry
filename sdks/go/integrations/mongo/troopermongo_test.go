package troopermongo

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
	"github.com/tasktrooper/telemetry-go/troopertest"
)

func raw(t *testing.T, d bson.D) bson.Raw {
	t.Helper()
	b, err := bson.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPeer(t *testing.T) {
	for in, want := range map[string]struct {
		host string
		port int
	}{
		"localhost:27017[-5]":     {"localhost", 27017},
		"db-1.internal:27018[-1]": {"db-1.internal", 27018},
		"[::1]:27017[-2]":         {"::1", 27017},
	} {
		h, p := peer(in)
		if h != want.host || p != want.port {
			t.Errorf("peer(%q) = %q,%d want %q,%d", in, h, p, want.host, want.port)
		}
	}
}

func TestMonitor(t *testing.T) {
	tests := []struct {
		name     string
		opts     []Option
		cmd      bson.D
		fail     error
		wantName string
		wantText string
	}{
		{"find default no text", nil,
			bson.D{{Key: "find", Value: "users"}, {Key: "filter", Value: bson.D{{Key: "email", Value: "bob@example.com"}}}},
			nil, "find users", ""},
		{"insert with obfuscated text", []Option{WithCommandText()},
			bson.D{{Key: "insert", Value: "orders"}, {Key: "documents", Value: bson.A{bson.D{{Key: "card", Value: "4111-1111"}}}}, {Key: "lsid", Value: bson.D{{Key: "id", Value: 1}}}},
			nil, "insert orders", `{"insert":"orders","documents":[{"card":?}]}`},
		{"failure", nil, bson.D{{Key: "delete", Value: "carts"}}, errors.New("boom"), "delete carts", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := troopertest.New(t, "svc")
			mon := NewMonitor(tt.opts...)
			ctx, srv := otel.Tracer("x").Start(context.Background(), "srv", trace.WithSpanKind(trace.SpanKindServer))
			ctx, _ = trooper.BeginEntry(ctx, "GET", "/orders/:id", nil)

			cmd := raw(t, tt.cmd)
			mon.Started(ctx, &event.CommandStartedEvent{Command: cmd, DatabaseName: "shop", CommandName: tt.cmd[0].Key, RequestID: 7, ConnectionID: "mongo.local:27017[-3]"})
			fin := event.CommandFinishedEvent{CommandName: tt.cmd[0].Key, DatabaseName: "shop", RequestID: 7, ConnectionID: "mongo.local:27017[-3]"}
			if tt.fail != nil {
				mon.Failed(ctx, &event.CommandFailedEvent{CommandFinishedEvent: fin, Failure: tt.fail})
			} else {
				mon.Succeeded(ctx, &event.CommandSucceededEvent{CommandFinishedEvent: fin})
			}
			srv.End()

			s := h.SpanByKind(trace.SpanKindClient)
			if s == nil {
				t.Fatal("no client span")
			}
			if s.Name() != tt.wantName {
				t.Errorf("name = %q, want %q", s.Name(), tt.wantName)
			}
			for k, want := range map[attribute.Key]string{
				"db.system":           "mongodb",
				"db.namespace":        "shop",
				"db.collection.name":  tt.cmd[0].Value.(string),
				"server.address":      "mongo.local",
				"server.port":         "27017",
				"trooper.entry.route": "/orders/:id",
				"db.query.text":       tt.wantText,
			} {
				if got := troopertest.AttrString(s, k); got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
			for _, kv := range s.Attributes() {
				if v := kv.Value.Emit(); strings.Contains(v, "bob@example.com") || strings.Contains(v, "4111") {
					t.Errorf("value leaked in %s: %s", kv.Key, v)
				}
			}
			if (tt.fail != nil) != (s.Status().Code == codes.Error) {
				t.Errorf("status = %v", s.Status())
			}
		})
	}
}
