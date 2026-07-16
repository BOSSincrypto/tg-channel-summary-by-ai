package security

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRedactorSanitizesConfiguredSecretsAcrossWrappedDiagnostics(t *testing.T) {
	redactor := NewRedactor("openrouter-secret", "custom-provider-secret")
	original := fmt.Errorf("provider response: %w", errors.New(
		"HTTP 502 from https://provider.invalid/chat?api_key=custom-provider-secret: Authorization Bearer openrouter-secret",
	))

	got := redactor.Error(original)
	if strings.Contains(got, "openrouter-secret") || strings.Contains(got, "custom-provider-secret") {
		t.Fatalf("sanitized diagnostic leaked a configured secret: %q", got)
	}
	for _, want := range []string{"provider response", "HTTP 502", "[redacted]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("sanitized diagnostic %q does not contain safe context %q", got, want)
		}
	}
}

func TestRedactorIgnoresEmptyAndShortSecrets(t *testing.T) {
	redactor := NewRedactor("", "ab")
	got := redactor.String("status=401; detail=abacus")
	if got != "status=401; detail=abacus" {
		t.Fatalf("short secret should not be replaced: %q", got)
	}
}

func TestRedactorRemovesRecoverableLongSecretFragments(t *testing.T) {
	redactor := NewRedactor("provider-secret-123456789")
	got := redactor.String("diagnostic prefix provider-secret-123456 suffix")
	if strings.Contains(got, "provider-secret") {
		t.Fatalf("secret fragment remained in diagnostic: %q", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("diagnostic was not marked as redacted: %q", got)
	}
}
