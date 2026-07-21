package log

import (
	"bytes"
	"strings"
	"testing"
)

func TestLevelString(t *testing.T) {
	tests := []struct {
		level Level
		want  string
	}{
		{LevelDebug, "debug"},
		{LevelInfo, "info"},
		{LevelWarn, "warn"},
		{LevelError, "error"},
		{LevelFatal, "fatal"},
	}
	for _, tt := range tests {
		if got := tt.level.String(); got != tt.want {
			t.Errorf("Level(%d).String() = %q, want %q", tt.level, got, tt.want)
		}
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		input string
		want  Level
	}{
		{"debug", LevelDebug},
		{"DEBUG", LevelDebug},
		{"info", LevelInfo},
		{"INFO", LevelInfo},
		{"warn", LevelWarn},
		{"WARN", LevelWarn},
		{"warning", LevelWarn},
		{"error", LevelError},
		{"ERROR", LevelError},
		{"fatal", LevelFatal},
		{"FATAL", LevelFatal},
		{"unknown", LevelInfo},
		{"", LevelInfo},
	}
	for _, tt := range tests {
		if got := ParseLevel(tt.input); got != tt.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

type testRedactor struct{}

func (r testRedactor) String(s string) string {
	return strings.ReplaceAll(s, "secret", "[redacted]")
}

func (r testRedactor) Error(err error) string {
	if err == nil {
		return ""
	}
	return r.String(err.Error())
}

func TestLoggerOutput(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, LevelDebug)

	logger.Info("hello world")
	output := buf.String()
	if !strings.Contains(output, "level=INFO") {
		t.Errorf("expected level=INFO in output, got: %s", output)
	}
	if !strings.Contains(output, "hello world") {
		t.Errorf("expected message in output, got: %s", output)
	}

	buf.Reset()
	logger.Debug("debug message")
	output = buf.String()
	if !strings.Contains(output, "level=DEBUG") {
		t.Errorf("expected level=DEBUG in output, got: %s", output)
	}

	buf.Reset()
	logger.Warn("warn message")
	output = buf.String()
	if !strings.Contains(output, "level=WARN") {
		t.Errorf("expected level=WARN in output, got: %s", output)
	}

	buf.Reset()
	logger.Error("error message")
	output = buf.String()
	if !strings.Contains(output, "level=ERROR") {
		t.Errorf("expected level=ERROR in output, got: %s", output)
	}
}

func TestLoggerLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, LevelWarn)

	logger.Debug("should not appear")
	logger.Info("should not appear")
	if buf.Len() > 0 {
		t.Errorf("DEBUG/INFO should be filtered at WARN level, got: %s", buf.String())
	}

	buf.Reset()
	logger.Warn("should appear")
	if !strings.Contains(buf.String(), "should appear") {
		t.Errorf("WARN should appear at WARN level, got: %s", buf.String())
	}

	buf.Reset()
	logger.Error("should appear")
	if !strings.Contains(buf.String(), "should appear") {
		t.Errorf("ERROR should appear at WARN level, got: %s", buf.String())
	}
}

func TestLoggerFormattedOutput(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, LevelDebug)

	logger.Infof("count=%d", 42)
	output := buf.String()
	if !strings.Contains(output, "count=42") {
		t.Errorf("expected formatted message, got: %s", output)
	}
}

func TestLoggerWithArgs(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, LevelDebug)

	logger.Info("database opened", "path", "/data/bot.db", "mode", "wal")
	output := buf.String()
	if !strings.Contains(output, "path=/data/bot.db") {
		t.Errorf("expected path key=value in output, got: %s", output)
	}
	if !strings.Contains(output, "mode=wal") {
		t.Errorf("expected mode key=value in output, got: %s", output)
	}
}

func TestLoggerRedaction(t *testing.T) {
	var buf bytes.Buffer
	logger := NewWithRedactor(&buf, LevelDebug, testRedactor{})

	logger.Info("message with secret token")
	output := buf.String()
	if strings.Contains(output, "secret") {
		t.Errorf("secret should be redacted, got: %s", output)
	}
	if !strings.Contains(output, "[redacted]") {
		t.Errorf("expected [redacted] in output, got: %s", output)
	}
}

func TestLoggerRedactionInArgs(t *testing.T) {
	var buf bytes.Buffer
	logger := NewWithRedactor(&buf, LevelDebug, testRedactor{})

	logger.Info("error details", "token", "my-secret-key", "user", "admin")
	output := buf.String()
	if strings.Contains(output, "my-secret-key") {
		t.Errorf("secret in args should be redacted, got: %s", output)
	}
	if !strings.Contains(output, "user=admin") {
		t.Errorf("non-secret args should be preserved, got: %s", output)
	}
}

func TestPrintfCompat(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, LevelDebug)

	logger.Printf("compat message %d", 1)
	output := buf.String()
	if !strings.Contains(output, "level=INFO") {
		t.Errorf("Printf should log at INFO level, got: %s", output)
	}
	if !strings.Contains(output, "compat message 1") {
		t.Errorf("expected compat message, got: %s", output)
	}
}

func TestPrintlnCompat(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, LevelDebug)

	logger.Println("compat", "message")
	output := buf.String()
	if !strings.Contains(output, "level=INFO") {
		t.Errorf("Println should log at INFO level, got: %s", output)
	}
}

func TestPackageLevelFunctions(t *testing.T) {
	var buf bytes.Buffer
	SetDefault(New(&buf, LevelDebug))

	Info("package level info")
	output := buf.String()
	if !strings.Contains(output, "level=INFO") {
		t.Errorf("package Info should work, got: %s", output)
	}

	buf.Reset()
	Warn("package level warn")
	output = buf.String()
	if !strings.Contains(output, "level=WARN") {
		t.Errorf("package Warn should work, got: %s", output)
	}

	buf.Reset()
	Error("package level error")
	output = buf.String()
	if !strings.Contains(output, "level=ERROR") {
		t.Errorf("package Error should work, got: %s", output)
	}

	// Reset to nil to restore default behavior for other tests.
	SetDefault(nil)
}

func TestWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, LevelDebug)

	child := logger.With("component", "parser")
	child.Info("processing channel")
	output := buf.String()
	if !strings.Contains(output, "component=parser") {
		t.Errorf("expected component=parser in output, got: %s", output)
	}
	if !strings.Contains(output, "processing channel") {
		t.Errorf("expected message in output, got: %s", output)
	}
}
