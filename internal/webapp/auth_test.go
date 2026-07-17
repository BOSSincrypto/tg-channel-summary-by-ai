package webapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

func TestWebAppAuthValidatesOwnerAndRejectsTampering(t *testing.T) {
	auth, err := NewWebAppAuth("unit-bot-token", "715602446")
	if err != nil {
		t.Fatalf("create auth: %v", err)
	}
	now := time.Unix(1_750_000_000, 0)
	auth.now = func() time.Time { return now }

	initData := signedInitData("unit-bot-token", "715602446", now)
	if got, err := auth.ValidateInitData(initData); err != nil || got != 715602446 {
		t.Fatalf("validate owner initData = %d, %v", got, err)
	}

	tampered := strings.Replace(initData, "715602446", "715602447", 1)
	if _, err := auth.ValidateInitData(tampered); err == nil {
		t.Fatal("tampered initData should be rejected")
	}

	foreign := signedInitData("unit-bot-token", "999999999", now)
	if _, err := auth.ValidateInitData(foreign); err == nil {
		t.Fatal("non-owner initData should be rejected")
	}
}

func TestWebAppAuthRejectsExpiredAndMissingInitData(t *testing.T) {
	auth, err := NewWebAppAuth("unit-bot-token", "715602446")
	if err != nil {
		t.Fatalf("create auth: %v", err)
	}
	now := time.Unix(1_750_000_000, 0)
	auth.now = func() time.Time { return now }

	expired := signedInitData("unit-bot-token", "715602446", now.Add(-25*time.Hour))
	if _, err := auth.ValidateInitData(expired); err == nil {
		t.Fatal("expired initData should be rejected")
	}
	if _, err := auth.ValidateInitData(""); err == nil {
		t.Fatal("missing initData should be rejected")
	}
}

func TestAuthenticatedProviderRoutesMaskSecrets(t *testing.T) {
	store, err := db.OpenWithEncryptionKey(":memory:", "unit-db-key")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	if _, err := store.Providers.Insert(&model.AIProvider{
		Name: "Unit Provider", BaseURL: "https://provider.invalid/v1",
		APIKey: "unit-provider-value", DefaultModel: "unit-model",
	}); err != nil {
		t.Fatalf("insert provider: %v", err)
	}

	auth, err := NewWebAppAuth("unit-bot-token", "715602446")
	if err != nil {
		t.Fatalf("create auth: %v", err)
	}
	server := NewWithProvidersAuthenticated(store, time.Second, http.DefaultClient, auth)

	unauthorized := httptest.NewRequest(http.MethodGet, "/api/providers", nil)
	unauthorizedRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthorizedRecorder, unauthorized)
	if unauthorizedRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want 401", unauthorizedRecorder.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/providers", nil)
	request.Header.Set(initDataHeader, signedInitData("unit-bot-token", "715602446", time.Now()))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "unit-provider-value") {
		t.Fatalf("provider response leaked API key: %s", body)
	}
	if !strings.Contains(body, `"has_key":true`) || strings.Contains(body, `"api_key"`) {
		t.Fatalf("provider response did not use transmission-safe key metadata: %s", body)
	}
}

func TestWebAppAuthRestrictsOriginsAndPreflight(t *testing.T) {
	auth, err := NewWebAppAuthWithOrigin("unit-bot-token", "715602446", "https://settings.example.test/webapp/")
	if err != nil {
		t.Fatalf("create auth: %v", err)
	}
	store, err := db.OpenWithEncryptionKey(":memory:", "unit-db-key")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	server := NewWithProvidersAuthenticated(store, time.Second, http.DefaultClient, auth)

	preflight := httptest.NewRequest(http.MethodOptions, "/api/providers", nil)
	preflight.Header.Set("Origin", "https://settings.example.test")
	preflight.Header.Set("Access-Control-Request-Method", http.MethodPost)
	preflightRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(preflightRecorder, preflight)
	if preflightRecorder.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", preflightRecorder.Code)
	}
	if got := preflightRecorder.Header().Get("Access-Control-Allow-Origin"); got != "https://settings.example.test" {
		t.Fatalf("preflight allow origin = %q", got)
	}
	if got := preflightRecorder.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Fatalf("preflight allow methods = %q, want POST", got)
	}
	if got := preflightRecorder.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, initDataHeader) {
		t.Fatalf("preflight allow headers = %q, want %s", got, initDataHeader)
	}

	foreign := httptest.NewRequest(http.MethodGet, "/api/providers", nil)
	foreign.Header.Set("Origin", "https://evil.example")
	foreignRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(foreignRecorder, foreign)
	if foreignRecorder.Code != http.StatusForbidden {
		t.Fatalf("foreign origin status = %d, want 403", foreignRecorder.Code)
	}
	if got := foreignRecorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("foreign origin unexpectedly allowed: %q", got)
	}
}

func TestAuthenticatedProviderMutationsRequireInitData(t *testing.T) {
	store, err := db.OpenWithEncryptionKey(":memory:", "unit-db-key")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	auth, err := NewWebAppAuthWithOrigin("unit-bot-token", "715602446", "https://settings.example.test/")
	if err != nil {
		t.Fatalf("create auth: %v", err)
	}
	server := NewWithProvidersAuthenticated(store, time.Second, http.DefaultClient, auth)
	request := httptest.NewRequest(http.MethodPost, "/api/providers", strings.NewReader(`{"name":"Provider"}`))
	request.Header.Set("Origin", "https://settings.example.test")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated mutation status = %d, want 401", recorder.Code)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "https://settings.example.test" {
		t.Fatalf("unauthenticated mutation lost CORS header: %q", got)
	}
}

func TestNewWithProvidersFailsClosedWithoutConfiguredAuth(t *testing.T) {
	store, err := db.OpenWithEncryptionKey(":memory:", "unit-db-key")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	server := NewWithProviders(store, time.Second, http.DefaultClient)
	request := httptest.NewRequest(http.MethodPost, "/api/providers", strings.NewReader(`{}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unconfigured auth status = %d, want 401", recorder.Code)
	}
}

func TestAuthenticatedProviderRouteAcceptsTMAAuthorization(t *testing.T) {
	store, err := db.OpenWithEncryptionKey(":memory:", "unit-db-key")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	auth, err := NewWebAppAuth("unit-bot-token", "715602446")
	if err != nil {
		t.Fatalf("create auth: %v", err)
	}
	server := NewWithProvidersAuthenticated(store, time.Second, http.DefaultClient, auth)
	request := httptest.NewRequest(http.MethodGet, "/api/providers", nil)
	request.Header.Set("Authorization", "tma "+signedInitData("unit-bot-token", "715602446", time.Now()))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("TMA authorization status = %d, want 200", recorder.Code)
	}
}

func signedInitData(botToken, ownerID string, timestamp time.Time) string {
	values := url.Values{}
	values.Set("auth_date", strconv.FormatInt(timestamp.Unix(), 10))
	values.Set("query_id", "unit-query")
	values.Set("user", `{"id":`+ownerID+`,"first_name":"Unit"}`)
	dataCheckString := makeDataCheckString(values)

	secretMAC := hmac.New(sha256.New, []byte("WebAppData"))
	_, _ = secretMAC.Write([]byte(botToken))
	signingKey := secretMAC.Sum(nil)
	hashMAC := hmac.New(sha256.New, signingKey)
	_, _ = hashMAC.Write([]byte(dataCheckString))
	values.Set("hash", hex.EncodeToString(hashMAC.Sum(nil)))
	return values.Encode()
}
