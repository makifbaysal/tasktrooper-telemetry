package notify

import (
	"context"
	"errors"
	"net/smtp"
	"testing"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/makifbaysal/tasktrooper-telemetry/sdks/go/troopertest"
)

func TestEmailClientSpan(t *testing.T) {
	h := troopertest.New(t, "tt-notifier")
	d := NewDeliverer(SMTPConfig{Addr: "smtp.example.com:587", From: "alerts@x.io"})
	d.sendMail = func(string, smtp.Auth, string, []string, []byte) error { return errors.New("421 try later") }
	ch := Channel{Name: "mail", Type: "email", Config: map[string]any{"to": "a@x.io"}}
	if err := d.Deliver(context.Background(), ch, "firing", firing); err == nil {
		t.Fatal("want error")
	}
	s := h.SpanByName("smtp send")
	if s == nil || s.SpanKind() != trace.SpanKindClient || troopertest.AttrString(s, "peer.service") != "smtp" ||
		s.Status().Code != codes.Error || !troopertest.HasException(s) {
		t.Fatalf("smtp span %+v", s)
	}
}
