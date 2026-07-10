// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"":        slog.LevelInfo,
		"bogus":   slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNewJSONFormat(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, FormatJSON, "info")
	log.Info("hello", "k", "v")

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, buf.String())
	}
	if rec["msg"] != "hello" || rec["k"] != "v" {
		t.Errorf("unexpected JSON record: %v", rec)
	}
}

func TestNewTextFormatDefault(t *testing.T) {
	var buf bytes.Buffer
	// Unknown format falls back to text (not JSON).
	log := New(&buf, "wat", "info")
	log.Info("hello", "k", "v")
	out := buf.String()
	if !strings.Contains(out, "msg=hello") || !strings.Contains(out, "k=v") {
		t.Errorf("expected text output, got: %q", out)
	}
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("expected text, got JSON: %q", out)
	}
}

func TestLevelFiltersDebug(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, FormatText, "info")
	log.Debug("should be filtered")
	if buf.Len() != 0 {
		t.Errorf("debug line emitted at info level: %q", buf.String())
	}
	log.Warn("should appear")
	if !strings.Contains(buf.String(), "should appear") {
		t.Errorf("warn line missing: %q", buf.String())
	}
}

func TestFromConfigEnvOverride(t *testing.T) {
	t.Setenv(EnvFormat, "json")
	t.Setenv(EnvLevel, "debug")
	var buf bytes.Buffer
	log := FromConfig(&buf, FormatText, "error") // config says text/error; env wins
	log.Debug("dbg")
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("env did not switch to JSON: %v\n%s", err, buf.String())
	}
	if rec["msg"] != "dbg" {
		t.Errorf("env did not lower level to debug: %v", rec)
	}
}
