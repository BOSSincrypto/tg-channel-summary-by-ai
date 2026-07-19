package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/parser"
)

type fakeChannelVerifier struct {
	err error
}

func (f fakeChannelVerifier) Verify(context.Context, string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return "Verified title", nil
}

type sequenceChannelVerifier struct {
	results []error
	calls   int
}

func (f *sequenceChannelVerifier) Verify(context.Context, string) (string, error) {
	f.calls++
	index := f.calls - 1
	if index >= len(f.results) {
		index = len(f.results) - 1
	}
	if f.results[index] != nil {
		return "", f.results[index]
	}
	return "Recovered title", nil
}

type recordingRetrySleeper struct {
	delays []time.Duration
}

func (s *recordingRetrySleeper) Sleep(_ context.Context, delay time.Duration) error {
	s.delays = append(s.delays, delay)
	return nil
}

type wrappedVerifierNetError struct {
	message string
}

func (e *wrappedVerifierNetError) Error() string   { return e.message }
func (e *wrappedVerifierNetError) Timeout() bool   { return false }
func (e *wrappedVerifierNetError) Temporary() bool { return false }

var _ net.Error = (*wrappedVerifierNetError)(nil)

type scriptedVerifierTransport struct {
	base       http.RoundTripper
	failures   []error
	calls      int
	serverHits int
}

func (t *scriptedVerifierTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.calls++
	if len(t.failures) > 0 {
		err := t.failures[0]
		t.failures = t.failures[1:]
		return nil, err
	}
	t.serverHits++
	return t.base.RoundTrip(request)
}

func wrappedVerifierTransportError(message string) error {
	return fmt.Errorf("transport wrapper: %w", &url.Error{
		Op:  http.MethodGet,
		URL: "https://t.me/s/retry_",
		Err: &wrappedVerifierNetError{message: message},
	})
}

func newBackendTestServer(t *testing.T) (*Server, *db.DB) {
	t.Helper()
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := NewWithProvidersForTesting(store, 0, http.DefaultClient)
	server.SetChannelVerifier(fakeChannelVerifier{})
	return server, store
}

func doJSON(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func TestChannelsAPIValidatesNormalizesAndRejectsDuplicates(t *testing.T) {
	server, _ := newBackendTestServer(t)

	invalid := doJSON(t, server.Handler(), http.MethodPost, "/api/channels", `{"username":"@@bad"}`)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid status = %d, want 400", invalid.Code)
	}

	created := doJSON(t, server.Handler(), http.MethodPost, "/api/channels", `{"username":"@Durov_"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", created.Code, created.Body.String())
	}
	var channel map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &channel); err != nil {
		t.Fatalf("decode channel: %v", err)
	}
	if channel["username"] != "durov_" || channel["title"] != "Verified title" {
		t.Fatalf("channel response = %#v", channel)
	}
	if channel["version"].(float64) != 1 {
		t.Fatalf("channel version = %#v, want 1", channel["version"])
	}

	duplicate := doJSON(t, server.Handler(), http.MethodPost, "/api/channels", `{"username":"@DUROV_"}`)
	if duplicate.Code != http.StatusConflict || !strings.Contains(duplicate.Body.String(), "Канал уже добавлен") {
		t.Fatalf("duplicate = %d %s", duplicate.Code, duplicate.Body.String())
	}
}

func TestChannelCreateRetriesTransientVerificationAndPersistsOnce(t *testing.T) {
	server, store := newBackendTestServer(t)
	verifier := &sequenceChannelVerifier{results: []error{
		wrappedVerifierTransportError("network unreachable"),
		errors.New("fetch t.me/s/retry_: HTTP 503 Service Unavailable"),
		nil,
	}}
	sleeper := &recordingRetrySleeper{}
	server.SetChannelVerifier(verifier)
	server.SetChannelVerificationRetry(3, sleeper.Sleep)

	response := doJSON(t, server.Handler(), http.MethodPost, "/api/channels", `{"username":"retry_"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("recovered create status = %d, body=%s", response.Code, response.Body.String())
	}
	if verifier.calls != 3 || len(sleeper.delays) != 2 {
		t.Fatalf("verification calls=%d sleeps=%v, want three calls and two sleeps", verifier.calls, sleeper.delays)
	}
	var count int
	if err := store.Conn().QueryRow(`SELECT COUNT(*) FROM channels WHERE username = 'retry_'`).Scan(&count); err != nil {
		t.Fatalf("count recovered channels: %v", err)
	}
	if count != 1 {
		t.Fatalf("recovered channel count = %d, want 1", count)
	}
}

func TestChannelCreateExhaustedVerificationRetriesDoesNotPersist(t *testing.T) {
	server, store := newBackendTestServer(t)
	verifier := &sequenceChannelVerifier{results: []error{
		wrappedVerifierTransportError("network unreachable"),
		errors.New("fetch t.me/s/exhaust_: HTTP 503 Service Unavailable"),
		wrappedVerifierTransportError("broken pipe"),
	}}
	sleeper := &recordingRetrySleeper{}
	server.SetChannelVerifier(verifier)
	server.SetChannelVerificationRetry(3, sleeper.Sleep)

	response := doJSON(t, server.Handler(), http.MethodPost, "/api/channels", `{"username":"exhaust_"}`)
	if response.Code != http.StatusBadGateway && response.Code != http.StatusServiceUnavailable {
		t.Fatalf("exhausted create status = %d, body=%s", response.Code, response.Body.String())
	}
	if verifier.calls != 3 || len(sleeper.delays) != 2 {
		t.Fatalf("verification calls=%d sleeps=%v, want three calls and two sleeps", verifier.calls, sleeper.delays)
	}
	var count int
	if err := store.Conn().QueryRow(`SELECT COUNT(*) FROM channels WHERE username = 'exhaust_'`).Scan(&count); err != nil {
		t.Fatalf("count exhausted channels: %v", err)
	}
	if count != 0 {
		t.Fatalf("exhausted channel count = %d, want 0", count)
	}
}

func TestChannelCreateClassifiesWrappedTransportErrorsAtProductionBoundary(t *testing.T) {
	tests := []struct {
		name           string
		failures       []error
		maxRetries     int
		wantStatus     int
		wantCalls      int
		wantServerHits int
		wantSleeps     int
		wantRows       int
	}{
		{
			name: "wrapped network unreachable recovers",
			failures: []error{
				wrappedVerifierTransportError("network unreachable"),
				wrappedVerifierTransportError("timeout"),
				wrappedVerifierTransportError("broken pipe"),
			},
			maxRetries:     4,
			wantStatus:     http.StatusCreated,
			wantCalls:      4,
			wantServerHits: 1,
			wantSleeps:     3,
			wantRows:       1,
		},
		{
			name: "wrapped transport exhaustion does not persist",
			failures: []error{
				wrappedVerifierTransportError("network unreachable"),
				wrappedVerifierTransportError("timeout"),
				wrappedVerifierTransportError("broken pipe"),
			},
			maxRetries:     3,
			wantStatus:     http.StatusBadGateway,
			wantCalls:      3,
			wantServerHits: 0,
			wantSleeps:     2,
			wantRows:       0,
		},
		{
			name:           "permanent verifier error is not retried",
			failures:       []error{errors.New("permanent verifier failure")},
			maxRetries:     4,
			wantStatus:     http.StatusBadGateway,
			wantCalls:      1,
			wantServerHits: 0,
			wantSleeps:     0,
			wantRows:       0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := db.Open(":memory:")
			if err != nil {
				t.Fatalf("open database: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `<div class="tgme_channel_info"><h1>Recovered title</h1></div>`)
			}))
			t.Cleanup(upstream.Close)

			transport := &scriptedVerifierTransport{
				base:     http.DefaultTransport,
				failures: append([]error(nil), tt.failures...),
			}
			channelParser := parser.NewWithOptions(parser.Options{
				Client:  &http.Client{Transport: transport},
				BaseURL: upstream.URL,
			})
			server := NewWithProvidersForTesting(store, 0, http.DefaultClient)
			server.SetChannelVerifier(parserChannelVerifier{parser: channelParser})
			sleeper := &recordingRetrySleeper{}
			server.SetChannelVerificationRetry(tt.maxRetries, sleeper.Sleep)

			response := doJSON(t, server.Handler(), http.MethodPost, "/api/channels", `{"username":"transport_"}`)
			if response.Code != tt.wantStatus {
				t.Fatalf("create status = %d, body=%s", response.Code, response.Body.String())
			}
			if transport.calls != tt.wantCalls || transport.serverHits != tt.wantServerHits {
				t.Fatalf("transport calls=%d server hits=%d, want calls=%d server hits=%d",
					transport.calls, transport.serverHits, tt.wantCalls, tt.wantServerHits)
			}
			if len(sleeper.delays) != tt.wantSleeps {
				t.Fatalf("backoff count = %d, want %d", len(sleeper.delays), tt.wantSleeps)
			}
			var rows int
			if err := store.Conn().QueryRow(`SELECT COUNT(*) FROM channels WHERE username = 'transport_'`).Scan(&rows); err != nil {
				t.Fatalf("count channels: %v", err)
			}
			if rows != tt.wantRows {
				t.Fatalf("channel rows = %d, want %d", rows, tt.wantRows)
			}
		})
	}
}

func TestChannelMutationsRequireCurrentPositiveVersion(t *testing.T) {
	server, store := newBackendTestServer(t)
	id, err := store.Channels.Insert(&model.Channel{Username: "locked_", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	before, err := store.Channels.GetByID(id)
	if err != nil {
		t.Fatalf("load channel: %v", err)
	}

	for _, body := range []string{`{"enabled":false}`, `{"enabled":false,"version":0}`} {
		response := doJSON(t, server.Handler(), http.MethodPatch, "/api/channels/"+jsonNumber(id), body)
		if response.Code != http.StatusConflict {
			t.Fatalf("invalid version status = %d, body=%s", response.Code, response.Body.String())
		}
	}
	malformed := doJSON(t, server.Handler(), http.MethodPatch, "/api/channels/"+jsonNumber(id), `{"enabled":false,"version":"1"}`)
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed version status = %d, body=%s", malformed.Code, malformed.Body.String())
	}
	stale := doJSON(t, server.Handler(), http.MethodPatch, "/api/channels/"+jsonNumber(id), `{"enabled":false,"version":99}`)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale version status = %d, body=%s", stale.Code, stale.Body.String())
	}
	unchanged, err := store.Channels.GetByID(id)
	if err != nil {
		t.Fatalf("load unchanged channel: %v", err)
	}
	if *unchanged != *before {
		t.Fatalf("rejected mutation changed channel: before=%#v after=%#v", before, unchanged)
	}

	current := doJSON(t, server.Handler(), http.MethodPatch, "/api/channels/"+jsonNumber(id), `{"enabled":false,"version":1}`)
	if current.Code != http.StatusOK {
		t.Fatalf("current version status = %d, body=%s", current.Code, current.Body.String())
	}
	updated, err := store.Channels.GetByID(id)
	if err != nil {
		t.Fatalf("load updated channel: %v", err)
	}
	if updated.Enabled || updated.Version != 2 {
		t.Fatalf("updated channel = %#v, want disabled version 2", updated)
	}

	deleteStale := doJSON(t, server.Handler(), http.MethodDelete, "/api/channels/"+jsonNumber(id), `{"version":1}`)
	if deleteStale.Code != http.StatusConflict {
		t.Fatalf("stale delete status = %d, body=%s", deleteStale.Code, deleteStale.Body.String())
	}
	deleteCurrent := doJSON(t, server.Handler(), http.MethodDelete, "/api/channels/"+jsonNumber(id), `{"version":2}`)
	if deleteCurrent.Code != http.StatusNoContent {
		t.Fatalf("current delete status = %d, body=%s", deleteCurrent.Code, deleteCurrent.Body.String())
	}
}

func TestGroupsAPIUsesStringChatIDAndRejectsDuplicateAssignments(t *testing.T) {
	server, store := newBackendTestServer(t)
	available := httptest.NewRecorder()
	server.Handler().ServeHTTP(available, httptest.NewRequest(http.MethodGet, "/api/groups/available", nil))
	if available.Code != http.StatusOK {
		t.Fatalf("available groups status = %d, body=%s", available.Code, available.Body.String())
	}
	channelID, err := store.Channels.Insert(&model.Channel{Username: "channel_"})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}

	created := doJSON(t, server.Handler(), http.MethodPost, "/api/groups", `{"chat_id":"-1002234567890123"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create group status = %d, body=%s", created.Code, created.Body.String())
	}
	var group map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &group); err != nil {
		t.Fatalf("decode group: %v", err)
	}
	if group["chat_id"] != "-1002234567890123" || group["telegram_chat_id"] != "-1002234567890123" {
		t.Fatalf("chat id was not serialized as string: %#v", group)
	}
	groupID := int64(group["id"].(float64))

	assignBody := `{"channel_id":"` + strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(jsonNumber(channelID)), "+")) + `"}`
	assigned := doJSON(t, server.Handler(), http.MethodPost, "/api/groups/"+jsonNumber(groupID)+"/channels", assignBody)
	if assigned.Code != http.StatusCreated {
		t.Fatalf("assign status = %d, body=%s", assigned.Code, assigned.Body.String())
	}
	duplicate := doJSON(t, server.Handler(), http.MethodPost, "/api/groups/"+jsonNumber(groupID)+"/channels", assignBody)
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate assignment status = %d, body=%s", duplicate.Code, duplicate.Body.String())
	}
}

func TestProductionTopicLifecycleIsUsedForAssignmentAndRemoval(t *testing.T) {
	server, store := newBackendTestServer(t)
	lifecycle := &fakeTopicLifecycle{store: store}
	server.SetTopicLifecycle(lifecycle)

	channelID, err := store.Channels.Insert(&model.Channel{Username: "topic_news", Title: "Topic News", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -1008,
		Title:          "Forum",
		Status:         model.GroupStatusActive,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}

	assigned := doJSON(t, server.Handler(), http.MethodPost,
		"/api/groups/"+jsonNumber(groupID)+"/channels",
		`{"channel_id":"`+jsonNumber(channelID)+`"}`)
	if assigned.Code != http.StatusCreated {
		t.Fatalf("assignment status = %d, body=%s", assigned.Code, assigned.Body.String())
	}
	if len(lifecycle.created) != 1 || lifecycle.created[0] != [2]int64{groupID, channelID} {
		t.Fatalf("created lifecycle calls = %#v", lifecycle.created)
	}
	assignments, err := store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		t.Fatalf("load assignments: %v", err)
	}
	if len(assignments) != 1 || assignments[0].TopicThreadID == nil || *assignments[0].TopicThreadID <= 0 {
		t.Fatalf("assignment = %#v, want persisted positive topic", assignments)
	}

	removed := doJSON(t, server.Handler(), http.MethodDelete,
		"/api/groups/"+jsonNumber(groupID)+"/channels/"+jsonNumber(channelID), "")
	if removed.Code != http.StatusNoContent {
		t.Fatalf("removal status = %d, body=%s", removed.Code, removed.Body.String())
	}
	if len(lifecycle.removed) != 1 || lifecycle.removed[0] != [2]int64{groupID, channelID} {
		t.Fatalf("removed lifecycle calls = %#v", lifecycle.removed)
	}
	assignments, err = store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		t.Fatalf("load assignments after removal: %v", err)
	}
	if len(assignments) != 0 {
		t.Fatalf("assignments after removal = %#v, want none", assignments)
	}
}

func TestProductionTopicLifecycleFailureLeavesAssignmentUnchanged(t *testing.T) {
	server, store := newBackendTestServer(t)
	lifecycle := &fakeTopicLifecycle{err: errors.New("telegram topic unavailable")}
	server.SetTopicLifecycle(lifecycle)

	channelID, err := store.Channels.Insert(&model.Channel{Username: "topic_fail", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -1009,
		Title:          "Forum",
		Status:         model.GroupStatusActive,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}

	response := doJSON(t, server.Handler(), http.MethodPost,
		"/api/groups/"+jsonNumber(groupID)+"/channels",
		`{"channel_id":"`+jsonNumber(channelID)+`"}`)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("failure status = %d, body=%s", response.Code, response.Body.String())
	}
	assignments, err := store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		t.Fatalf("load assignments after failure: %v", err)
	}
	if len(assignments) != 0 {
		t.Fatalf("assignments after failed lifecycle = %#v, want none", assignments)
	}
}

func TestProductionWebAppTopicCloseFailureLeavesDurablePendingState(t *testing.T) {
	server, store := newBackendTestServer(t)
	lifecycle := &failureRecoverableTopicLifecycle{store: store, failMarkClosed: true}
	server.SetTopicLifecycle(lifecycle)

	channelID, err := store.Channels.Insert(&model.Channel{Username: "webapp_pending_close", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -1011,
		Title:          "Forum",
		Status:         model.GroupStatusActive,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}

	assigned := doJSON(t, server.Handler(), http.MethodPost,
		"/api/groups/"+jsonNumber(groupID)+"/channels",
		`{"channel_id":"`+jsonNumber(channelID)+`"}`)
	if assigned.Code != http.StatusCreated {
		t.Fatalf("assignment status = %d, body=%s", assigned.Code, assigned.Body.String())
	}
	if _, err := store.Conn().Exec(`
		UPDATE forum_topics SET close_pending = 1
		WHERE group_id = ? AND message_thread_id = ?`, groupID, lifecycle.threadID); err != nil {
		t.Fatalf("record simulated pending close: %v", err)
	}
	pendingView := doJSON(t, server.Handler(), http.MethodGet,
		"/api/groups/"+jsonNumber(groupID), "")
	if pendingView.Code != http.StatusOK {
		t.Fatalf("pending group status = %d, body=%s", pendingView.Code, pendingView.Body.String())
	}
	var pendingGroup map[string]any
	if err := json.Unmarshal(pendingView.Body.Bytes(), &pendingGroup); err != nil {
		t.Fatalf("decode pending group: %v", err)
	}
	if assignments, ok := pendingGroup["assignments"].([]any); !ok || len(assignments) != 0 {
		t.Fatalf("pending assignments view = %#v, want hidden", pendingGroup["assignments"])
	}
	if err := store.ForumTopics.MarkReopened(groupID, lifecycle.threadID); err != nil {
		t.Fatalf("clear simulated pending close: %v", err)
	}
	removed := doJSON(t, server.Handler(), http.MethodDelete,
		"/api/groups/"+jsonNumber(groupID)+"/channels/"+jsonNumber(channelID), "")
	if removed.Code != http.StatusInternalServerError {
		t.Fatalf("removal status = %d, body=%s", removed.Code, removed.Body.String())
	}
	if len(lifecycle.closeForumTopicCalls) != 1 {
		t.Fatalf("CloseForumTopic calls = %d, want one successful external close", len(lifecycle.closeForumTopicCalls))
	}
	assignments, err := store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		t.Fatalf("load assignments after failed mark: %v", err)
	}
	if len(assignments) != 0 {
		t.Fatalf("assignments after failed mark = %#v, want removed", assignments)
	}
	topic, err := store.ForumTopics.Get(groupID, lifecycle.threadID)
	if err != nil {
		t.Fatalf("load pending topic: %v", err)
	}
	if !topic.ClosePending || topic.Closed {
		t.Fatalf("topic after failed mark = %#v, want pending recovery", topic)
	}

	lifecycle.failMarkClosed = false
	if err := lifecycle.Reconcile(); err != nil {
		t.Fatalf("reconcile pending topic: %v", err)
	}
	topic, err = store.ForumTopics.Get(groupID, lifecycle.threadID)
	if err != nil {
		t.Fatalf("load reconciled topic: %v", err)
	}
	if !topic.Closed || topic.ClosePending {
		t.Fatalf("topic after reconciliation = %#v, want closed", topic)
	}
	if err := lifecycle.Reconcile(); err != nil {
		t.Fatalf("repeat reconcile: %v", err)
	}
	if len(lifecycle.closeForumTopicCalls) != 2 {
		t.Fatalf("repeat CloseForumTopic calls = %d, want idempotent recovery", len(lifecycle.closeForumTopicCalls))
	}
}

func TestNonForumAssignmentDoesNotCallTopicLifecycle(t *testing.T) {
	server, store := newBackendTestServer(t)
	lifecycle := &fakeTopicLifecycle{store: store}
	server.SetTopicLifecycle(lifecycle)

	channelID, err := store.Channels.Insert(&model.Channel{Username: "regular_news", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -1010,
		Title:          "Ineligible",
		Status:         model.GroupStatusIneligible,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}

	response := doJSON(t, server.Handler(), http.MethodPost,
		"/api/groups/"+jsonNumber(groupID)+"/channels",
		`{"channel_id":"`+jsonNumber(channelID)+`"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("assignment status = %d, body=%s", response.Code, response.Body.String())
	}
	if len(lifecycle.created) != 0 {
		t.Fatalf("non-forum lifecycle calls = %#v, want none", lifecycle.created)
	}
}

func TestForumTopicCatalogReturnsPersistedPositiveTopics(t *testing.T) {
	server, store := newBackendTestServer(t)
	channelID, err := store.Channels.Insert(&model.Channel{Username: "catalog_news", Title: "Catalog News", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -1013,
		Title:          "Forum",
		Status:         model.GroupStatusActive,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	threadID := int64(901)
	if err := store.Groups.AssignChannel(groupID, channelID, &threadID); err != nil {
		t.Fatalf("assign topic: %v", err)
	}
	if err := store.ForumTopics.Observe(groupID, threadID, "Catalog News"); err != nil {
		t.Fatalf("observe assigned topic: %v", err)
	}

	response := doJSON(t, server.Handler(), http.MethodGet, "/api/groups/"+jsonNumber(groupID)+"/topics", "")
	if response.Code != http.StatusOK {
		t.Fatalf("topics status = %d, body=%s", response.Code, response.Body.String())
	}
	var topics []map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &topics); err != nil {
		t.Fatalf("decode topics: %v", err)
	}
	if len(topics) != 1 || topics[0]["message_thread_id"] != float64(threadID) || topics[0]["name"] != "Catalog News" {
		t.Fatalf("topics = %#v, want persisted positive topic", topics)
	}
}

func TestProductionForumTopicCatalogReturnsUnassignedObservedTopic(t *testing.T) {
	server, store := newBackendTestServer(t)
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -1018,
		Title:          "Observed Forum",
		Status:         model.GroupStatusActive,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := store.ForumTopics.Observe(groupID, 1501, "Existing unassigned topic"); err != nil {
		t.Fatalf("observe topic: %v", err)
	}

	response := doJSON(t, server.Handler(), http.MethodGet,
		"/api/groups/"+jsonNumber(groupID)+"/topics", "")
	if response.Code != http.StatusOK {
		t.Fatalf("topics status = %d, body=%s", response.Code, response.Body.String())
	}
	var topics []Topic
	if err := json.Unmarshal(response.Body.Bytes(), &topics); err != nil {
		t.Fatalf("decode topics: %v", err)
	}
	if len(topics) != 1 || topics[0].MessageThreadID != 1501 ||
		topics[0].Name != "Existing unassigned topic" {
		t.Fatalf("topics = %#v, want the real unassigned topic", topics)
	}
	if assignments, err := store.Groups.GetChannelAssignments(groupID); err != nil {
		t.Fatalf("load assignments: %v", err)
	} else if len(assignments) != 0 {
		t.Fatalf("topic discovery created assignments = %#v", assignments)
	}
}

func TestProductionForumTopicSelectionReusesObservedTopic(t *testing.T) {
	server, store := newBackendTestServer(t)
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -1019,
		Title:          "Observed Forum",
		Status:         model.GroupStatusActive,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	channelID, err := store.Channels.Insert(&model.Channel{Username: "observed_selection", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	if err := store.ForumTopics.Observe(groupID, 1502, "Reusable topic"); err != nil {
		t.Fatalf("observe topic: %v", err)
	}
	response := doJSON(t, server.Handler(), http.MethodPost,
		"/api/groups/"+jsonNumber(groupID)+"/channels",
		`{"channel_id":"`+jsonNumber(channelID)+`","topic_thread_id":1502}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("selection status = %d, body=%s", response.Code, response.Body.String())
	}
	assignments, err := store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		t.Fatalf("load selected assignment: %v", err)
	}
	if len(assignments) != 1 || assignments[0].TopicThreadID == nil ||
		*assignments[0].TopicThreadID != 1502 {
		t.Fatalf("selected assignment = %#v", assignments)
	}
}

func TestInjectedForumTopicCatalogIsUsedForSelection(t *testing.T) {
	server, store := newBackendTestServer(t)
	catalogChannelID, err := store.Channels.Insert(&model.Channel{Username: "injected_catalog", Title: "Injected Channel", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -1016, Title: "Forum", Status: model.GroupStatusActive})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	server.SetTopicCatalog(staticTopicCatalog{topics: []Topic{{MessageThreadID: 1201, Name: "Announcements"}}})

	topicsResponse := doJSON(t, server.Handler(), http.MethodGet, "/api/groups/"+jsonNumber(groupID)+"/topics", "")
	if topicsResponse.Code != http.StatusOK || !strings.Contains(topicsResponse.Body.String(), `"message_thread_id":1201`) {
		t.Fatalf("injected topics = %d %s", topicsResponse.Code, topicsResponse.Body.String())
	}
	assignmentResponse := doJSON(t, server.Handler(), http.MethodPost,
		"/api/groups/"+jsonNumber(groupID)+"/channels",
		`{"channel_id":"`+jsonNumber(catalogChannelID)+`","topic_thread_id":1201}`)
	if assignmentResponse.Code != http.StatusCreated {
		t.Fatalf("injected topic assignment = %d %s", assignmentResponse.Code, assignmentResponse.Body.String())
	}
	assignments, err := store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		t.Fatalf("load injected assignment: %v", err)
	}
	if len(assignments) != 1 || assignments[0].TopicThreadID == nil || *assignments[0].TopicThreadID != 1201 {
		t.Fatalf("injected assignment = %#v", assignments)
	}
}

func TestWebAppGroupCreationPersistsNonForumEligibility(t *testing.T) {
	server, store := newBackendTestServer(t)
	server.SetGroupVerifier(fakeForumGroupVerifier{title: "Regular", isForum: false})
	channelID, err := store.Channels.Insert(&model.Channel{Username: "created_regular", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}

	created := doJSON(t, server.Handler(), http.MethodPost, "/api/groups", `{"chat_id":"-1017"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create non-forum status = %d, body=%s", created.Code, created.Body.String())
	}
	var group map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &group); err != nil {
		t.Fatalf("decode created group: %v", err)
	}
	if group["is_forum"] != false || group["status"] != model.GroupStatusIneligible {
		t.Fatalf("created non-forum group = %#v", group)
	}
	groupID := int64(group["id"].(float64))
	assignment := doJSON(t, server.Handler(), http.MethodPost,
		"/api/groups/"+jsonNumber(groupID)+"/channels",
		`{"channel_id":"`+jsonNumber(channelID)+`","topic_thread_id":1202}`)
	if assignment.Code != http.StatusBadRequest {
		t.Fatalf("non-forum topic assignment status = %d, body=%s", assignment.Code, assignment.Body.String())
	}
	stored, err := store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		t.Fatalf("load non-forum assignments: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("non-forum invalid assignment persisted = %#v", stored)
	}
}

func TestNonForumResponsesOmitTopicFieldsAndRejectTopicPayloads(t *testing.T) {
	server, store := newBackendTestServer(t)
	channelID, err := store.Channels.Insert(&model.Channel{Username: "regular_catalog", Title: "Regular", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	unassignedChannelID, err := store.Channels.Insert(&model.Channel{Username: "regular_unassigned", Title: "Regular Unassigned", Enabled: true})
	if err != nil {
		t.Fatalf("insert unassigned channel: %v", err)
	}
	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -1014,
		Title:          "Regular",
		Status:         model.GroupStatusIneligible,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	threadID := int64(902)
	if err := store.Groups.AssignChannel(groupID, channelID, &threadID); err != nil {
		t.Fatalf("assign topic fixture: %v", err)
	}

	groupResponse := doJSON(t, server.Handler(), http.MethodGet, "/api/groups/"+jsonNumber(groupID), "")
	if groupResponse.Code != http.StatusOK {
		t.Fatalf("group status = %d, body=%s", groupResponse.Code, groupResponse.Body.String())
	}
	var group map[string]any
	if err := json.Unmarshal(groupResponse.Body.Bytes(), &group); err != nil {
		t.Fatalf("decode group: %v", err)
	}
	if group["is_forum"] != false {
		t.Fatalf("is_forum = %#v, want false", group["is_forum"])
	}
	assignments := group["assignments"].([]any)
	if len(assignments) != 1 {
		t.Fatalf("assignments = %#v, want one assignment", assignments)
	}
	if _, exists := assignments[0].(map[string]any)["topic_thread_id"]; exists {
		t.Fatalf("non-forum assignment leaked topic_thread_id: %#v", assignments[0])
	}

	topicsResponse := doJSON(t, server.Handler(), http.MethodGet, "/api/groups/"+jsonNumber(groupID)+"/topics", "")
	if topicsResponse.Code != http.StatusOK || strings.TrimSpace(topicsResponse.Body.String()) != "[]" {
		t.Fatalf("non-forum topics = %d %s, want empty array", topicsResponse.Code, topicsResponse.Body.String())
	}

	assignmentResponse := doJSON(t, server.Handler(), http.MethodPost,
		"/api/groups/"+jsonNumber(groupID)+"/channels",
		`{"channel_id":"`+jsonNumber(unassignedChannelID)+`","topic_thread_id":"903"}`)
	if assignmentResponse.Code != http.StatusBadRequest {
		t.Fatalf("non-forum topic assignment status = %d, body=%s", assignmentResponse.Code, assignmentResponse.Body.String())
	}
	stored, err := store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		t.Fatalf("load assignments: %v", err)
	}
	if len(stored) != 1 || stored[0].TopicThreadID == nil || *stored[0].TopicThreadID != threadID {
		t.Fatalf("non-forum rejected mutation changed assignments = %#v", stored)
	}
}

func TestZeroTopicIDIsRejectedWithoutAssignmentMutation(t *testing.T) {
	server, store := newBackendTestServer(t)
	channelID, err := store.Channels.Insert(&model.Channel{Username: "zero_topic", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -1015, Title: "Forum", Status: model.GroupStatusActive})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}

	response := doJSON(t, server.Handler(), http.MethodPost,
		"/api/groups/"+jsonNumber(groupID)+"/channels",
		`{"channel_id":"`+jsonNumber(channelID)+`","topic_thread_id":0}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("zero topic status = %d, body=%s", response.Code, response.Body.String())
	}
	assignments, err := store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		t.Fatalf("load assignments after zero topic: %v", err)
	}
	if len(assignments) != 0 {
		t.Fatalf("zero topic created assignment = %#v", assignments)
	}
}

type fakeTopicLifecycle struct {
	store   *db.DB
	err     error
	created [][2]int64
	removed [][2]int64
}

type failureRecoverableTopicLifecycle struct {
	store                *db.DB
	threadID             int64
	failMarkClosed       bool
	closeForumTopicCalls [][2]int64
}

func (f *failureRecoverableTopicLifecycle) CreateChannelTopic(_ context.Context, groupID, channelID int64) error {
	f.threadID = 2100
	if err := f.store.Groups.UpdateChannelTopic(groupID, channelID, f.threadID); err != nil {
		return err
	}
	return f.store.ForumTopics.PersistOwned(groupID, f.threadID, "WebApp pending")
}

func (f *failureRecoverableTopicLifecycle) RemoveChannelTopic(_ context.Context, groupID, channelID int64) error {
	assignments, err := f.store.Groups.GetChannelAssignments(groupID)
	if err != nil {
		return err
	}
	if len(assignments) != 1 || assignments[0].TopicThreadID == nil {
		return db.ErrNotFound
	}
	if err := f.store.Groups.UnassignChannel(groupID, channelID); err != nil {
		return err
	}
	if err := f.store.ForumTopics.BeginClose(groupID, *assignments[0].TopicThreadID); err != nil {
		return err
	}
	f.closeForumTopicCalls = append(f.closeForumTopicCalls, [2]int64{groupID, *assignments[0].TopicThreadID})
	if f.failMarkClosed {
		return errors.New("injected MarkClosed failure")
	}
	return f.store.ForumTopics.MarkClosed(groupID, *assignments[0].TopicThreadID)
}

func (f *failureRecoverableTopicLifecycle) Reconcile() error {
	pending, err := f.store.ForumTopics.ListPending()
	if err != nil {
		return err
	}
	for _, topic := range pending {
		f.closeForumTopicCalls = append(f.closeForumTopicCalls, [2]int64{topic.GroupID, topic.MessageThreadID})
		if f.failMarkClosed {
			return errors.New("injected MarkClosed failure")
		}
		if err := f.store.ForumTopics.MarkClosed(topic.GroupID, topic.MessageThreadID); err != nil {
			return err
		}
	}
	return nil
}

type staticTopicCatalog struct {
	topics []Topic
	err    error
}

func (c staticTopicCatalog) ListTopics(context.Context, int64) ([]Topic, error) {
	if c.err != nil {
		return nil, c.err
	}
	return append([]Topic(nil), c.topics...), nil
}

type fakeForumGroupVerifier struct {
	title   string
	isForum bool
}

func (f fakeForumGroupVerifier) Verify(int64) (string, error) {
	return f.title, nil
}

func (f fakeForumGroupVerifier) VerifyGroup(int64) (string, bool, error) {
	return f.title, f.isForum, nil
}

func (f *fakeTopicLifecycle) CreateChannelTopic(_ context.Context, groupID, channelID int64) error {
	if f.err != nil {
		return f.err
	}
	f.created = append(f.created, [2]int64{groupID, channelID})
	threadID := int64(700 + len(f.created))
	return f.store.Groups.UpdateChannelTopic(groupID, channelID, threadID)
}

func (f *fakeTopicLifecycle) RemoveChannelTopic(_ context.Context, groupID, channelID int64) error {
	if f.err != nil {
		return f.err
	}
	f.removed = append(f.removed, [2]int64{groupID, channelID})
	return f.store.Groups.UnassignChannel(groupID, channelID)
}

func TestSettingsAPIUsesOptimisticLocking(t *testing.T) {
	server, _ := newBackendTestServer(t)
	first := httptest.NewRecorder()
	server.Handler().ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("settings GET status = %d", first.Code)
	}
	var settings settingsPayload
	if err := json.Unmarshal(first.Body.Bytes(), &settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	update := `{"digest_time":"09:00","timezone":"UTC","default_model":"gpt-4o","version":` + jsonNumber(settings.Version) + `}`
	saved := doJSON(t, server.Handler(), http.MethodPut, "/api/settings", update)
	if saved.Code != http.StatusOK {
		t.Fatalf("settings update status = %d, body=%s", saved.Code, saved.Body.String())
	}
	stale := doJSON(t, server.Handler(), http.MethodPut, "/api/settings", update)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale settings status = %d, body=%s", stale.Code, stale.Body.String())
	}
}

func TestChannelDeleteCascadesAssignments(t *testing.T) {
	server, store := newBackendTestServer(t)
	channelID, err := store.Channels.Insert(&model.Channel{Username: "cascade_"})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	groupID, err := store.Groups.Insert(&model.Group{TelegramChatID: -1009, Title: "Group"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := store.Groups.AssignChannel(groupID, channelID, nil); err != nil {
		t.Fatalf("assign channel: %v", err)
	}
	response := doJSON(t, server.Handler(), http.MethodDelete, "/api/channels/"+jsonNumber(channelID), `{"version":1}`)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", response.Code)
	}
	var count int
	if err := store.Conn().QueryRow(`SELECT COUNT(*) FROM group_channels WHERE channel_id = ?`, channelID).Scan(&count); err != nil {
		t.Fatalf("count assignments: %v", err)
	}
	if count != 0 {
		t.Fatalf("assignment count = %d, want 0", count)
	}
}

func jsonNumber(value int64) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.TrimSpace(string(mustJSON(value))), `"`, ""))
}

func mustJSON(value int64) []byte {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return body
}
