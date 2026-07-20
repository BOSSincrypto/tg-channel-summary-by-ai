package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/boss/tg-channel-summary-by-ai/internal/config"
	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/parser"
)

func TestSeedValidatorBotAdminFixtureIsIdempotentAndComplete(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	first, err := seedValidatorBotAdminFixture(store)
	if err != nil {
		t.Fatalf("seed fixture: %v", err)
	}
	second, err := seedValidatorBotAdminFixture(store)
	if err != nil {
		t.Fatalf("reseed fixture: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("fixture seed result changed: first=%+v second=%+v", first, second)
	}

	channels, err := store.Channels.List()
	if err != nil {
		t.Fatalf("list channels: %v", err)
	}
	if len(channels) != 2 {
		t.Fatalf("seeded channels = %d, want valid and duplicate fixtures only", len(channels))
	}
	groups, err := store.Groups.List()
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	if len(groups) < 2 {
		t.Fatalf("seeded groups = %d, want forum and non-forum fixtures", len(groups))
	}
	providers, err := store.Providers.List()
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	if len(providers) < 2 {
		t.Fatalf("seeded providers = %d, want default and custom fixtures", len(providers))
	}
	topics, err := store.ForumTopics.ListOpen(first.ForumGroupID)
	if err != nil {
		t.Fatalf("list seeded topics: %v", err)
	}
	if len(topics) < 2 {
		t.Fatalf("seeded topics = %d, want observed topic catalog", len(topics))
	}
	digests, err := store.Digests.ListByGroup(first.ForumGroupID, 10)
	if err != nil {
		t.Fatalf("list seeded digests: %v", err)
	}
	if len(digests) != 1 {
		t.Fatalf("seeded digests = %d, want one deterministic digest", len(digests))
	}
	settings, err := store.Config.Get("webapp_settings")
	if err != nil {
		t.Fatalf("load seeded WebApp settings: %v", err)
	}
	if settings != `{"digest_time":"10:15","timezone":"UTC","default_model":"validator-model"}` {
		t.Fatalf("seeded WebApp settings = %q", settings)
	}
	var channelRows, groupRows, providerRows, digestRows int
	if err := store.Conn().QueryRow(`SELECT COUNT(*) FROM channels`).Scan(&channelRows); err != nil {
		t.Fatalf("count channels: %v", err)
	}
	if err := store.Conn().QueryRow(`SELECT COUNT(*) FROM groups`).Scan(&groupRows); err != nil {
		t.Fatalf("count groups: %v", err)
	}
	if err := store.Conn().QueryRow(`SELECT COUNT(*) FROM ai_providers`).Scan(&providerRows); err != nil {
		t.Fatalf("count providers: %v", err)
	}
	if err := store.Conn().QueryRow(`SELECT COUNT(*) FROM digests`).Scan(&digestRows); err != nil {
		t.Fatalf("count digests: %v", err)
	}
	if channelRows != len(channels) || groupRows != len(groups) || providerRows != len(providers) || digestRows != len(digests) {
		t.Fatalf("reseed duplicated rows: channels=%d/%d groups=%d/%d providers=%d/%d digests=%d/%d",
			channelRows, len(channels), groupRows, len(groups), providerRows, len(providers), digestRows, len(digests))
	}
}

func TestValidatorBotAdminBoundariesAreLocalAndDeterministic(t *testing.T) {
	verifier := validatorChannelVerifier{}
	tests := []struct {
		name string
		want error
	}{
		{name: validatorFixtureChannelNotFound, want: parser.ErrChannelNotFound},
		{name: validatorFixtureChannelPrivate, want: parser.ErrChannelPrivate},
		{name: validatorFixtureChannelEmpty, want: nil},
		{name: validatorFixtureChannelTransient, want: validatorTransientError},
		{name: validatorFixtureChannelRateLimited, want: validatorRateLimitError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			title, err := verifier.Verify(context.Background(), test.name)
			if test.want == nil {
				if err != nil || title == "" {
					t.Fatalf("Verify() = %q, %v, want deterministic title", title, err)
				}
				return
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("Verify() error = %v, want %v", err, test.want)
			}
		})
	}
	title, err := verifier.Verify(context.Background(), "unlisted_fixture_channel")
	if err != nil || title == "" {
		t.Fatalf("unlisted channel = %q, %v, want local success", title, err)
	}
}

func TestValidatorTransientBoundaryRecoversOnThirdAttempt(t *testing.T) {
	verifier := newValidatorChannelVerifier()
	for attempt := 1; attempt <= 2; attempt++ {
		title, err := verifier.Verify(context.Background(), validatorFixtureChannelTransient)
		if title != "" || !errors.Is(err, validatorTransientError) {
			t.Fatalf("attempt %d = %q, %v, want transient failure", attempt, title, err)
		}
	}
	title, err := verifier.Verify(context.Background(), validatorFixtureChannelTransient)
	if err != nil || title != "Validator Recovered Channel" {
		t.Fatalf("third attempt = %q, %v, want recovery", title, err)
	}
}

func TestValidatorFixtureProfileRequiresHTTPOnlyMode(t *testing.T) {
	t.Setenv("VALIDATOR_FIXTURE", validatorFixtureProfile)
	t.Setenv("VALIDATOR_HTTP_ONLY", "")
	if validatorFixtureEnabled() {
		t.Fatal("fixture profile must be disabled outside validator HTTP mode")
	}
	t.Setenv("VALIDATOR_HTTP_ONLY", "1")
	if !validatorFixtureEnabled() {
		t.Fatal("fixture profile should enable with exact validator HTTP opt-in")
	}
}

func TestValidatorServerWiresFixtureBoundariesWithoutExternalHTTP(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := seedValidatorBotAdminFixture(store); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}

	server, err := newValidatorHTTPServer(&validatorConfigForTest, store)
	if err != nil {
		t.Fatalf("create validator server: %v", err)
	}
	if err := configureValidatorBotAdminFixture(server, store); err != nil {
		t.Fatalf("configure fixture: %v", err)
	}
	testServer := httptest.NewServer(server.Handler())
	t.Cleanup(testServer.Close)

	request, err := http.NewRequest(http.MethodGet, testServer.URL+"/api/channels", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	request.Header.Set("X-Telegram-Init-Data", validatorOwnerInitData())
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatalf("GET channels: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET channels status = %d, want 200", response.StatusCode)
	}
	settingsRequest, err := http.NewRequest(http.MethodGet, testServer.URL+"/api/settings", nil)
	if err != nil {
		t.Fatalf("create settings request: %v", err)
	}
	settingsRequest.Header.Set("X-Telegram-Init-Data", validatorOwnerInitData())
	settingsResponse, err := testServer.Client().Do(settingsRequest)
	if err != nil {
		t.Fatalf("GET settings: %v", err)
	}
	settingsResponse.Body.Close()
	if settingsResponse.StatusCode != http.StatusOK {
		t.Fatalf("GET settings status = %d, want 200", settingsResponse.StatusCode)
	}
	updateRequest, err := http.NewRequest(http.MethodPut, testServer.URL+"/api/settings",
		strings.NewReader(`{"digest_time":"11:30","timezone":"UTC","default_model":"validator-model","version":1}`))
	if err != nil {
		t.Fatalf("create settings update: %v", err)
	}
	updateRequest.Header.Set("Content-Type", "application/json")
	updateRequest.Header.Set("X-Telegram-Init-Data", validatorOwnerInitData())
	updateResponse, err := testServer.Client().Do(updateRequest)
	if err != nil {
		t.Fatalf("PUT settings: %v", err)
	}
	updateResponse.Body.Close()
	if updateResponse.StatusCode != http.StatusOK {
		t.Fatalf("PUT settings status = %d, want 200", updateResponse.StatusCode)
	}
}

var validatorConfigForTest = config.Config{
	BotToken:        "validator:fixture-test",
	OwnerTelegramID: "715602446",
	OpenRouterKey:   "validator-openrouter-key",
	WebAppURL:       "http://localhost:8080/webapp/",
}
