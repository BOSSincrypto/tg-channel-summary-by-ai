package log

import (
	"os"
	"sync"
)

var (
	defaultLogger     *Logger
	defaultLoggerOnce sync.Once
	defaultLoggerMu   sync.RWMutex
)

// GetDefault returns the package-level logger, creating it if necessary.
func GetDefault() *Logger {
	defaultLoggerMu.RLock()
	l := defaultLogger
	defaultLoggerMu.RUnlock()
	if l != nil {
		return l
	}
	defaultLoggerOnce.Do(func() {
		defaultLoggerMu.Lock()
		defaultLogger = New(os.Stderr, LevelInfo)
		defaultLoggerMu.Unlock()
	})
	defaultLoggerMu.RLock()
	defer defaultLoggerMu.RUnlock()
	return defaultLogger
}

// SetDefault replaces the package-level logger. Pass nil to restore the
// original stderr INFO-level logger.
func SetDefault(l *Logger) {
	defaultLoggerMu.Lock()
	defer defaultLoggerMu.Unlock()
	if l == nil {
		defaultLogger = New(os.Stderr, LevelInfo)
	} else {
		defaultLogger = l
	}
}

// Debug logs at DEBUG level using the default logger.
func Debug(msg string, args ...any) {
	GetDefault().Debug(msg, args...)
}

// Debugf formats and logs at DEBUG level using the default logger.
func Debugf(format string, args ...any) {
	GetDefault().Debugf(format, args...)
}

// Info logs at INFO level using the default logger.
func Info(msg string, args ...any) {
	GetDefault().Info(msg, args...)
}

// Infof formats and logs at INFO level using the default logger.
func Infof(format string, args ...any) {
	GetDefault().Infof(format, args...)
}

// Warn logs at WARN level using the default logger.
func Warn(msg string, args ...any) {
	GetDefault().Warn(msg, args...)
}

// Warnf formats and logs at WARN level using the default logger.
func Warnf(format string, args ...any) {
	GetDefault().Warnf(format, args...)
}

// Error logs at ERROR level using the default logger.
func Error(msg string, args ...any) {
	GetDefault().Error(msg, args...)
}

// Errorf formats and logs at ERROR level using the default logger.
func Errorf(format string, args ...any) {
	GetDefault().Errorf(format, args...)
}

// Fatal logs at ERROR level and exits using the default logger.
func Fatal(msg string, args ...any) {
	GetDefault().Fatal(msg, args...)
}

// Fatalf formats, logs at ERROR level, and exits using the default logger.
func Fatalf(format string, args ...any) {
	GetDefault().Fatalf(format, args...)
}

// Printf formats and logs at INFO level using the default logger. This is a
// migration aid for callers moving from the standard log package.
func Printf(format string, args ...any) {
	GetDefault().Printf(format, args...)
}

// Println logs at INFO level using the default logger. This is a migration
// aid for callers moving from the standard log package.
func Println(args ...any) {
	GetDefault().Println(args...)
}
