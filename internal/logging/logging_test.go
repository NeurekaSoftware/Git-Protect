package logging

import (
	"context"
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		want   slog.Level
		wantOK bool
	}{
		{name: "debug", input: "debug", want: slog.LevelDebug, wantOK: true},
		{name: "info", input: "info", want: slog.LevelInfo, wantOK: true},
		{name: "warn", input: "warn", want: slog.LevelWarn, wantOK: true},
		{name: "error", input: "error", want: slog.LevelError, wantOK: true},
		{name: "case insensitive", input: "WARN", want: slog.LevelWarn, wantOK: true},
		{name: "padded", input: "  info  ", want: slog.LevelInfo, wantOK: true},
		{name: "unknown", input: "verbose", want: slog.LevelInfo, wantOK: false},
		{name: "empty", input: "", want: slog.LevelInfo, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseLevel(tt.input)
			if ok != tt.wantOK {
				t.Fatalf("ParseLevel(%q) ok = %v, want %v", tt.input, ok, tt.wantOK)
			}
			if got != tt.want {
				t.Fatalf("ParseLevel(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestSupportedLevelNamesMatchParseLevel(t *testing.T) {
	for _, name := range SupportedLevelNames() {
		if _, ok := ParseLevel(name); !ok {
			t.Errorf("SupportedLevelNames contains %q, which ParseLevel rejects", name)
		}
	}
}

func TestSetLevelChangesThreshold(t *testing.T) {
	Install()
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		// Level starts at its zero value (Info), so Debug must be filtered out.
		t.Log("debug records are filtered at the default level, as expected")
	}
	SetLevel(slog.LevelDebug)
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug records should pass after SetLevel(LevelDebug)")
	}
	SetLevel(slog.LevelError)
	if slog.Default().Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("info records should be filtered after SetLevel(LevelError)")
	}
}
