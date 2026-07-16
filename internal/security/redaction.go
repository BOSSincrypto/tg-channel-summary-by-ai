// Package security contains small, shared safeguards for sensitive runtime data.
package security

import (
	"net/url"
	"sort"
	"strings"
)

const minimumRedactableSecretLength = 4
const minimumFragmentLength = 12

// Redactor removes configured secrets from diagnostics while retaining the
// surrounding status and error context.
type Redactor struct {
	secrets []string
}

// NewRedactor creates a redactor for the supplied credentials. Empty and very
// short values are ignored because replacing them would corrupt normal text.
func NewRedactor(secrets ...string) *Redactor {
	unique := make(map[string]struct{}, len(secrets))
	cleaned := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if len(secret) < minimumRedactableSecretLength {
			continue
		}
		if _, exists := unique[secret]; exists {
			continue
		}
		unique[secret] = struct{}{}
		cleaned = append(cleaned, secret)
	}
	sort.SliceStable(cleaned, func(i, j int) bool {
		return len(cleaned[i]) > len(cleaned[j])
	})
	return &Redactor{secrets: cleaned}
}

// String sanitizes a diagnostic string.
func (r *Redactor) String(value string) string {
	if r == nil || value == "" {
		return value
	}
	for _, secret := range r.secrets {
		replacements := []string{secret}
		if escaped := url.QueryEscape(secret); escaped != secret {
			replacements = append(replacements, escaped)
		}
		if escaped := url.PathEscape(secret); escaped != secret {
			replacements = append(replacements, escaped)
		}
		if len(secret) >= minimumFragmentLength+4 {
			replacements = append(replacements, secret[:minimumFragmentLength])
			replacements = append(replacements, secret[len(secret)-minimumFragmentLength:])
		}
		sort.SliceStable(replacements, func(i, j int) bool {
			return len(replacements[i]) > len(replacements[j])
		})
		for _, replacement := range replacements {
			value = strings.ReplaceAll(value, replacement, "[redacted]")
		}
	}
	return value
}

// Error sanitizes an error while preserving its safe diagnostic text.
func (r *Redactor) Error(err error) string {
	if err == nil {
		return ""
	}
	return r.String(err.Error())
}

// Wrap returns a sanitized error with the supplied context.
func (r *Redactor) Wrap(context string, err error) error {
	if err == nil {
		return nil
	}
	message := r.Error(err)
	if context != "" {
		message = context + ": " + message
	}
	return &sanitizedError{message: message, cause: err}
}

type sanitizedError struct {
	message string
	cause   error
}

func (e *sanitizedError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func (e *sanitizedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}
