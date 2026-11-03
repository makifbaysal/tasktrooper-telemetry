package selftel

import (
	"context"
	"log/slog"

	trooper "github.com/makifbaysal/tasktrooper-telemetry/sdks/go"
)

// entryHandler adds trooper.entry.route / trooper.entry.method from the request/job entry in ctx
// (trooper.BeginEntry: HTTP integrations, StartJob) to every record, so the ingest logs `route` column is filled.
// The pinned SDK's trooperslog (v0.1.0) does not do this itself; drop this wrapper once the SDK version that does
// is required in go.mod (otherwise the attributes are added twice).
type entryHandler struct{ slog.Handler }

func (h entryHandler) Handle(ctx context.Context, r slog.Record) error {
	if route, method := trooper.EntryFromContext(ctx).Route(); route != "" || method != "" {
		r = r.Clone()
		if route != "" {
			r.AddAttrs(slog.String(string(trooper.AttrEntryRoute), route))
		}
		if method != "" {
			r.AddAttrs(slog.String(string(trooper.AttrEntryMethod), method))
		}
	}
	return h.Handler.Handle(ctx, r)
}

func (h entryHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return entryHandler{h.Handler.WithAttrs(attrs)}
}

func (h entryHandler) WithGroup(name string) slog.Handler {
	return entryHandler{h.Handler.WithGroup(name)}
}
