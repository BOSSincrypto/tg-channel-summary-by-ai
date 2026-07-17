package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
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

type fakeTopicLifecycle struct {
	store   *db.DB
	err     error
	created [][2]int64
	removed [][2]int64
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
	response := doJSON(t, server.Handler(), http.MethodDelete, "/api/channels/"+jsonNumber(channelID), "")
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
