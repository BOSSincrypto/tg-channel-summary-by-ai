package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/security"
)

const telegramAPIBaseURL = "https://api.telegram.org"

// OwnerNotifier sends direct owner notifications via the Telegram Bot API.
type OwnerNotifier struct {
	baseURL         string
	botToken        string
	ownerID         string
	httpClient      *http.Client
	providerSecrets []string
	secretSource    func() []string
}

// NewOwnerNotifier creates a notifier that delivers maintenance alerts to the configured owner.
func NewOwnerNotifier(botToken, ownerID string, providerSecrets ...string) *OwnerNotifier {
	return &OwnerNotifier{
		baseURL:         telegramAPIBaseURL,
		botToken:        botToken,
		ownerID:         ownerID,
		providerSecrets: providerSecrets,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// SetProviderSecretSource supplies current provider credentials for each
// notification. This keeps notifications safe when providers are changed in
// the WebApp after startup.
func (n *OwnerNotifier) SetProviderSecretSource(source func() []string) {
	if n == nil {
		return
	}
	n.secretSource = source
}

// NotifyOwner sends a plain-text Telegram message to the owner.
func (n *OwnerNotifier) NotifyOwner(ctx context.Context, text string) error {
	if n.botToken == "" {
		return fmt.Errorf("bot token is required")
	}
	if n.ownerID == "" {
		return fmt.Errorf("owner telegram id is required")
	}
	text = n.redact(text)

	payload := map[string]any{
		"chat_id": n.ownerID,
		"text":    text,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal owner notification payload: %w", err)
	}

	url := fmt.Sprintf("%s/bot%s/sendMessage", n.baseURL, n.botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build owner notification request: %w", n.redactError(err))
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send owner notification request: %w", n.redactError(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("send owner notification failed with status %s", resp.Status)
	}

	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode owner notification response: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("telegram sendMessage returned ok=false")
	}

	return nil
}

func (n *OwnerNotifier) redact(text string) string {
	if n == nil {
		return text
	}
	secrets := append([]string(nil), n.providerSecrets...)
	if n.secretSource != nil {
		secrets = append(secrets, n.secretSource()...)
	}
	secrets = append(secrets, n.botToken)
	return security.NewRedactor(secrets...).String(text)
}

func (n *OwnerNotifier) redactError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(n.redact(err.Error()))
}
