package troopermongo

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const scopeName = "github.com/tasktrooper/telemetry-go/integrations/mongo"

type config struct {
	tp          trace.TracerProvider
	commandText bool
	next        *event.CommandMonitor
}

// Option configures NewMonitor.
type Option func(*config)

// WithTracerProvider overrides the global TracerProvider.
func WithTracerProvider(tp trace.TracerProvider) Option { return func(c *config) { c.tp = tp } }

// WithCommandText records db.query.text with all values replaced by "?" (keys only).
func WithCommandText() Option { return func(c *config) { c.commandText = true } }

// WithNext chains another CommandMonitor (called after the trooper monitor).
func WithNext(m *event.CommandMonitor) Option { return func(c *config) { c.next = m } }

type spanKey struct {
	conn string
	id   int64
}

type monitor struct {
	cfg   config
	spans sync.Map // spanKey -> trace.Span
}

// NewMonitor returns a CommandMonitor that traces every command.
func NewMonitor(opts ...Option) *event.CommandMonitor {
	m := &monitor{}
	for _, o := range opts {
		o(&m.cfg)
	}
	return &event.CommandMonitor{Started: m.started, Succeeded: m.succeeded, Failed: m.failed}
}

func (m *monitor) tracer() trace.Tracer {
	tp := m.cfg.tp
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	return tp.Tracer(scopeName)
}

func (m *monitor) started(ctx context.Context, evt *event.CommandStartedEvent) {
	defer func() { _ = recover() }()
	if m.cfg.next != nil && m.cfg.next.Started != nil {
		defer m.cfg.next.Started(ctx, evt)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	attrs := []attribute.KeyValue{
		attribute.String("db.system", "mongodb"),
		attribute.String("db.system.name", "mongodb"),
		attribute.String("db.operation.name", evt.CommandName),
	}
	if evt.DatabaseName != "" {
		attrs = append(attrs, attribute.String("db.namespace", evt.DatabaseName))
	}
	coll := collection(evt.Command)
	if coll != "" {
		attrs = append(attrs, attribute.String("db.collection.name", coll))
	}
	if host, port := peer(evt.ConnectionID); host != "" {
		attrs = append(attrs, attribute.String("server.address", host))
		if port > 0 {
			attrs = append(attrs, attribute.Int("server.port", port))
		}
	}
	if m.cfg.commandText {
		attrs = append(attrs, attribute.String("db.query.text", Obfuscate(evt.Command)))
	}
	name := evt.CommandName
	if coll != "" {
		name += " " + coll
	}
	_, span := m.tracer().Start(ctx, name, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	m.spans.Store(spanKey{evt.ConnectionID, evt.RequestID}, span)
}

func (m *monitor) end(evt event.CommandFinishedEvent, err error) {
	v, ok := m.spans.LoadAndDelete(spanKey{evt.ConnectionID, evt.RequestID})
	if !ok {
		return
	}
	span := v.(trace.Span)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

func (m *monitor) succeeded(ctx context.Context, evt *event.CommandSucceededEvent) {
	defer func() { _ = recover() }()
	m.end(evt.CommandFinishedEvent, nil)
	if m.cfg.next != nil && m.cfg.next.Succeeded != nil {
		m.cfg.next.Succeeded(ctx, evt)
	}
}

func (m *monitor) failed(ctx context.Context, evt *event.CommandFailedEvent) {
	defer func() { _ = recover() }()
	m.end(evt.CommandFinishedEvent, evt.Failure)
	if m.cfg.next != nil && m.cfg.next.Failed != nil {
		m.cfg.next.Failed(ctx, evt)
	}
}

// collection returns the collection name: the string value of the first command element
// (e.g. {"find": "users"}).
func collection(cmd bson.Raw) string {
	elems, err := cmd.Elements()
	if err != nil || len(elems) == 0 {
		return ""
	}
	s, ok := elems[0].Value().StringValueOK()
	if !ok {
		return ""
	}
	return s
}

// peer parses a driver connection ID ("host:port[-N]") into host and port.
func peer(connID string) (string, int) {
	if i := strings.LastIndexByte(connID, '['); i > 0 { // driver format: "host:port[-N]"
		connID = connID[:i]
	}
	host, p, err := net.SplitHostPort(connID)
	if err != nil {
		return connID, 0
	}
	port, _ := strconv.Atoi(p)
	return host, port
}

var metaKeys = map[string]bool{"lsid": true, "$clusterTime": true, "$db": true, "$readPreference": true, "txnNumber": true, "signature": true}

// Obfuscate renders a command document as JSON-like text with every scalar value replaced by "?".
// The command name and collection (first element) are kept.
func Obfuscate(cmd bson.Raw) string {
	var b strings.Builder
	writeDoc(&b, cmd, true)
	return b.String()
}

func writeDoc(b *strings.Builder, doc bson.Raw, top bool) {
	elems, err := doc.Elements()
	if err != nil {
		b.WriteString("?")
		return
	}
	b.WriteByte('{')
	n := 0
	for i, e := range elems {
		k := e.Key()
		if top && metaKeys[k] {
			continue
		}
		if n > 0 {
			b.WriteByte(',')
		}
		n++
		b.WriteString(strconv.Quote(k))
		b.WriteByte(':')
		v := e.Value()
		switch {
		case top && i == 0 && v.Type == bson.TypeString:
			b.WriteString(strconv.Quote(v.StringValue()))
		case v.Type == bson.TypeEmbeddedDocument:
			writeDoc(b, v.Document(), false)
		case v.Type == bson.TypeArray:
			vals, _ := v.Array().Values()
			b.WriteByte('[')
			for j, av := range vals {
				if j > 0 {
					b.WriteByte(',')
				}
				if av.Type == bson.TypeEmbeddedDocument {
					writeDoc(b, av.Document(), false)
				} else {
					b.WriteString("?")
				}
			}
			b.WriteByte(']')
		default:
			b.WriteString("?")
		}
	}
	b.WriteByte('}')
}
