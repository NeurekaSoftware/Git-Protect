// Package logging configures the application's console logger.
//
// The daemon logs structured plain-language records to stdout through the
// package-level slog default logger. The level threshold is a mutable
// slog.LevelVar so a settings reload can retune verbosity in place while other
// goroutines are mid-write.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// DefaultLevelName is the level used when settings do not configure one.
const DefaultLevelName = "info"

var levelVar = new(slog.LevelVar)

// Install sets the process-wide default logger to write text records to stdout,
// steered by the shared level threshold.
func Install() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: levelVar})))
}

// SupportedLevelNames returns the level names accepted in settings, in the order
// the validator lists them in its error message.
func SupportedLevelNames() []string {
	return []string{"debug", "info", "warn", "error"}
}

// ParseLevel maps a configured level name onto a slog level. It accepts the
// names from SupportedLevelNames case-insensitively and reports false for
// anything else.
func ParseLevel(name string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

// SetLevel adjusts the threshold of the installed default logger in place. A
// reload that leaves the level unchanged costs nothing.
func SetLevel(level slog.Level) {
	levelVar.Set(level)
}
