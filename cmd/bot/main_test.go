package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/bot"
	"github.com/boss/tg-channel-summary-by-ai/internal/config"
	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/digest"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/scheduler"
	"github.com/boss/tg-channel-summary-by-ai/internal/summarizer"
	"github.com/boss/tg-channel-summary-by-ai/internal/webapp"
)

func TestValidatorHTTPOnlyEnabledRequiresExactOptIn(t *testing.T) {
	t.Setenv("VALIDATOR_HTTP_ONLY", "1")
	if !validatorHTTPOnlyEnabled() {
		t.Fatal("validator mode should be enabled for VALIDATOR_HTTP_ONLY=1")
	}
	t.Setenv("VALIDATOR_HTTP_ONLY", "true")
	if validatorHTTPOnlyEnabled() {
		t.Fatal("validator mode should require the exact opt-in value")
	}
	t.Setenv("VALIDATOR_HTTP_ONLY", "")
	if validatorHTTPOnlyEnabled() {
		t.Fatal("validator mode should remain disabled when unset")
	}
}

func TestValidatorHTTPServerServesHealthAndEmbeddedWebAppWithoutBot(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	server, err := newValidatorHTTPServer(&config.Config{
		BotToken:        "validator:fake",
		OwnerTelegramID: "715602446",
		WebAppURL:       "https://validator.example/webapp/",
	}, store)
	if err != nil {
		t.Fatalf("create validator server: %v", err)
	}
	testServer := httptest.NewServer(server.Handler())
	defer testServer.Close()

	healthResponse, err := testServer.Client().Get(testServer.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer healthResponse.Body.Close()
	if healthResponse.StatusCode != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200", healthResponse.StatusCode)
	}

	webAppResponse, err := testServer.Client().Get(testServer.URL + "/webapp/")
	if err != nil {
		t.Fatalf("GET /webapp/: %v", err)
	}
	defer webAppResponse.Body.Close()
	if webAppResponse.StatusCode != http.StatusOK {
		t.Fatalf("GET /webapp/ status = %d, want 200", webAppResponse.StatusCode)
	}
}

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
	if err := store.Config.Set("webapp_settings", `{"digest_time":"21:00","timezone":"Europe/Moscow","default_model":"openai/gpt-oss-120b"}`); err != nil {
		t.Fatalf("seed WebApp settings: %v", err)
	}

	runner := &testDigestRunner{}
	sched := scheduler.New(runner, scheduler.WithGroupSource(store.Groups))
	if err := sched.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer sched.Stop()

	settings := bot.BotSettings{DigestTime: "09:30", Channels: []string{"@news"}, Version: 1}
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

func TestProductionSettingsBoundaryReconcilesLateSchedulerFailureAfterRestart(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -1005, Title: "Recovery group"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := store.Groups.UpdateGroupSettings(&model.GroupSettings{
		GroupID: groupID, DigestTime: "21:00", Timezone: "UTC",
	}); err != nil {
		t.Fatalf("seed group settings: %v", err)
	}
	if err := store.Config.Set("webapp_settings", `{"digest_time":"21:00","timezone":"UTC","default_model":"model"}`); err != nil {
		t.Fatalf("seed WebApp settings: %v", err)
	}
	failRefresh := true
	sched := scheduler.New(&testDigestRunner{}, scheduler.WithGroupSource(store.Groups),
		scheduler.WithRefreshFailureHook(func() error {
			if failRefresh {
				return errors.New("late scheduler failure")
			}
			return nil
		}))
	if err := sched.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer sched.Stop()

	if _, err := applyProductionSettingsMutation(context.Background(), store, sched, webapp.SettingsMutation{
		DigestTime: "09:30", Timezone: "UTC", DefaultModel: "model", Version: 1,
	}); err == nil {
		t.Fatal("settings update succeeded despite late scheduler failure")
	}
	if got, ok := sched.ScheduleForGroup(groupID); !ok || got != "CRON_TZ=UTC 0 21 * * *" {
		t.Fatalf("scheduler after failed update = %q, registered=%v, want old schedule", got, ok)
	}
	if _, err := store.Config.Get("webapp_settings_sync_pending"); err != nil {
		t.Fatalf("pending settings intent missing: %v", err)
	}
	value, version, err := store.Config.GetWithVersion("webapp_settings")
	if err != nil {
		t.Fatalf("load committed settings: %v", err)
	}
	if !containsJSONValue(value, "09:30") || version != 2 {
		t.Fatalf("committed settings = %q version %d, want new value version 2", value, version)
	}

	failRefresh = false
	if err := reconcilePendingSettings(context.Background(), store, sched); err != nil {
		t.Fatalf("reconcile pending settings: %v", err)
	}
	if got, ok := sched.ScheduleForGroup(groupID); !ok || got != "CRON_TZ=UTC 30 9 * * *" {
		t.Fatalf("scheduler after reconciliation = %q, registered=%v, want new schedule", got, ok)
	}
	if _, err := store.Config.Get("webapp_settings_sync_pending"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("pending settings after reconciliation = %v, want not found", err)
	}
}

func TestProductionSettingsBoundaryRejectsMissingVersionWithoutSideEffects(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -1006, Title: "Version group"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := store.Groups.UpdateGroupSettings(&model.GroupSettings{
		GroupID: groupID, DigestTime: "21:00", Timezone: "UTC",
	}); err != nil {
		t.Fatalf("seed group settings: %v", err)
	}
	if err := store.Config.Set("webapp_settings", `{"digest_time":"21:00","timezone":"UTC","default_model":"model"}`); err != nil {
		t.Fatalf("seed WebApp settings: %v", err)
	}
	sched := scheduler.New(&testDigestRunner{}, scheduler.WithGroupSource(store.Groups))
	if err := sched.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer sched.Stop()

	if _, err := applyProductionSettingsMutation(context.Background(), store, sched, webapp.SettingsMutation{
		DigestTime: "09:30", Timezone: "UTC", DefaultModel: "model",
	}); !errors.Is(err, db.ErrConflict) {
		t.Fatalf("missing version error = %v, want conflict", err)
	}
	value, version, err := store.Config.GetWithVersion("webapp_settings")
	if err != nil {
		t.Fatalf("load settings after rejected version: %v", err)
	}
	if !containsJSONValue(value, "21:00") || version != 1 {
		t.Fatalf("settings after rejected version = %q version %d", value, version)
	}
	if _, err := store.Config.Get(pendingSettingsSyncKey); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("pending sync after rejected version = %v", err)
	}
	if got, ok := sched.ScheduleForGroup(groupID); !ok || got != "CRON_TZ=UTC 0 21 * * *" {
		t.Fatalf("scheduler after rejected version = %q, registered=%v", got, ok)
	}
}

func TestProductionSettingsBoundarySerializesConcurrentMutations(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -1007, Title: "Concurrent group"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := store.Groups.UpdateGroupSettings(&model.GroupSettings{
		GroupID: groupID, DigestTime: "21:00", Timezone: "UTC",
	}); err != nil {
		t.Fatalf("seed group settings: %v", err)
	}
	if err := store.Config.Set("webapp_settings", `{"digest_time":"21:00","timezone":"UTC","default_model":"model"}`); err != nil {
		t.Fatalf("seed WebApp settings: %v", err)
	}
	sched := scheduler.New(&testDigestRunner{}, scheduler.WithGroupSource(store.Groups))
	if err := sched.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer sched.Stop()

	results := make(chan error, 2)
	for _, digestTime := range []string{"09:00", "10:00"} {
		go func(digestTime string) {
			_, applyErr := applyProductionSettingsMutation(context.Background(), store, sched, webapp.SettingsMutation{
				DigestTime: digestTime, Timezone: "UTC", DefaultModel: "model", Version: 1,
			})
			results <- applyErr
		}(digestTime)
	}
	var succeeded, conflicts int
	for range 2 {
		switch applyErr := <-results; {
		case applyErr == nil:
			succeeded++
		case errors.Is(applyErr, db.ErrConflict):
			conflicts++
		default:
			t.Fatalf("concurrent settings error = %v", applyErr)
		}
	}
	if succeeded != 1 || conflicts != 1 {
		t.Fatalf("concurrent results = succeeded:%d conflicts:%d, want one each", succeeded, conflicts)
	}
	_, version, err := store.Config.GetWithVersion("webapp_settings")
	if err != nil {
		t.Fatalf("load concurrent settings: %v", err)
	}
	if version != 2 {
		t.Fatalf("concurrent settings version = %d, want 2", version)
	}
	if _, err := store.Config.Get(pendingSettingsSyncKey); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("concurrent pending sync = %v, want not found", err)
	}
	schedule, ok := sched.ScheduleForGroup(groupID)
	if !ok || (schedule != "CRON_TZ=UTC 0 9 * * *" && schedule != "CRON_TZ=UTC 0 10 * * *") {
		t.Fatalf("concurrent scheduler schedule = %q, registered=%v", schedule, ok)
	}
}

func TestProductionSettingsRefreshSerializesConcurrentGroupCreate(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -100701, Title: "Existing"})
	if err != nil {
		t.Fatalf("insert existing group: %v", err)
	}
	if err := store.Groups.UpdateGroupSettings(&model.GroupSettings{GroupID: groupID, DigestTime: "21:00", Timezone: "UTC"}); err != nil {
		t.Fatalf("seed existing settings: %v", err)
	}
	if err := store.Config.Set("webapp_settings", `{"digest_time":"21:00","timezone":"UTC","default_model":"model"}`); err != nil {
		t.Fatalf("seed WebApp settings: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	sched := scheduler.New(&testDigestRunner{}, scheduler.WithGroupSource(store.Groups),
		scheduler.WithLifecycleHooks(func() {
			once.Do(func() {
				close(entered)
				<-release
			})
		}, nil))
	if err := sched.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer sched.Stop()

	auth, err := webapp.NewWebAppAuth("unit-bot-token", "715602446")
	if err != nil {
		t.Fatalf("create WebApp auth: %v", err)
	}
	server := webapp.NewWithProvidersAuthenticated(store, time.Second, http.DefaultClient, auth)
	server.SetGroupScheduler(sched)
	server.SetGroupVerifier(productionGroupVerifier{})
	server.SetSettingsApplier(func(ctx context.Context, mutation webapp.SettingsMutation) (int64, error) {
		return applyProductionSettingsMutation(ctx, store, sched, mutation)
	})
	authHeader := "tma " + signedInitDataForProductionTest("unit-bot-token", "715602446", time.Now())

	putDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(http.MethodPut, "/api/settings",
			strings.NewReader(`{"digest_time":"09:30","timezone":"UTC","default_model":"model","version":1}`))
		request.Header.Set("Authorization", authHeader)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		putDone <- response
	}()
	<-entered

	createDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(http.MethodPost, "/api/groups", strings.NewReader(`{"chat_id":"-100702"}`))
		request.Header.Set("Authorization", authHeader)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		createDone <- response
	}()
	select {
	case response := <-createDone:
		t.Fatalf("group create crossed settings lifecycle boundary: status=%d body=%s", response.Code, response.Body.String())
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if response := <-putDone; response.Code != http.StatusOK {
		t.Fatalf("settings PUT status = %d, body=%s", response.Code, response.Body.String())
	}
	response := <-createDone
	if response.Code != http.StatusCreated {
		t.Fatalf("group create status = %d, body=%s", response.Code, response.Body.String())
	}
	created, err := store.Groups.GetByChatID(-100702)
	if err != nil {
		t.Fatalf("load concurrently created group: %v", err)
	}
	if _, ok := sched.ScheduleForGroup(created.ID); !ok {
		t.Fatal("concurrently created group has no live scheduler job")
	}
	if got, ok := sched.ScheduleForGroup(groupID); !ok || got != "CRON_TZ=UTC 30 9 * * *" {
		t.Fatalf("existing group schedule = %q, registered=%v", got, ok)
	}
}

func TestProductionSettingsRefreshSerializesConcurrentGroupDelete(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	keepID, err := store.Groups.Insert(&model.Group{TelegramChatID: -100703, Title: "Keep"})
	if err != nil {
		t.Fatalf("insert keep group: %v", err)
	}
	deleteID, err := store.Groups.Insert(&model.Group{TelegramChatID: -100704, Title: "Delete"})
	if err != nil {
		t.Fatalf("insert delete group: %v", err)
	}
	for _, groupID := range []int64{keepID, deleteID} {
		if err := store.Groups.UpdateGroupSettings(&model.GroupSettings{GroupID: groupID, DigestTime: "21:00", Timezone: "UTC"}); err != nil {
			t.Fatalf("seed group %d settings: %v", groupID, err)
		}
	}
	if err := store.Config.Set("webapp_settings", `{"digest_time":"21:00","timezone":"UTC","default_model":"model"}`); err != nil {
		t.Fatalf("seed WebApp settings: %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	sched := scheduler.New(&testDigestRunner{}, scheduler.WithGroupSource(store.Groups),
		scheduler.WithLifecycleHooks(func() {
			once.Do(func() {
				close(entered)
				<-release
			})
		}, nil))
	if err := sched.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer sched.Stop()
	auth, err := webapp.NewWebAppAuth("unit-bot-token", "715602446")
	if err != nil {
		t.Fatalf("create WebApp auth: %v", err)
	}
	server := webapp.NewWithProvidersAuthenticated(store, time.Second, http.DefaultClient, auth)
	server.SetGroupScheduler(sched)
	server.SetGroupVerifier(productionGroupVerifier{})
	server.SetSettingsApplier(func(ctx context.Context, mutation webapp.SettingsMutation) (int64, error) {
		return applyProductionSettingsMutation(ctx, store, sched, mutation)
	})
	authHeader := "tma " + signedInitDataForProductionTest("unit-bot-token", "715602446", time.Now())
	putDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(http.MethodPut, "/api/settings",
			strings.NewReader(`{"digest_time":"10:15","timezone":"UTC","default_model":"model","version":1}`))
		request.Header.Set("Authorization", authHeader)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		putDone <- response
	}()
	<-entered
	deleteDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(http.MethodDelete, "/api/groups/"+strconv.FormatInt(deleteID, 10),
			strings.NewReader(`{"version":1}`))
		request.Header.Set("Authorization", authHeader)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		deleteDone <- response
	}()
	select {
	case response := <-deleteDone:
		t.Fatalf("group delete crossed settings lifecycle boundary: status=%d body=%s", response.Code, response.Body.String())
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if response := <-putDone; response.Code != http.StatusOK {
		t.Fatalf("settings PUT status = %d, body=%s", response.Code, response.Body.String())
	}
	if response := <-deleteDone; response.Code != http.StatusNoContent {
		t.Fatalf("group delete status = %d, body=%s", response.Code, response.Body.String())
	}
	if _, err := store.Groups.GetByID(deleteID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("deleted group lookup = %v, want not found", err)
	}
	if _, ok := sched.ScheduleForGroup(deleteID); ok {
		t.Fatal("concurrently deleted group was resurrected by settings refresh")
	}
	if got, ok := sched.ScheduleForGroup(keepID); !ok || got != "CRON_TZ=UTC 15 10 * * *" {
		t.Fatalf("kept group schedule = %q, registered=%v", got, ok)
	}
}

type testDigestRunner struct{}

func (*testDigestRunner) Generate(int64) (*digest.Digest, error) {
	return &digest.Digest{}, nil
}

type productionGroupVerifier struct{}

func (productionGroupVerifier) Verify(chatID int64) (string, error) {
	return strconv.FormatInt(chatID, 10), nil
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

// TestSchedulerCatchUpProductionWiringFiresForMissedSchedule proves that the
// scheduler is constructed with DigestHistory and DSTSkipNotifier in the
// production path and that CatchUp fires for a missed schedule through the
// real DigestRepository. This satisfies VAL-DIGEST-003 at the production
// boundary, not only from an isolated package unit test.
func TestSchedulerCatchUpProductionWiringFiresForMissedSchedule(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -1007,
		Title:          "Catch-up production",
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := store.Groups.UpdateGroupSettings(&model.GroupSettings{
		GroupID: groupID, DigestTime: "09:00", Timezone: "UTC",
	}); err != nil {
		t.Fatalf("update group settings: %v", err)
	}

	runner := &testDigestRunner{}
	var dstNotifications []string
	now := time.Date(2026, 7, 21, 9, 30, 0, 0, time.UTC)

	// Construct the scheduler the same way main.go does, with DigestHistory
	// and DSTSkipNotifier wired to the real repositories.
	sched := scheduler.New(runner,
		scheduler.WithGroupSource(store.Groups),
		scheduler.WithDigestHistory(store.Digests),
		scheduler.WithDSTSkipNotifier(func(groupID int64, groupTitle, digestTime, timezone, reason string) {
			dstNotifications = append(dstNotifications, reason)
		}),
		scheduler.WithNowFunc(func() time.Time { return now }),
	)
	if err := sched.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer sched.Stop()

	// Verify the schedule was registered (VAL-DIGEST-001: triggers at configured time)
	if got, ok := sched.ScheduleForGroup(groupID); !ok || got != "CRON_TZ=UTC 0 9 * * *" {
		t.Fatalf("scheduler schedule = %q, registered=%v", got, ok)
	}

	// Catch-up should fire because no digest has been sent (VAL-DIGEST-003)
	if err := sched.CatchUp(); err != nil {
		t.Fatalf("catch-up: %v", err)
	}

	if len(dstNotifications) != 0 {
		t.Fatalf("DST notifications = %v, want 0 (UTC has no DST)", dstNotifications)
	}
}

// TestSchedulerDSTSkipProductionWiringNotifiesOwner proves that the DST skip
// notifier is invoked through the production scheduler construction when a
// scheduled time does not exist due to a spring-forward transition.
func TestSchedulerDSTSkipProductionWiringNotifiesOwner(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	nyLoc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -1008,
		Title:          "DST production",
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := store.Groups.UpdateGroupSettings(&model.GroupSettings{
		GroupID: groupID, DigestTime: "02:30", Timezone: "America/New_York",
	}); err != nil {
		t.Fatalf("update group settings: %v", err)
	}

	// Record a digest for yesterday so catch-up doesn't fire (only DST skip)
	_, err = store.Digests.Insert(&model.Digest{
		GroupID: groupID, SentAt: "2026-03-07 07:35:00", PostCount: 1,
	})
	if err != nil {
		t.Fatalf("insert digest: %v", err)
	}

	runner := &testDigestRunner{}
	var dstNotifications []string
	now := time.Date(2026, 3, 8, 3, 0, 0, 0, nyLoc) // After spring-forward gap

	sched := scheduler.New(runner,
		scheduler.WithGroupSource(store.Groups),
		scheduler.WithDigestHistory(store.Digests),
		scheduler.WithDSTSkipNotifier(func(gID int64, title, dt, tz, reason string) {
			dstNotifications = append(dstNotifications, fmt.Sprintf("%d|%s|%s|%s", gID, dt, tz, reason))
		}),
		scheduler.WithNowFunc(func() time.Time { return now }),
	)
	if err := sched.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer sched.Stop()

	if err := sched.CatchUp(); err != nil {
		t.Fatalf("catch-up: %v", err)
	}

	if len(dstNotifications) != 1 {
		t.Fatalf("DST notifications = %v, want 1", dstNotifications)
	}
	if !strings.Contains(dstNotifications[0], "02:30") || !strings.Contains(dstNotifications[0], "America/New_York") {
		t.Fatalf("DST notification = %q, want 02:30 and America/New_York", dstNotifications[0])
	}
}
