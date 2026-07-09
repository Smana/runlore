// SPDX-License-Identifier: Apache-2.0

// Package logging builds RunLore's slog logger with a configurable output format
// (human-readable text or structured JSON) and verbosity level. In-cluster
// deployments choose JSON so logs are ingestible by Loki/VictoriaLogs/CloudWatch;
// local CLI runs keep text. Format and level come from config, with the
// RUNLORE_LOG_FORMAT / RUNLORE_LOG_LEVEL env vars overriding (so verbosity is
// tunable without editing config — useful for a quick debug session).
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Format identifiers for the log handler.
const (
	FormatText = "text" // default; human-readable
	FormatJSON = "json" // structured, for log aggregation backends
)

// Env vars that override the configured format/level at startup.
const (
	EnvFormat = "RUNLORE_LOG_FORMAT"
	EnvLevel  = "RUNLORE_LOG_LEVEL"
)

// New returns a slog.Logger writing to w with the given format and level.
// An unrecognized format falls back to text; an unrecognized/empty level to info.
func New(w io.Writer, format, level string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: ParseLevel(level)}
	var h slog.Handler
	if strings.EqualFold(strings.TrimSpace(format), FormatJSON) {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h)
}

// FromConfig builds a logger from the configured format/level, with the
// RUNLORE_LOG_FORMAT / RUNLORE_LOG_LEVEL env vars taking precedence when set.
func FromConfig(w io.Writer, format, level string) *slog.Logger {
	if v := os.Getenv(EnvFormat); v != "" {
		format = v
	}
	if v := os.Getenv(EnvLevel); v != "" {
		level = v
	}
	return New(w, format, level)
}

// ParseLevel maps a level name (case-insensitive) to a slog.Level. Unknown or
// empty values map to Info — the safe production default.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
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
