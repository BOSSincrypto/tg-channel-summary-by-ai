package db

import (
	"errors"
	"strings"
)

// Sentinel errors for repository operations.
var (
	// ErrNotFound is returned when a single-row lookup finds no result.
	ErrNotFound = errors.New("not found")

	// ErrDuplicate is returned when an INSERT violates a UNIQUE constraint.
	ErrDuplicate = errors.New("duplicate entry")

	// ErrConflict is returned when an optimistic-lock version is stale.
	ErrConflict = errors.New("optimistic lock conflict")
)

// boolToInt converts a bool to an integer (0 or 1) for SQLite storage.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// intToBool converts an integer (0 or 1) to a bool.
func intToBool(i int) bool {
	return i != 0
}

// isUniqueViolation checks whether the given error is a SQLite UNIQUE constraint
// violation. This is detected by checking the error message since modernc.org/sqlite
// does not expose structured error codes via the database/sql interface.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed")
}
