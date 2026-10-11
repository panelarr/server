// SPDX-License-Identifier: GPL-3.0-or-later

// Package logging builds the server's structured logger.
package logging

import (
	"io"
	"log/slog"

	"github.com/panelarr/server/internal/config"
)

// New returns a logger writing JSON lines (or text) at the configured level.
// Every line carries service=server; components add their own attributes
// with Component.
func New(cfg config.Log, w io.Writer) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.Level))
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if cfg.Format == "text" {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h).With("service", "server")
}

// Component returns a child logger for one part of the server.
func Component(l *slog.Logger, name string) *slog.Logger { return l.With("component", name) }
