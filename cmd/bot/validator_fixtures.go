package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/digest"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/parser"
	"github.com/boss/tg-channel-summary-by-ai/internal/summarizer"
	"github.com/boss/tg-channel-summary-by-ai/internal/webapp"
)

const validatorFixtureProfile = "bot-admin-r2"

const (
	validatorFixtureChannelValid       = "fixture_valid"
	validatorFixtureChannelDuplicate   = "fixture_duplicate"
	validatorFixtureChannelNotFound    = "fixture_missing"
	validatorFixtureChannelPrivate     = "fixture_private"
	validatorFixtureChannelEmpty       = "fixture_empty"
	validatorFixtureChannelTransient   = "fixture_retry"
	validatorFixtureChannelRateLimited = "fixture_rate"

	validatorFixtureForumChatID   int64 = -1007000000001
	validatorFixtureRegularChatID int64 = -1007000000002
)

var (
	validatorTransientError = errors.New("validator fixture transient channel failure")
	validatorRateLimitError = errors.New("validator fixture channel rate limit")
	validatorGroupError     = errors.New("validator fixture group unavailable")
)

type validatorFixtureSeed struct {
	ForumGroupID   int64
	RegularGroupID int64
	ValidChannelID int64
	ForumTopicIDs  []int64
	ProviderIDs    []int64
	DigestID       int64
}

// seedValidatorBotAdminFixture creates the complete local WebApp profile.
// Every lookup is keyed by a stable business identifier so restarting against
// the same temporary DB is safe and does not create duplicate rows.
func seedValidatorBotAdminFixture(store *db.DB) (validatorFixtureSeed, error) {
	if store == nil {
		return validatorFixtureSeed{}, errors.New("validator fixture requires a database")
	}
	var result validatorFixtureSeed

	channelIDs := make(map[string]int64)
	for _, fixture := range []struct {
		username string
		title    string
	}{
		{validatorFixtureChannelValid, "Validator Valid Channel"},
		{validatorFixtureChannelDuplicate, "Validator Duplicate Channel"},
	} {
		channel, err := store.Channels.GetByUsername(fixture.username)
		if errors.Is(err, db.ErrNotFound) {
			id, insertErr := store.Channels.Insert(&model.Channel{
				Username: fixture.username,
				Title:    fixture.title,
				Enabled:  true,
			})
			if insertErr != nil {
				return validatorFixtureSeed{}, fmt.Errorf("seed channel %s: %w", fixture.username, insertErr)
			}
			channel, err = store.Channels.GetByID(id)
		}
		if err != nil {
			return validatorFixtureSeed{}, fmt.Errorf("load channel %s: %w", fixture.username, err)
		}
		channelIDs[fixture.username] = channel.ID
	}
	result.ValidChannelID = channelIDs[validatorFixtureChannelValid]

	providerIDs, err := seedValidatorProviders(store)
	if err != nil {
		return validatorFixtureSeed{}, err
	}
	result.ProviderIDs = providerIDs

	result.ForumGroupID, err = seedValidatorGroup(store, validatorFixtureForumChatID, "Validator Forum", model.GroupStatusActive)
	if err != nil {
		return validatorFixtureSeed{}, err
	}
	result.RegularGroupID, err = seedValidatorGroup(store, validatorFixtureRegularChatID, "Validator Regular Group", model.GroupStatusIneligible)
	if err != nil {
		return validatorFixtureSeed{}, err
	}

	if err := store.Groups.UpdateGroupSettings(&model.GroupSettings{
		GroupID:    result.ForumGroupID,
		ProviderID: &providerIDs[1],
		Model:      stringPointer("validator-model"),
		DigestTime: "10:15",
		Timezone:   "UTC",
	}); err != nil {
		return validatorFixtureSeed{}, fmt.Errorf("seed forum settings: %w", err)
	}
	if err := store.Groups.UpdateGroupSettings(&model.GroupSettings{
		GroupID:    result.RegularGroupID,
		DigestTime: "11:20",
		Timezone:   "Europe/Moscow",
	}); err != nil {
		return validatorFixtureSeed{}, fmt.Errorf("seed regular settings: %w", err)
	}
	if err := ensureValidatorSettings(store); err != nil {
		return validatorFixtureSeed{}, err
	}

	for _, topic := range []struct {
		id   int64
		name string
	}{
		{101, "Announcements"},
		{102, "Daily digest"},
	} {
		if err := store.ForumTopics.Observe(result.ForumGroupID, topic.id, topic.name); err != nil {
			return validatorFixtureSeed{}, fmt.Errorf("seed observed topic %d: %w", topic.id, err)
		}
		result.ForumTopicIDs = append(result.ForumTopicIDs, topic.id)
	}
	if err := store.Groups.AssignChannel(result.ForumGroupID, result.ValidChannelID, &result.ForumTopicIDs[0]); err != nil {
		if !errors.Is(err, db.ErrDuplicate) {
			return validatorFixtureSeed{}, fmt.Errorf("seed channel assignment: %w", err)
		}
	}

	result.DigestID, err = seedValidatorDigest(store, result.ForumGroupID, result.ValidChannelID)
	if err != nil {
		return validatorFixtureSeed{}, err
	}
	return result, nil
}

func seedValidatorProviders(store *db.DB) ([]int64, error) {
	providers := []struct {
		name, baseURL, key, model string
		defaulted                 bool
	}{
		{"OpenRouter", summarizer.DefaultOpenRouterBaseURL, "validator-openrouter-key", summarizer.DefaultOpenRouterModel, true},
		{"Validator Local", "https://validator.local/v1", "validator-provider-key", "validator-model", false},
	}
	ids := make([]int64, 0, len(providers))
	for _, fixture := range providers {
		provider, err := store.Providers.GetByName(fixture.name)
		if errors.Is(err, db.ErrNotFound) {
			id, insertErr := store.Providers.Insert(&model.AIProvider{
				Name:         fixture.name,
				BaseURL:      fixture.baseURL,
				APIKey:       fixture.key,
				DefaultModel: fixture.model,
				IsDefault:    fixture.defaulted,
			})
			if insertErr != nil {
				return nil, fmt.Errorf("seed provider %s: %w", fixture.name, insertErr)
			}
			provider, err = store.Providers.GetByID(id)
		}
		if err != nil {
			return nil, fmt.Errorf("load provider %s: %w", fixture.name, err)
		}
		ids = append(ids, provider.ID)
	}
	return ids, nil
}

func seedValidatorGroup(store *db.DB, chatID int64, title, status string) (int64, error) {
	group, err := store.Groups.GetByChatID(chatID)
	if errors.Is(err, db.ErrNotFound) {
		id, insertErr := store.Groups.Insert(&model.Group{
			TelegramChatID: chatID,
			Title:          title,
			Status:         status,
		})
		if insertErr != nil {
			return 0, fmt.Errorf("seed group %d: %w", chatID, insertErr)
		}
		return id, nil
	}
	if err != nil {
		return 0, fmt.Errorf("load group %d: %w", chatID, err)
	}
	return group.ID, nil
}

func seedValidatorDigest(store *db.DB, groupID, channelID int64) (int64, error) {
	digests, err := store.Digests.ListByGroup(groupID, 10)
	if err != nil {
		return 0, fmt.Errorf("list validator digests: %w", err)
	}
	if len(digests) > 0 {
		return digests[0].ID, nil
	}
	summary := "Тестовый дайджест для локальной проверки WebApp."
	postID, err := store.Posts.Insert(&model.Post{
		ChannelID:   channelID,
		MessageID:   1,
		Text:        "Тестовая публикация validator.",
		Summary:     &summary,
		PostedAt:    time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
		URL:         "https://t.me/fixture_valid/1",
		ContentHash: "validator-fixture-post",
	})
	if err != nil && !errors.Is(err, db.ErrDuplicate) {
		return 0, fmt.Errorf("seed digest post: %w", err)
	}
	if errors.Is(err, db.ErrDuplicate) {
		post, getErr := store.Posts.GetByChannelAndMessageID(channelID, 1)
		if getErr != nil {
			return 0, fmt.Errorf("load digest post: %w", getErr)
		}
		postID = post.ID
	}
	digestID, err := store.Digests.Insert(&model.Digest{
		GroupID:   groupID,
		PostCount: 1,
	})
	if err != nil {
		return 0, fmt.Errorf("seed digest: %w", err)
	}
	if err := store.Digests.AddPost(digestID, postID); err != nil {
		return 0, fmt.Errorf("link digest post: %w", err)
	}
	return digestID, nil
}

func ensureValidatorSettings(store *db.DB) error {
	if _, err := store.Config.Get("webapp_settings"); errors.Is(err, db.ErrNotFound) {
		value := `{"digest_time":"10:15","timezone":"UTC","default_model":"validator-model"}`
		if err := store.Config.Set("webapp_settings", value); err != nil {
			return fmt.Errorf("seed WebApp settings: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("load WebApp settings: %w", err)
	}
	return nil
}

func stringPointer(value string) *string {
	return &value
}

func validatorFixtureEnabled() bool {
	return validatorHTTPOnlyEnabled() && os.Getenv("VALIDATOR_FIXTURE") == validatorFixtureProfile
}

func configureValidatorBotAdminFixture(server *webapp.Server, store *db.DB) error {
	if server == nil || store == nil {
		return errors.New("validator fixture requires server and database")
	}
	server.SetChannelVerifier(newValidatorChannelVerifier())
	server.SetGroupVerifier(validatorGroupVerifier{})
	server.SetTopicCatalog(validatorTopicCatalog{store: store})
	server.SetTopicLifecycle(validatorTopicLifecycle{store: store})
	server.SetSettingsApplier(validatorSettingsApplier{store: store}.Apply)
	server.SetDigestRunner(validatorDigestRunner{store: store})
	return nil
}

type validatorDisabledChannelVerifier struct{}

func (validatorDisabledChannelVerifier) Verify(context.Context, string) (string, error) {
	return "", errors.New("channel verification is disabled in validator HTTP mode")
}

type validatorDisabledGroupVerifier struct{}

func (validatorDisabledGroupVerifier) Verify(int64) (string, error) {
	return "", errors.New("group verification is disabled in validator HTTP mode")
}

type validatorSettingsApplier struct {
	store *db.DB
}

func (a validatorSettingsApplier) Apply(ctx context.Context, mutation webapp.SettingsMutation) (int64, error) {
	if a.store == nil {
		return 0, errors.New("validator settings applier is not configured")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if mutation.Version <= 0 {
		return 0, db.ErrConflict
	}
	payload := map[string]any{
		"digest_time":   strings.TrimSpace(mutation.DigestTime),
		"timezone":      strings.TrimSpace(mutation.Timezone),
		"default_model": strings.TrimSpace(mutation.DefaultModel),
	}
	if mutation.Channels != nil {
		payload["channels"] = append([]string(nil), mutation.Channels...)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("encode validator settings: %w", err)
	}
	return a.store.Config.SetOptimistic("webapp_settings", string(encoded), mutation.Version)
}

type validatorChannelVerifier struct {
	attempts *sync.Map
}

func newValidatorChannelVerifier() validatorChannelVerifier {
	return validatorChannelVerifier{attempts: &sync.Map{}}
}

func (v validatorChannelVerifier) Verify(_ context.Context, username string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(strings.TrimPrefix(username, "@"))) {
	case validatorFixtureChannelNotFound:
		return "", fmt.Errorf("validator channel %s: %w", username, parser.ErrChannelNotFound)
	case validatorFixtureChannelPrivate:
		return "", fmt.Errorf("validator channel %s: %w", username, parser.ErrChannelPrivate)
	case validatorFixtureChannelTransient:
		if v.attempts != nil {
			value, _ := v.attempts.LoadOrStore(username, 0)
			attempt := value.(int)
			v.attempts.Store(username, attempt+1)
			if attempt >= 2 {
				return "Validator Recovered Channel", nil
			}
		}
		return "", fmt.Errorf("validator channel %s: %w", username, validatorTransientFailure{})
	case validatorFixtureChannelRateLimited:
		return "", validatorRateLimitFailure{rate: &parser.RateLimitError{RetryAfter: time.Millisecond}}
	case validatorFixtureChannelEmpty:
		return "Validator Empty Channel", nil
	default:
		return "Validator Local Channel", nil
	}
}

type validatorTransientFailure struct{}

func (validatorTransientFailure) Error() string   { return validatorTransientError.Error() }
func (validatorTransientFailure) Timeout() bool   { return true }
func (validatorTransientFailure) Temporary() bool { return true }
func (validatorTransientFailure) Unwrap() error   { return validatorTransientError }

type validatorRateLimitFailure struct {
	rate *parser.RateLimitError
}

func (e validatorRateLimitFailure) Error() string {
	return e.rate.Error()
}

func (e validatorRateLimitFailure) Unwrap() []error {
	return []error{validatorRateLimitError, e.rate}
}

type validatorGroupVerifier struct{}

func (validatorGroupVerifier) Verify(chatID int64) (string, error) {
	title, _, err := validatorGroupVerifier{}.VerifyGroup(chatID)
	return title, err
}

func (validatorGroupVerifier) VerifyGroup(chatID int64) (string, bool, error) {
	switch chatID {
	case validatorFixtureForumChatID:
		return "Validator Forum", true, nil
	case validatorFixtureRegularChatID:
		return "Validator Regular Group", false, nil
	case -1007000000099:
		return "", false, validatorGroupError
	default:
		return "Validator Group " + strconv.FormatInt(chatID, 10), true, nil
	}
}

type validatorTopicCatalog struct {
	store *db.DB
}

func (c validatorTopicCatalog) ListTopics(_ context.Context, groupID int64) ([]webapp.Topic, error) {
	if c.store == nil {
		return nil, errors.New("validator topic catalog is not configured")
	}
	group, err := c.store.Groups.GetByID(groupID)
	if err != nil {
		return nil, err
	}
	if group.Status != "" && group.Status != model.GroupStatusActive {
		return []webapp.Topic{}, nil
	}
	topics, err := c.store.ForumTopics.ListOpen(groupID)
	if err != nil {
		return nil, fmt.Errorf("list validator topics: %w", err)
	}
	result := make([]webapp.Topic, 0, len(topics))
	for _, topic := range topics {
		if topic.MessageThreadID > 0 && strings.TrimSpace(topic.Name) != "" {
			result = append(result, webapp.Topic{
				MessageThreadID: topic.MessageThreadID,
				Name:            topic.Name,
			})
		}
	}
	return result, nil
}

type validatorTopicLifecycle struct {
	store *db.DB
}

func (l validatorTopicLifecycle) CheckTopicPermission(context.Context, int64) error {
	return nil
}

func (l validatorTopicLifecycle) CreateChannelTopic(_ context.Context, groupID, channelID int64) error {
	if l.store == nil {
		return errors.New("validator topic lifecycle is not configured")
	}
	threadID := int64(900 + channelID%100)
	if err := l.store.ForumTopics.PersistOwned(groupID, threadID, "Validator created topic"); err != nil {
		return err
	}
	return l.store.Groups.UpdateChannelTopic(groupID, channelID, threadID)
}

func (l validatorTopicLifecycle) RemoveChannelTopic(_ context.Context, groupID, channelID int64) error {
	if l.store == nil {
		return errors.New("validator topic lifecycle is not configured")
	}
	assignments, err := l.store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		return err
	}
	for _, assignment := range assignments {
		if assignment.ChannelID == channelID {
			return l.store.Groups.UnassignChannel(groupID, channelID)
		}
	}
	return db.ErrNotFound
}

func (l validatorTopicLifecycle) RemoveChannelTopicWithVersion(_ context.Context, groupID, channelID, version int64) (int64, error) {
	if l.store == nil {
		return 0, errors.New("validator topic lifecycle is not configured")
	}
	return l.store.Groups.UnassignChannelOptimistic(groupID, channelID, version)
}

type validatorDigestRunner struct {
	store *db.DB
}

func (r validatorDigestRunner) GenerateManual(groupID int64) (*digest.Digest, error) {
	if r.store == nil {
		return nil, errors.New("validator digest runner is not configured")
	}
	group, err := r.store.Groups.GetByID(groupID)
	if err != nil {
		return nil, err
	}
	if group.Status != "" && group.Status != model.GroupStatusActive {
		return &digest.Digest{
			GroupID: groupID,
			Outcome: digest.OutcomeNoPosts,
			Message: "В группе нет доступных каналов для дайджеста.",
		}, nil
	}
	return &digest.Digest{
		GroupID:        groupID,
		PostCount:      1,
		ChannelCount:   1,
		Outcome:        digest.OutcomeSucceeded,
		Message:        "Тестовый дайджест отправлен локально.",
		SummariesSaved: true,
		Delivered:      true,
	}, nil
}

// validatorHTTPTransport returns deterministic OpenAI-compatible responses
// without opening a socket. It is used by provider validation in safe mode.
type validatorHTTPTransport struct{}

func (validatorHTTPTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil {
		return nil, errors.New("validator transport received nil request")
	}
	body := `{"choices":[{"message":{"content":"OK"}}]}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Request:    request,
	}, nil
}

func validatorOwnerInitData() string {
	values := url.Values{}
	values.Set("auth_date", strconv.FormatInt(time.Now().Unix(), 10))
	values.Set("query_id", "validator-bot-admin-r2")
	values.Set("user", `{"id":715602446,"first_name":"Validator","username":"validator_owner"}`)
	dataCheckString := strings.Join([]string{
		"auth_date=" + values.Get("auth_date"),
		"query_id=" + values.Get("query_id"),
		"user=" + values.Get("user"),
	}, "\n")
	secretMAC := hmac.New(sha256.New, []byte("WebAppData"))
	_, _ = secretMAC.Write([]byte("validator:fixture-test"))
	hashMAC := hmac.New(sha256.New, secretMAC.Sum(nil))
	_, _ = hashMAC.Write([]byte(dataCheckString))
	values.Set("hash", hex.EncodeToString(hashMAC.Sum(nil)))
	return values.Encode()
}
