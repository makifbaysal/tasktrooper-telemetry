package selftel

import (
	"context"
	"log/slog"

	trooper "github.com/makifbaysal/tasktrooper-telemetry/sdks/go"
)

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
