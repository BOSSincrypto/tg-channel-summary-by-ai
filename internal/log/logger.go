// Package log provides structured logging built on log/slog with
// secret redaction and configurable log levels. It supports five levels:
// DEBUG, INFO, WARN, ERROR, and FATAL.
//
// Package-level functions operate on a default logger that can be replaced
// via SetDefault. The default logger writes key=value text to stderr at INFO
// level with no redaction until configured.
package log

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// Level represents a log severity.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
	LevelFatal
)

// String returns the lower-case level name used in structured output.
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	case LevelFatal:
		return "fatal"
	default:
		return "info"
	}
}

// ParseLevel converts a case-insensitive string to a Level.
// Unknown values return LevelInfo.
func ParseLevel(raw string) Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return LevelDebug
	case "info":
		return LevelInfo
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	case "fatal":
		return LevelFatal
	default:
		return LevelInfo
	}
}

// Redactor sanitizes values before they are logged.
type Redactor interface {
	String(string) string
	Error(error) string
}

// Logger is a structured logger with secret redaction.
type Logger struct {
	inner    *slog.Logger
	level    Level
	redactor Redactor
	mu       sync.Mutex
}

// New creates a Logger that writes to w at the given level.
func New(w io.Writer, level Level) *Logger {
	if w == nil {
		w = os.Stderr
	}
	handler := slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: slogLevel(level),
	})
	return &Logger{
		inner: slog.New(handler),
		level: level,
	}
}

// NewWithRedactor creates a Logger that redacts secrets via the supplied
// Redactor before writing log entries.
func NewWithRedactor(w io.Writer, level Level, redactor Redactor) *Logger {
	if w == nil {
		w = os.Stderr
	}
	base := slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: slogLevel(level),
	})
	handler := &redactHandler{next: base, redactor: redactor}
	return &Logger{
		inner:    slog.New(handler),
		level:    level,
		redactor: redactor,
	}
}

// Debug logs at DEBUG level.
func (l *Logger) Debug(msg string, args ...any) {
	l.log(context.Background(), slog.LevelDebug, msg, args...)
}

// Debugf formats and logs at DEBUG level.
func (l *Logger) Debugf(format string, args ...any) {
	l.log(context.Background(), slog.LevelDebug, fmt.Sprintf(format, args...))
}

// Info logs at INFO level.
func (l *Logger) Info(msg string, args ...any) {
	l.log(context.Background(), slog.LevelInfo, msg, args...)
}

// Infof formats and logs at INFO level.
func (l *Logger) Infof(format string, args ...any) {
	l.log(context.Background(), slog.LevelInfo, fmt.Sprintf(format, args...))
}

// Warn logs at WARN level.
func (l *Logger) Warn(msg string, args ...any) {
	l.log(context.Background(), slog.LevelWarn, msg, args...)
}

// Warnf formats and logs at WARN level.
func (l *Logger) Warnf(format string, args ...any) {
	l.log(context.Background(), slog.LevelWarn, fmt.Sprintf(format, args...))
}

// Error logs at ERROR level.
func (l *Logger) Error(msg string, args ...any) {
	l.log(context.Background(), slog.LevelError, msg, args...)
}

// Errorf formats and logs at ERROR level.
func (l *Logger) Errorf(format string, args ...any) {
	l.log(context.Background(), slog.LevelError, fmt.Sprintf(format, args...))
}

// Fatal logs at ERROR level and then calls os.Exit(1).
func (l *Logger) Fatal(msg string, args ...any) {
	l.log(context.Background(), slog.LevelError, msg, args...)
	os.Exit(1)
}

// Fatalf formats, logs at ERROR level, and then calls os.Exit(1).
func (l *Logger) Fatalf(format string, args ...any) {
	l.log(context.Background(), slog.LevelError, fmt.Sprintf(format, args...))
	os.Exit(1)
}

// Printf formats and logs at INFO level. It exists as a migration aid for
// callers transitioning from the standard log package.
func (l *Logger) Printf(format string, args ...any) {
	l.Infof(format, args...)
}

// Println logs at INFO level. It exists as a migration aid for callers
// transitioning from the standard log package.
func (l *Logger) Println(args ...any) {
	l.Info(fmt.Sprint(args...))
}

// With returns a Logger with the given attributes permanently attached.
func (l *Logger) With(args ...any) *Logger {
	if l == nil {
		return nil
	}
	return &Logger{
		inner:    l.inner.With(args...),
		level:    l.level,
		redactor: l.redactor,
	}
}

func (l *Logger) log(ctx context.Context, level slog.Level, msg string, args ...any) {
	if l == nil || !l.inner.Enabled(ctx, level) {
		return
	}
	if l.redactor != nil {
		msg = l.redactor.String(msg)
		redacted := make([]any, len(args))
		for i, arg := range args {
			if s, ok := arg.(string); ok {
				redacted[i] = l.redactor.String(s)
			} else {
				redacted[i] = arg
			}
		}
		args = redacted
	}
	l.inner.Log(ctx, level, msg, args...)
}

func slogLevel(level Level) slog.Leveler {
	switch level {
	case LevelDebug:
		return slog.LevelDebug
	case LevelInfo:
		return slog.LevelInfo
	case LevelWarn:
		return slog.LevelWarn
	case LevelError:
		return slog.LevelError
	case LevelFatal:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// redactHandler wraps a slog.Handler and redacts secrets from log records.
type redactHandler struct {
	next     slog.Handler
	redactor Redactor
}

func (h *redactHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *redactHandler) Handle(ctx context.Context, record slog.Record) error {
	if h.redactor == nil {
		return h.next.Handle(ctx, record)
	}
	record.Message = h.redactor.String(record.Message)
	record.Attrs(func(attr slog.Attr) bool {
		return true
	})
	return h.next.Handle(ctx, record)
}

func (h *redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if h.redactor == nil {
		return &redactHandler{next: h.next.WithAttrs(attrs), redactor: nil}
	}
	redacted := make([]slog.Attr, len(attrs))
	for i, attr := range attrs {
		if attr.Value.Kind() == slog.KindString {
			redacted[i] = slog.String(attr.Key, h.redactor.String(attr.Value.String()))
		} else {
			redacted[i] = attr
		}
	}
	return &redactHandler{next: h.next.WithAttrs(redacted), redactor: h.redactor}
}

func (h *redactHandler) WithGroup(name string) slog.Handler {
	return &redactHandler{next: h.next.WithGroup(name), redactor: h.redactor}
}
