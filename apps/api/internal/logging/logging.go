// Package logging builds the service's structured logger.
package logging

import (
	"io"
	"log/slog"
	"strings"
)

// New returns a JSON slog.Logger tagged with the service name, as required by
// the Observability standard (structured logs, consistent field names).
// An unrecognised level falls back to info rather than failing startup.
func New(w io.Writer, service, level string) *slog.Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: parseLevel(level)})

	return slog.New(handler).With(slog.String("service", service))
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
