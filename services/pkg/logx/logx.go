// Package logx builds the JSON slog logger every service uses.
package logx

import (
	"log/slog"
	"os"
	"strings"
)

// New returns a JSON logger at level ("debug", "info", "warn", "error") tagged with service.
func New(service, level string) *slog.Logger {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l})).With("service", service)
}
