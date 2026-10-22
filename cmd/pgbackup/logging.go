package main

import (
	"io"
	"log/slog"
)

// newLogger builds the process logger. It is created once in main and passed
// explicitly to everything that logs; there is no global logger.
func newLogger(w io.Writer, level slog.Level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}
