package selfobs

import (
	"context"
	"log/slog"

	trooper "github.com/makifbaysal/tasktrooper-telemetry/sdks/go"
	trooperslog "github.com/makifbaysal/tasktrooper-telemetry/sdks/go/integrations/slog"
)

// LogHandler wraps the service's local handler: records go to local as before and also to OTel logs (via
// trooperslog, at the local handler's minimum level), except records logged with a suppressed (self-project) context,
// which stay local only. Records logged with a request/consumer context carry trooper.entry.route / .method (EntryRoute).
func LogHandler(local slog.Handler, name string) slog.Handler {
	return &logHandler{local: local, both: EntryRoute(trooperslog.NewHandler(name, local, trooperslog.WithLevel(minLevel(local))))}
}

type logHandler struct {
	local slog.Handler // the unchanged local handler
	both  slog.Handler // local + OTel
}

func minLevel(h slog.Handler) slog.Level {
	for _, l := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn} {
		if h.Enabled(context.Background(), l) {
			return l
		}
	}
	return slog.LevelError
}

func (h *logHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.both.Enabled(ctx, l) }

func (h *logHandler) Handle(ctx context.Context, r slog.Record) error {
	if Suppressed(ctx) {
		if h.local.Enabled(ctx, r.Level) {
			return h.local.Handle(ctx, r)
		}
		return nil
	}
	return h.both.Handle(ctx, r)
}

func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logHandler{local: h.local.WithAttrs(attrs), both: h.both.WithAttrs(attrs)}
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	return &logHandler{local: h.local.WithGroup(name), both: h.both.WithGroup(name)}
}

// EntryRoute wraps next so records logged with a context under an SDK entry (HTTP route template, consumer process or
// job name; trooper.BeginEntry) get the trooper.entry.route / trooper.entry.method attributes the ingest flatten
// stores as the log row's route (the pinned trooperslog bridge adds only trace_id/span_id). Records without an
// entry are passed through unchanged.
func EntryRoute(next slog.Handler) slog.Handler { return entryHandler{next: next} }

type entryHandler struct{ next slog.Handler }

func (h entryHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h entryHandler) Handle(ctx context.Context, r slog.Record) error {
	if route, method := trooper.EntryFromContext(ctx).Route(); route != "" {
		r = r.Clone()
		r.AddAttrs(slog.String(string(trooper.AttrEntryRoute), route))
		if method != "" {
			r.AddAttrs(slog.String(string(trooper.AttrEntryMethod), method))
		}
	}
	return h.next.Handle(ctx, r)
}

func (h entryHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return entryHandler{next: h.next.WithAttrs(attrs)}
}

func (h entryHandler) WithGroup(name string) slog.Handler {
	return entryHandler{next: h.next.WithGroup(name)}
}
