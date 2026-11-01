package selfobs

import (
	"context"
	"log/slog"

	trooperslog "github.com/makifbaysal/tasktrooper-telemetry/sdks/go/integrations/slog"
)

// LogHandler wraps the service's local handler: records go to local as before and also to OTel logs (via
// trooperslog, at the local handler's minimum level), except records logged with a suppressed (self-project) context,
// which stay local only.
func LogHandler(local slog.Handler, name string) slog.Handler {
	return &logHandler{local: local, both: trooperslog.NewHandler(name, local, trooperslog.WithLevel(minLevel(local)))}
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
