package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestNewEmitsJSONWithServiceField(t *testing.T) {
	var buf bytes.Buffer

	New(&buf, "collabboard-api", "info").Info("hello", slog.String("key", "value"))

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log line %q is not JSON: %v", buf.String(), err)
	}

	for field, want := range map[string]string{
		"service": "collabboard-api",
		"msg":     "hello",
		"level":   "INFO",
		"key":     "value",
	} {
		if got, _ := entry[field].(string); got != want {
			t.Errorf("field %q = %q, want %q", field, got, want)
		}
	}
}

func TestParseLevel(t *testing.T) {
	tests := map[string]slog.Level{
		"debug":     slog.LevelDebug,
		"WARN":      slog.LevelWarn,
		" warning ": slog.LevelWarn,
		"error":     slog.LevelError,
		"":          slog.LevelInfo,
		"nonsense":  slog.LevelInfo,
	}

	for input, want := range tests {
		if got := parseLevel(input); got != want {
			t.Errorf("parseLevel(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestNewRespectsLevel(t *testing.T) {
	var buf bytes.Buffer

	New(&buf, "collabboard-api", "error").Info("should be filtered out")

	if buf.Len() != 0 {
		t.Errorf("expected info log to be filtered at error level, got %q", buf.String())
	}
}
