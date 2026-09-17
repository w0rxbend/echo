package main

import (
	"log/slog"
	"strings"
	"testing"
)

func TestParseLogLevelAcceptsEveryAdvertisedName(t *testing.T) {
	want := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}

	for name, wantLevel := range want {
		level, err := parseLogLevel(name)
		if err != nil {
			t.Fatalf("parseLogLevel(%q): %v", name, err)
		}
		if level.Level() != wantLevel {
			t.Fatalf("parseLogLevel(%q) = %v, want %v", name, level.Level(), wantLevel)
		}
	}

	if got := len(logLevelParsers); got != len(want) {
		t.Fatalf("logLevelParsers has %d entries, want %d: an added level needs a case here and in the -log-level help text", got, len(want))
	}
}

// A typo used to start the process at info with nothing said, so an operator
// reaching for debug during an incident got no extra lines and no hint why.
func TestParseLogLevelRejectsUnknownValue(t *testing.T) {
	for _, value := range []string{"debg", "DEBUG", "trace", "", "info "} {
		level, err := parseLogLevel(value)
		if err == nil {
			t.Fatalf("parseLogLevel(%q) = %v, want an error", value, level)
		}
		if level != nil {
			t.Fatalf("parseLogLevel(%q) returned level %v alongside an error", value, level)
		}
		// The message has to name the bad value and the way out; an operator
		// reads it on stderr with no logger configured yet.
		if !strings.Contains(err.Error(), value) && value != "" {
			t.Fatalf("parseLogLevel(%q) error %q does not name the rejected value", value, err)
		}
		for name := range logLevelParsers {
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("parseLogLevel(%q) error %q does not offer %q", value, err, name)
			}
		}
	}
}

// The help text and the error message both render this list, so it must be
// stable rather than map-iteration order.
func TestLogLevelNamesAreOrderedBySeverity(t *testing.T) {
	got := logLevelNames()
	want := []string{"debug", "info", "warn", "error"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("logLevelNames() = %v, want %v", got, want)
	}
}
