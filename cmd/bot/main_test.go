package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/bot"
	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/digest"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/scheduler"
	"github.com/boss/tg-channel-summary-by-ai/internal/summarizer"
	"github.com/boss/tg-channel-summary-by-ai/internal/webapp"
)

func TestEnsureDefaultAIProviderSeedsOpenRouter(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	if err := ensureDefaultAIProvider(store, "startup-key"); err != nil {
		t.Fatalf("ensure default provider: %v", err)
	}
	provider, err := store.Providers.GetDefault()
	if err != nil {
		t.Fatalf("get default provider: %v", err)
	}
	if provider.Name != "OpenRouter" || provider.BaseURL != summarizer.DefaultOpenRouterBaseURL {
		t.Fatalf("default provider = %#v, want OpenRouter", provider)
	}
	if provider.APIKey != "startup-key" {
		t.Fatalf("default provider API key = %q, want startup key", provider.APIKey)
	}
	if provider.DefaultModel != summarizer.DefaultOpenRouterModel {
		t.Fatalf("default model = %q, want %q", provider.DefaultModel, summarizer.DefaultOpenRouterModel)
	}
}

func TestEnsureDefaultAIProviderDoesNotReplaceCustomDefault(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	if _, err := store.Providers.Insert(&model.AIProvider{
		Name:         "Custom",
		BaseURL:      "https://custom.example/v1",
		APIKey:       "custom-key",
		DefaultModel: "custom-model",
		IsDefault:    true,
	}); err != nil {
		t.Fatalf("insert custom provider: %v", err)
	}

	if err := ensureDefaultAIProvider(store, "startup-key"); err != nil {
		t.Fatalf("ensure default provider: %v", err)
	}
	provider, err := store.Providers.GetDefault()
	if err != nil {
		t.Fatalf("get default provider: %v", err)
	}
	if provider.Name != "Custom" || provider.APIKey != "custom-key" || provider.DefaultModel != "custom-model" {
		t.Fatalf("custom default was replaced: %#v", provider)
	}
}

func TestProductionSettingsBoundaryPersistsAndRefreshesForTelegramAndHTTP(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -1001,
		Title:          "Production group",
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := store.Groups.UpdateGroupSettings(&model.GroupSettings{
		GroupID:    groupID,
		DigestTime: "21:00",
		Timezone:   "UTC",
	}); err != nil {
		t.Fatalf("update group settings: %v", err)
	}

	runner := &testDigestRunner{}
	sched := scheduler.New(runner, scheduler.WithGroupSource(store.Groups))
	if err := sched.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer sched.Stop()

	settings := bot.BotSettings{DigestTime: "09:30", Channels: []string{"@news"}}
	if err := applyProductionSettings(context.Background(), store, sched, settings); err != nil {
		t.Fatalf("apply production settings: %v", err)
	}

	groupSettings, err := store.Groups.GetGroupSettings(groupID)
	if err != nil {
		t.Fatalf("load group settings: %v", err)
	}
	if groupSettings.DigestTime != "09:30" || groupSettings.Timezone != "UTC" {
		t.Fatalf("group settings = %+v, want digest time 09:30 and preserved UTC", groupSettings)
	}
	value, err := store.Config.Get("webapp_settings")
	if err != nil {
		t.Fatalf("load persisted WebApp settings: %v", err)
	}
	if value == "" || !containsJSONValue(value, "09:30") || !containsJSONValue(value, "@news") {
		t.Fatalf("persisted WebApp settings = %q", value)
	}
	if got, ok := sched.ScheduleForGroup(groupID); !ok || got != "CRON_TZ=UTC 30 9 * * *" {
		t.Fatalf("scheduler schedule = %q, registered=%v", got, ok)
	}

	auth, err := webapp.NewWebAppAuth("unit-bot-token", "715602446")
	if err != nil {
		t.Fatalf("create WebApp auth: %v", err)
	}
	server := webapp.NewWithProvidersAuthenticated(store, time.Second, http.DefaultClient, auth)
	server.SetSettingsApplier(func(ctx context.Context, mutation webapp.SettingsMutation) (int64, error) {
		return applyProductionSettingsMutation(ctx, store, sched, mutation)
	})

	get := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	get.Header.Set("Authorization", "tma "+signedInitDataForProductionTest("unit-bot-token", "715602446", time.Now()))
	getRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(getRecorder, get)
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("authenticated settings GET status = %d, body=%s", getRecorder.Code, getRecorder.Body.String())
	}
	var current struct {
		Version int64 `json:"version"`
	}
	if err := json.Unmarshal(getRecorder.Body.Bytes(), &current); err != nil {
		t.Fatalf("decode current settings: %v", err)
	}
	update := fmt.Sprintf(`{"digest_time":"10:45","timezone":"Asia/Tokyo","default_model":"custom-model","version":%d}`, current.Version)
	put := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(update))
	put.Header.Set("Content-Type", "application/json")
	put.Header.Set("Authorization", "tma "+signedInitDataForProductionTest("unit-bot-token", "715602446", time.Now()))
	putRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(putRecorder, put)
	if putRecorder.Code != http.StatusOK {
		t.Fatalf("authenticated settings PUT status = %d, body=%s", putRecorder.Code, putRecorder.Body.String())
	}

	groupSettings, err = store.Groups.GetGroupSettings(groupID)
	if err != nil {
		t.Fatalf("load HTTP-updated group settings: %v", err)
	}
	if groupSettings.DigestTime != "10:45" || groupSettings.Timezone != "Asia/Tokyo" {
		t.Fatalf("HTTP-updated group settings = %+v", groupSettings)
	}
	value, err = store.Config.Get("webapp_settings")
	if err != nil {
		t.Fatalf("load HTTP-updated WebApp settings: %v", err)
	}
	if !containsJSONValue(value, "custom-model") || !containsJSONValue(value, "@news") {
		t.Fatalf("HTTP-updated persisted settings = %q", value)
	}
	if got, ok := sched.ScheduleForGroup(groupID); !ok || got != "CRON_TZ=Asia/Tokyo 45 10 * * *" {
		t.Fatalf("HTTP-updated scheduler schedule = %q, registered=%v", got, ok)
	}
}

type testDigestRunner struct{}

func (*testDigestRunner) Generate(int64) (*digest.Digest, error) {
	return &digest.Digest{}, nil
}

func containsJSONValue(value, expected string) bool {
	return strings.Contains(value, expected)
}

func signedInitDataForProductionTest(botToken, ownerID string, timestamp time.Time) string {
	values := url.Values{}
	values.Set("auth_date", strconv.FormatInt(timestamp.Unix(), 10))
	values.Set("query_id", "production-boundary")
	values.Set("user", `{"id":`+ownerID+`,"first_name":"Production"}`)
	dataCheckString := strings.Join([]string{
		"auth_date=" + values.Get("auth_date"),
		"query_id=" + values.Get("query_id"),
		"user=" + values.Get("user"),
	}, "\n")
	secretMAC := hmac.New(sha256.New, []byte("WebAppData"))
	_, _ = secretMAC.Write([]byte(botToken))
	hashMAC := hmac.New(sha256.New, secretMAC.Sum(nil))
	_, _ = hashMAC.Write([]byte(dataCheckString))
	values.Set("hash", hex.EncodeToString(hashMAC.Sum(nil)))
	return values.Encode()
}
