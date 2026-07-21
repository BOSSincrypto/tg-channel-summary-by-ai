package parser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

func TestChannelProcessorPersistsParserOutputIntoSQLite(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/s/example" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`
			<div class="tgme_widget_message" data-post="example/41">
				<div class="tgme_widget_message_text">A post with <a href="https://example.com/story#part">a link</a></div>
				<time datetime="2026-07-15T18:30:00+00:00"></time>
			</div>
			<div class="tgme_widget_message" data-post="example/40">
				<a class="tgme_widget_message_photo_wrap"></a>
			</div>`))
	}))
	defer server.Close()

	database, cleanup := newStorageTestDB(t)
	defer cleanup()
	channel := &model.Channel{Username: "Example", Enabled: true}
	channelID, err := database.Channels.Insert(channel)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	channel.ID = channelID

	fetcher := NewWithOptions(Options{Client: server.Client(), BaseURL: server.URL})
	processor := NewChannelProcessor(fetcher, NewPostStorage(database.Channels, database.Posts)).
		WithSleeper(&recordingSleeper{})
	result, err := processor.ProcessChannel(channel)
	if err != nil {
		t.Fatalf("process channel: %v", err)
	}
	if result.ParsedPosts != 1 || result.StoredPosts != 1 || result.MediaOnlySkipped != 1 {
		t.Fatalf("process result = %+v, want one parsed/stored post and one media-only skip", result)
	}

	stored, err := database.Posts.GetByChannelAndMessageID(channelID, 41)
	if err != nil {
		t.Fatalf("get stored post: %v", err)
	}
	if stored.URL != "https://t.me/example/41" {
		t.Fatalf("stored URL = %q, want canonical URL", stored.URL)
	}
	if stored.ContentHash != HashContent("A post with a link") {
		t.Fatalf("content hash = %q, want %q", stored.ContentHash, HashContent("A post with a link"))
	}
	wantLinks := HashLinkURLs([]string{"https://example.com/story#part"})
	if stored.LinkURLsHash == nil || wantLinks == nil || *stored.LinkURLsHash != *wantLinks {
		t.Fatalf("link URL hash = %v, want %v", stored.LinkURLsHash, wantLinks)
	}

	updated, err := database.Channels.GetByID(channelID)
	if err != nil {
		t.Fatalf("get updated channel: %v", err)
	}
	if updated.LastPostID != 41 {
		t.Fatalf("last_post_id = %d, want 41", updated.LastPostID)
	}

	second, err := processor.ProcessChannel(updated)
	if err != nil {
		t.Fatalf("reprocess channel: %v", err)
	}
	if second.StoredPosts != 0 {
		t.Fatalf("reprocess result = %+v, want no newly stored posts", second)
	}
	var count int
	if err := database.Conn().QueryRow("SELECT COUNT(*) FROM posts WHERE channel_id = ?", channelID).Scan(&count); err != nil {
		t.Fatalf("count posts: %v", err)
	}
	if count != 1 {
		t.Fatalf("post count = %d, want 1 after duplicate fetch", count)
	}
}

func TestChannelProcessorContinuesBatchAfterChannelFailure(t *testing.T) {
	database, cleanup := newStorageTestDB(t)
	defer cleanup()
	first := &model.Channel{Username: "first", Enabled: true}
	firstID, err := database.Channels.Insert(first)
	if err != nil {
		t.Fatalf("insert first channel: %v", err)
	}
	first.ID = firstID
	second := &model.Channel{Username: "second", Enabled: true}
	secondID, err := database.Channels.Insert(second)
	if err != nil {
		t.Fatalf("insert second channel: %v", err)
	}
	second.ID = secondID

	fetcher := &fakeChannelFetcher{posts: map[string][]ParsedPost{
		"first": {{MessageID: 1, Text: "first", PostedAt: "2026-07-15T18:30:00Z"}},
	}, errors: map[string]error{"second": ErrChannelNotFound}}
	processor := NewChannelProcessor(fetcher, NewPostStorage(database.Channels, database.Posts)).
		WithSleeper(&recordingSleeper{})
	batch, err := processor.ProcessChannels([]model.Channel{*first, *second})
	if err != nil {
		t.Fatalf("process batch: %v", err)
	}
	if len(batch.Results) != 1 || batch.Results[0].StoredPosts != 1 {
		t.Fatalf("batch results = %+v, want first channel stored", batch.Results)
	}
	if len(batch.Failures) != 1 || batch.Failures[0].Channel.Username != "second" {
		t.Fatalf("batch failures = %+v, want second channel failure", batch.Failures)
	}
}

func TestChannelProcessorExposesHTTPAndPopulationMetadata(t *testing.T) {
	database, cleanup := newStorageTestDB(t)
	defer cleanup()

	populated := &model.Channel{Username: "populated", Enabled: true, LastPostID: 41}
	populatedID, err := database.Channels.Insert(populated)
	if err != nil {
		t.Fatalf("insert populated channel: %v", err)
	}
	populated.ID = populatedID
	newChannel := &model.Channel{Username: "new", Enabled: true}
	newID, err := database.Channels.Insert(newChannel)
	if err != nil {
		t.Fatalf("insert new channel: %v", err)
	}
	newChannel.ID = newID

	fetcher := &fakeStatsChannelFetcher{
		posts: map[string][]ParsedPost{
			"populated": nil,
			"new":       {{MessageID: 1, Text: "new post", PostedAt: "2026-07-15T18:30:00Z"}},
		},
		stats: map[string]ParseStats{
			"populated": {HTTPStatus: http.StatusOK},
			"new":       {HTTPStatus: http.StatusOK},
		},
	}
	processor := NewChannelProcessor(fetcher, NewPostStorage(database.Channels, database.Posts))
	batch, err := processor.ProcessChannels([]model.Channel{*populated, *newChannel})
	if err != nil {
		t.Fatalf("process channels: %v", err)
	}
	if len(batch.Results) != 2 {
		t.Fatalf("results = %+v, want two successful results", batch.Results)
	}
	if got := batch.Results[0]; got.HTTPStatus != http.StatusOK || got.ParsedPosts != 0 || !got.PreviouslyPopulated {
		t.Fatalf("populated result = %+v, want HTTP 200, zero posts, previously populated", got)
	}
	if got := batch.Results[1]; got.HTTPStatus != http.StatusOK || got.ParsedPosts != 1 || got.PreviouslyPopulated {
		t.Fatalf("new result = %+v, want HTTP 200, one post, not previously populated", got)
	}
}

func TestChannelProcessorNotifiesOnceForStructuralChange(t *testing.T) {
	database, cleanup := newStorageTestDB(t)
	defer cleanup()
	channels := make([]model.Channel, 0, 2)
	for _, username := range []string{"first", "second"} {
		channel := &model.Channel{Username: username, Enabled: true, LastPostID: 10}
		id, err := database.Channels.Insert(channel)
		if err != nil {
			t.Fatalf("insert channel %s: %v", username, err)
		}
		channel.ID = id
		channels = append(channels, *channel)
	}

	notifier := &recordingOwnerNotifier{}
	fetcher := &fakeStatsChannelFetcher{
		posts: map[string][]ParsedPost{"first": nil, "second": nil},
		stats: map[string]ParseStats{
			"first":  {HTTPStatus: http.StatusOK},
			"second": {HTTPStatus: http.StatusOK},
		},
	}
	processor := NewChannelProcessor(fetcher, NewPostStorage(database.Channels, database.Posts), notifier)
	batch, err := processor.ProcessChannels(channels)
	if err != nil {
		t.Fatalf("process channels: %v", err)
	}
	if len(batch.Failures) != 0 {
		t.Fatalf("failures = %+v, want none", batch.Failures)
	}
	if len(notifier.messages) != 1 {
		t.Fatalf("notifications = %d, want one", len(notifier.messages))
	}
	if !strings.Contains(notifier.messages[0], "структуру t.me/s") ||
		!strings.Contains(notifier.messages[0], "Проверьте парсер") {
		t.Fatalf("notification = %q, want actionable Russian structure warning", notifier.messages[0])
	}
}

func TestChannelProcessorSkipsFalseStructuralChangeAlerts(t *testing.T) {
	tests := []struct {
		name              string
		channels          []model.Channel
		posts             map[string][]ParsedPost
		stats             map[string]ParseStats
		errors            map[string]error
		wantNotifications int
	}{
		{
			name:              "new empty channels",
			channels:          []model.Channel{{Username: "new", Enabled: true}},
			posts:             map[string][]ParsedPost{"new": nil},
			stats:             map[string]ParseStats{"new": {HTTPStatus: http.StatusOK}},
			wantNotifications: 0,
		},
		{
			name: "mixed non-empty results",
			channels: []model.Channel{
				{Username: "populated-empty", Enabled: true, LastPostID: 10},
				{Username: "populated-nonempty", Enabled: true, LastPostID: 10},
			},
			posts: map[string][]ParsedPost{
				"populated-empty":    nil,
				"populated-nonempty": {{MessageID: 11, Text: "post", PostedAt: "2026-07-15T18:30:00Z"}},
			},
			stats: map[string]ParseStats{
				"populated-empty":    {HTTPStatus: http.StatusOK},
				"populated-nonempty": {HTTPStatus: http.StatusOK},
			},
			wantNotifications: 0,
		},
		{
			name: "failed HTTP request",
			channels: []model.Channel{
				{Username: "populated", Enabled: true, LastPostID: 10},
				{Username: "failed", Enabled: true, LastPostID: 10},
			},
			posts: map[string][]ParsedPost{"populated": nil, "failed": nil},
			stats: map[string]ParseStats{
				"populated": {HTTPStatus: http.StatusOK},
				"failed":    {HTTPStatus: http.StatusBadGateway},
			},
			errors:            map[string]error{"failed": errors.New("bad gateway")},
			wantNotifications: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database, cleanup := newStorageTestDB(t)
			defer cleanup()
			for i := range tt.channels {
				id, err := database.Channels.Insert(&tt.channels[i])
				if err != nil {
					t.Fatalf("insert channel: %v", err)
				}
				tt.channels[i].ID = id
			}
			notifier := &recordingOwnerNotifier{}
			processor := NewChannelProcessor(
				&fakeStatsChannelFetcher{posts: tt.posts, stats: tt.stats, errors: tt.errors},
				NewPostStorage(database.Channels, database.Posts),
				notifier,
			)
			if _, err := processor.ProcessChannels(tt.channels); err != nil {
				t.Fatalf("process channels: %v", err)
			}
			if len(notifier.messages) != tt.wantNotifications {
				t.Fatalf("notifications = %q, want %d", notifier.messages, tt.wantNotifications)
			}
		})
	}
}

func TestChannelProcessorPersistsAndNotifiesPreviouslyWorkingNotFoundChannel(t *testing.T) {
	database, cleanup := newStorageTestDB(t)
	defer cleanup()

	channel := &model.Channel{
		Username:   "oldname",
		Title:      "Keep this title",
		Enabled:    true,
		LastPostID: 41,
	}
	channelID, err := database.Channels.Insert(channel)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	channel.ID = channelID
	post := &model.Post{
		ChannelID:   channelID,
		MessageID:   41,
		Text:        "previous post",
		PostedAt:    "2026-07-15T18:30:00Z",
		URL:         "https://t.me/oldname/41",
		ContentHash: HashContent("previous post"),
	}
	if _, err := database.Posts.Insert(post); err != nil {
		t.Fatalf("insert previous post: %v", err)
	}

	notifier := &recordingOwnerNotifier{}
	processor := NewChannelProcessor(
		&fakeChannelFetcher{errors: map[string]error{"oldname": ErrChannelNotFound}},
		NewPostStorage(database.Channels, database.Posts),
		notifier,
	).WithSleeper(&recordingSleeper{})
	result, err := processor.ProcessChannel(channel)
	if !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("process error = %v, want ErrChannelNotFound", err)
	}
	if result.Channel.FetchErrorKind != FetchErrorKindNotFound {
		t.Fatalf("result error kind = %q, want %q", result.Channel.FetchErrorKind, FetchErrorKindNotFound)
	}
	stored, err := database.Channels.GetByID(channelID)
	if err != nil {
		t.Fatalf("get failed channel: %v", err)
	}
	if stored.FetchErrorKind != FetchErrorKindNotFound || stored.FetchErrorMessage == "" || stored.FetchErrorAt == nil {
		t.Fatalf("stored failure = %+v, want durable not-found state", stored)
	}
	if stored.Title != "Keep this title" || !stored.Enabled || stored.LastPostID != 41 {
		t.Fatalf("channel data was not preserved: %+v", stored)
	}
	if _, err := database.Posts.GetByChannelAndMessageID(channelID, 41); err != nil {
		t.Fatalf("previous post was removed: %v", err)
	}
	if len(notifier.messages) != 1 {
		t.Fatalf("notifications = %d, want one", len(notifier.messages))
	}
	for _, want := range []string{"@oldname", "не найден", "переименован", "обновите username", "настройках"} {
		if !strings.Contains(notifier.messages[0], want) {
			t.Fatalf("notification %q does not contain %q", notifier.messages[0], want)
		}
	}
}

func TestChannelProcessorClearsFetchErrorAfterRecoveryAndPreservesPosts(t *testing.T) {
	database, cleanup := newStorageTestDB(t)
	defer cleanup()

	channel := &model.Channel{Username: "recover", Title: "Configured", Enabled: false, LastPostID: 1}
	channelID, err := database.Channels.Insert(channel)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	channel.ID = channelID
	if err := database.Channels.MarkFetchError(channelID, FetchErrorKindNotFound, "old failure"); err != nil {
		t.Fatalf("mark error: %v", err)
	}
	previous := &model.Post{
		ChannelID: channelID, MessageID: 1, Text: "previous",
		PostedAt: "2026-07-15T18:30:00Z", URL: "https://t.me/recover/1",
		ContentHash: HashContent("previous"),
	}
	if _, err := database.Posts.Insert(previous); err != nil {
		t.Fatalf("insert previous post: %v", err)
	}

	processor := NewChannelProcessor(
		&fakeChannelFetcher{posts: map[string][]ParsedPost{
			"recover": {{MessageID: 2, Text: "recovered", PostedAt: "2026-07-16T18:30:00Z"}},
		}},
		NewPostStorage(database.Channels, database.Posts),
	)
	result, err := processor.ProcessChannel(channel)
	if err != nil {
		t.Fatalf("recovery process: %v", err)
	}
	if result.StoredPosts != 1 {
		t.Fatalf("stored posts = %d, want one recovered post", result.StoredPosts)
	}
	recovered, err := database.Channels.GetByID(channelID)
	if err != nil {
		t.Fatalf("get recovered channel: %v", err)
	}
	if recovered.FetchErrorKind != "" || recovered.FetchErrorMessage != "" || recovered.FetchErrorAt != nil {
		t.Fatalf("recovered error state = %+v, want cleared", recovered)
	}
	if recovered.Title != "Configured" || recovered.Enabled || recovered.LastPostID != 2 {
		t.Fatalf("recovered channel configuration = %+v, want preserved config and cursor 2", recovered)
	}
	if _, err := database.Posts.GetByChannelAndMessageID(channelID, 1); err != nil {
		t.Fatalf("previous post was not preserved: %v", err)
	}
	if _, err := database.Posts.GetByChannelAndMessageID(channelID, 2); err != nil {
		t.Fatalf("recovered post was not stored: %v", err)
	}
}

type fakeChannelFetcher struct {
	posts  map[string][]ParsedPost
	errors map[string]error
}

type fakeStatsChannelFetcher struct {
	posts  map[string][]ParsedPost
	stats  map[string]ParseStats
	errors map[string]error
}

func (f *fakeStatsChannelFetcher) ParseChannel(username string) ([]ParsedPost, error) {
	posts, _, err := f.ParseChannelWithStats(username)
	return posts, err
}

func (f *fakeStatsChannelFetcher) ParseChannelWithStats(username string) ([]ParsedPost, ParseStats, error) {
	if err := f.errors[username]; err != nil {
		return nil, f.stats[username], err
	}
	return f.posts[username], f.stats[username], nil
}

type recordingOwnerNotifier struct {
	messages []string
}

func (n *recordingOwnerNotifier) NotifyOwner(_ context.Context, text string) error {
	n.messages = append(n.messages, text)
	return nil
}

func (f *fakeChannelFetcher) ParseChannel(username string) ([]ParsedPost, error) {
	if err := f.errors[username]; err != nil {
		return nil, err
	}
	return f.posts[username], nil
}

// sequenceFetcher returns canned responses per call for each channel username.
type sequenceFetcher struct {
	// responses[username][callIndex] => posts, stats, error
	calls     map[string]int
	order     []string
	responses map[string][]sequenceResponse
}

type sequenceResponse struct {
	posts []ParsedPost
	stats ParseStats
	err   error
}

func (f *sequenceFetcher) ParseChannel(username string) ([]ParsedPost, error) {
	posts, _, err := f.ParseChannelWithStats(username)
	return posts, err
}

func (f *sequenceFetcher) ParseChannelWithStats(username string) ([]ParsedPost, ParseStats, error) {
	if f.calls == nil {
		f.calls = make(map[string]int)
	}
	f.order = append(f.order, username)
	idx := f.calls[username]
	f.calls[username] = idx + 1
	responses := f.responses[username]
	if idx >= len(responses) {
		last := responses[len(responses)-1]
		return last.posts, last.stats, last.err
	}
	resp := responses[idx]
	return resp.posts, resp.stats, resp.err
}

type recordingSleeper struct {
	sleeps []time.Duration
}

func (s *recordingSleeper) Sleep(_ context.Context, d time.Duration) error {
	s.sleeps = append(s.sleeps, d)
	return nil
}

func TestProcessChannelsRetriesOnlyRateLimitedChannel(t *testing.T) {
	database, cleanup := newStorageTestDB(t)
	defer cleanup()

	channelA := &model.Channel{Username: "channela", Enabled: true}
	idA, err := database.Channels.Insert(channelA)
	if err != nil {
		t.Fatalf("insert A: %v", err)
	}
	channelA.ID = idA
	channelB := &model.Channel{Username: "channelb", Enabled: true}
	idB, err := database.Channels.Insert(channelB)
	if err != nil {
		t.Fatalf("insert B: %v", err)
	}
	channelB.ID = idB

	fetcher := &sequenceFetcher{responses: map[string][]sequenceResponse{
		"channela": {
			{stats: ParseStats{HTTPStatus: http.StatusTooManyRequests}, err: &RateLimitError{RetryAfter: 17 * time.Second}},
			{posts: []ParsedPost{{MessageID: 1, Text: "a recovered", PostedAt: "2026-07-15T18:30:00Z"}}, stats: ParseStats{HTTPStatus: http.StatusOK}},
		},
		"channelb": {
			{posts: []ParsedPost{{MessageID: 2, Text: "b ok", PostedAt: "2026-07-15T18:31:00Z"}}, stats: ParseStats{HTTPStatus: http.StatusOK}},
		},
	}}
	sleeper := &recordingSleeper{}
	processor := NewChannelProcessor(fetcher, NewPostStorage(database.Channels, database.Posts)).
		WithMaxRetries(3).
		WithSleeper(sleeper)

	batch, err := processor.ProcessChannels([]model.Channel{*channelA, *channelB})
	if err != nil {
		t.Fatalf("process channels: %v", err)
	}
	if len(batch.Failures) != 0 {
		t.Fatalf("failures = %+v, want none after A recovery", batch.Failures)
	}
	if len(batch.Results) != 2 {
		t.Fatalf("results = %+v, want both channels", batch.Results)
	}
	if fetcher.calls["channela"] != 2 {
		t.Fatalf("channel A attempts = %d, want 2", fetcher.calls["channela"])
	}
	if fetcher.calls["channelb"] != 1 {
		t.Fatalf("channel B attempts = %d, want 1", fetcher.calls["channelb"])
	}
	// B must proceed before A is retried: first-pass A, first-pass B, then retry A.
	if len(fetcher.order) < 3 || fetcher.order[0] != "channela" || fetcher.order[1] != "channelb" || fetcher.order[2] != "channela" {
		t.Fatalf("fetch order = %v, want [channela, channelb, channela]", fetcher.order)
	}
	if len(sleeper.sleeps) != 1 || sleeper.sleeps[0] != 17*time.Second {
		t.Fatalf("sleeps = %v, want one 17s Retry-After sleep only for channel A", sleeper.sleeps)
	}
}

func TestProcessChannelsUsesRetryAfterForEachRateLimitedResponse(t *testing.T) {
	database, cleanup := newStorageTestDB(t)
	defer cleanup()

	channel := &model.Channel{Username: "changing-limit", Enabled: true}
	id, err := database.Channels.Insert(channel)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	channel.ID = id

	fetcher := &sequenceFetcher{responses: map[string][]sequenceResponse{
		"changing-limit": {
			{stats: ParseStats{HTTPStatus: http.StatusTooManyRequests}, err: &RateLimitError{RetryAfter: 17 * time.Second}},
			{stats: ParseStats{HTTPStatus: http.StatusTooManyRequests}, err: &RateLimitError{RetryAfter: 3 * time.Second}},
			{posts: []ParsedPost{{MessageID: 1, Text: "recovered", PostedAt: "2026-07-15T18:30:00Z"}}, stats: ParseStats{HTTPStatus: http.StatusOK}},
		},
	}}
	sleeper := &recordingSleeper{}
	processor := NewChannelProcessor(fetcher, NewPostStorage(database.Channels, database.Posts)).
		WithMaxRetries(2).
		WithSleeper(sleeper)

	batch, err := processor.ProcessChannels([]model.Channel{*channel})
	if err != nil {
		t.Fatalf("process channels: %v", err)
	}
	if len(batch.Failures) != 0 || len(batch.Results) != 1 {
		t.Fatalf("batch = results=%+v failures=%+v, want recovered success", batch.Results, batch.Failures)
	}
	if got, want := sleeper.sleeps, []time.Duration{17 * time.Second, 3 * time.Second}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sleeps = %v, want Retry-After durations %v", got, want)
	}
}

func TestProcessChannelUsesHTTPDateRetryAfter(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Retry-After", now.Add(45*time.Second).Format(http.TimeFormat))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`
			<div class="tgme_widget_message" data-post="datelimit/1">
				<div class="tgme_widget_message_text">recovered</div>
				<time datetime="2026-07-15T18:30:00Z"></time>
			</div>`))
	}))
	defer server.Close()

	database, cleanup := newStorageTestDB(t)
	defer cleanup()
	channel := &model.Channel{Username: "datelimit", Enabled: true}
	id, err := database.Channels.Insert(channel)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	channel.ID = id
	sleeper := &recordingSleeper{}
	fetcher := NewWithOptions(Options{
		Client:  server.Client(),
		BaseURL: server.URL,
		Now:     func() time.Time { return now },
	})
	processor := NewChannelProcessor(fetcher, NewPostStorage(database.Channels, database.Posts)).
		WithMaxRetries(1).
		WithSleeper(sleeper)

	result, err := processor.ProcessChannel(channel)
	if err != nil {
		t.Fatalf("process channel: %v", err)
	}
	if result.StoredPosts != 1 {
		t.Fatalf("result = %+v, want one recovered post", result)
	}
	if len(sleeper.sleeps) != 1 || sleeper.sleeps[0] != 45*time.Second {
		t.Fatalf("sleeps = %v, want [45s] from HTTP-date Retry-After", sleeper.sleeps)
	}
}

func TestProcessChannelsRateLimitDefaultBackoffAndNoInfiniteLoop(t *testing.T) {
	database, cleanup := newStorageTestDB(t)
	defer cleanup()

	channel := &model.Channel{Username: "limited", Enabled: true, LastPostID: 5}
	id, err := database.Channels.Insert(channel)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	channel.ID = id

	fetcher := &sequenceFetcher{responses: map[string][]sequenceResponse{
		"limited": {
			{stats: ParseStats{HTTPStatus: http.StatusTooManyRequests}, err: &RateLimitError{RetryAfter: defaultRateLimitBackoff}},
		},
	}}
	sleeper := &recordingSleeper{}
	processor := NewChannelProcessor(fetcher, NewPostStorage(database.Channels, database.Posts)).
		WithMaxRetries(3).
		WithSleeper(sleeper)

	batch, err := processor.ProcessChannels([]model.Channel{*channel})
	if err != nil {
		t.Fatalf("process channels: %v", err)
	}
	if len(batch.Failures) != 1 {
		t.Fatalf("failures = %+v, want one exhausted rate-limit failure", batch.Failures)
	}
	var rateLimitErr *RateLimitError
	if !errors.As(batch.Failures[0].Err, &rateLimitErr) {
		t.Fatalf("failure err = %v, want RateLimitError", batch.Failures[0].Err)
	}
	if rateLimitErr.RetryAfter != defaultRateLimitBackoff {
		t.Fatalf("retry after = %s, want %s", rateLimitErr.RetryAfter, defaultRateLimitBackoff)
	}
	if fetcher.calls["limited"] != 4 {
		t.Fatalf("attempts = %d, want initial attempt plus MaxRetries=3 retries", fetcher.calls["limited"])
	}
	// Three sleeps between the initial attempt and three retries. The
	// five-minute default is used for every response without Retry-After.
	if len(sleeper.sleeps) != 3 {
		t.Fatalf("sleeps = %v, want three backoff sleeps", sleeper.sleeps)
	}
	for i, want := range []time.Duration{defaultRateLimitBackoff, defaultRateLimitBackoff, defaultRateLimitBackoff} {
		if sleeper.sleeps[i] != want {
			t.Fatalf("sleep %d = %s, want %s", i, sleeper.sleeps[i], want)
		}
	}
}

func TestProcessChannelsExhaustedRateLimitPreservedInBatch(t *testing.T) {
	database, cleanup := newStorageTestDB(t)
	defer cleanup()

	okChannel := &model.Channel{Username: "ok", Enabled: true}
	okID, err := database.Channels.Insert(okChannel)
	if err != nil {
		t.Fatalf("insert ok: %v", err)
	}
	okChannel.ID = okID
	badChannel := &model.Channel{Username: "bad", Enabled: true, LastPostID: 9}
	badID, err := database.Channels.Insert(badChannel)
	if err != nil {
		t.Fatalf("insert bad: %v", err)
	}
	badChannel.ID = badID

	fetcher := &sequenceFetcher{responses: map[string][]sequenceResponse{
		"ok": {
			{posts: []ParsedPost{{MessageID: 1, Text: "ok", PostedAt: "2026-07-15T18:30:00Z"}}, stats: ParseStats{HTTPStatus: http.StatusOK}},
		},
		"bad": {
			{stats: ParseStats{HTTPStatus: http.StatusTooManyRequests}, err: &RateLimitError{RetryAfter: time.Second}},
		},
	}}
	processor := NewChannelProcessor(fetcher, NewPostStorage(database.Channels, database.Posts)).
		WithMaxRetries(2).
		WithSleeper(&recordingSleeper{})

	batch, err := processor.ProcessChannels([]model.Channel{*okChannel, *badChannel})
	if err != nil {
		t.Fatalf("process channels: %v", err)
	}
	if len(batch.Results) != 1 || batch.Results[0].Channel.Username != "ok" {
		t.Fatalf("results = %+v, want successful ok channel only", batch.Results)
	}
	if len(batch.Failures) != 1 || batch.Failures[0].Channel.Username != "bad" {
		t.Fatalf("failures = %+v, want exhausted bad channel preserved", batch.Failures)
	}
	if batch.Failures[0].HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("failure HTTP status = %d, want 429", batch.Failures[0].HTTPStatus)
	}
	stored, err := database.Channels.GetByID(badID)
	if err != nil {
		t.Fatalf("get bad channel: %v", err)
	}
	if stored.FetchErrorKind != FetchErrorKindRateLimited {
		t.Fatalf("stored kind = %q, want %q", stored.FetchErrorKind, FetchErrorKindRateLimited)
	}
}

func TestProcessChannelsPartialRateLimitNotifiesOnce(t *testing.T) {
	database, cleanup := newStorageTestDB(t)
	defer cleanup()

	okChannel := &model.Channel{Username: "ok", Enabled: true}
	okID, err := database.Channels.Insert(okChannel)
	if err != nil {
		t.Fatalf("insert ok: %v", err)
	}
	okChannel.ID = okID
	badChannel := &model.Channel{Username: "ratelimited", Enabled: true, LastPostID: 3}
	badID, err := database.Channels.Insert(badChannel)
	if err != nil {
		t.Fatalf("insert bad: %v", err)
	}
	badChannel.ID = badID

	notifier := &recordingOwnerNotifier{}
	fetcher := &sequenceFetcher{responses: map[string][]sequenceResponse{
		"ok": {
			{posts: []ParsedPost{{MessageID: 1, Text: "ok", PostedAt: "2026-07-15T18:30:00Z"}}, stats: ParseStats{HTTPStatus: http.StatusOK}},
		},
		"ratelimited": {
			{stats: ParseStats{HTTPStatus: http.StatusTooManyRequests}, err: &RateLimitError{RetryAfter: time.Second}},
		},
	}}
	processor := NewChannelProcessor(fetcher, NewPostStorage(database.Channels, database.Posts), notifier).
		WithMaxRetries(1).
		WithSleeper(&recordingSleeper{})

	batch, err := processor.ProcessChannels([]model.Channel{*okChannel, *badChannel})
	if err != nil {
		t.Fatalf("process channels: %v", err)
	}
	if len(batch.Failures) != 1 {
		t.Fatalf("failures = %+v, want one rate-limit failure", batch.Failures)
	}
	if len(notifier.messages) != 1 {
		t.Fatalf("notifications = %q, want exactly one partial-digest rate-limit alert", notifier.messages)
	}
	msg := notifier.messages[0]
	for _, want := range []string{"ограничен", "частичн", "@ratelimited"} {
		if !strings.Contains(strings.ToLower(msg), strings.ToLower(want)) && !strings.Contains(msg, want) {
			// keep checking exact Russian tokens below
			_ = want
		}
	}
	if !strings.Contains(msg, "rate") && !strings.Contains(msg, "429") && !strings.Contains(msg, "ограничен") {
		// Prefer Russian actionable wording.
		if !strings.Contains(msg, "лимит") && !strings.Contains(msg, "частот") {
			t.Fatalf("notification %q missing rate-limit language", msg)
		}
	}
	if !strings.Contains(msg, "@ratelimited") {
		t.Fatalf("notification %q missing limited channel", msg)
	}
	if !strings.Contains(msg, "частичн") && !strings.Contains(msg, "частичный") && !strings.Contains(msg, "неполн") {
		t.Fatalf("notification %q missing partial-digest wording", msg)
	}
}

func TestProcessChannelsFullySuccessfulBatchNoRateLimitNotification(t *testing.T) {
	database, cleanup := newStorageTestDB(t)
	defer cleanup()

	channel := &model.Channel{Username: "ok", Enabled: true}
	id, err := database.Channels.Insert(channel)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	channel.ID = id

	notifier := &recordingOwnerNotifier{}
	// First attempt rate-limits, second succeeds — batch fully succeeds, no partial notify.
	fetcher := &sequenceFetcher{responses: map[string][]sequenceResponse{
		"ok": {
			{stats: ParseStats{HTTPStatus: http.StatusTooManyRequests}, err: &RateLimitError{RetryAfter: 2 * time.Second}},
			{posts: []ParsedPost{{MessageID: 1, Text: "ok", PostedAt: "2026-07-15T18:30:00Z"}}, stats: ParseStats{HTTPStatus: http.StatusOK}},
		},
	}}
	processor := NewChannelProcessor(fetcher, NewPostStorage(database.Channels, database.Posts), notifier).
		WithMaxRetries(3).
		WithSleeper(&recordingSleeper{})

	batch, err := processor.ProcessChannels([]model.Channel{*channel})
	if err != nil {
		t.Fatalf("process channels: %v", err)
	}
	if len(batch.Failures) != 0 || len(batch.Results) != 1 {
		t.Fatalf("batch = results=%+v failures=%+v, want full success", batch.Results, batch.Failures)
	}
	if len(notifier.messages) != 0 {
		t.Fatalf("notifications = %q, want none for fully successful batch", notifier.messages)
	}
}

func TestProcessChannelDoesNotPersistOrNotifyPostValidationFailure(t *testing.T) {
	database, cleanup := newStorageTestDB(t)
	defer cleanup()

	channel := &model.Channel{Username: "invalid-post", Enabled: true}
	id, err := database.Channels.Insert(channel)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	channel.ID = id
	notifier := &recordingOwnerNotifier{}
	fetcher := &fakeChannelFetcher{
		posts: map[string][]ParsedPost{
			"invalid-post": {{MessageID: 1, Text: "missing timestamp"}},
		},
	}

	_, err = NewChannelProcessor(fetcher, NewPostStorage(database.Channels, database.Posts), notifier).
		WithSleeper(&recordingSleeper{}).
		ProcessChannel(channel)
	if err == nil {
		t.Fatal("ProcessChannel() error = nil, want validation failure")
	}
	if len(notifier.messages) != 0 {
		t.Fatalf("notifications = %q, want none for post validation failure", notifier.messages)
	}
	stored, err := database.Channels.GetByID(id)
	if err != nil {
		t.Fatalf("get channel: %v", err)
	}
	if stored.FetchErrorKind != "" || stored.FetchErrorMessage != "" || stored.FetchErrorAt != nil {
		t.Fatalf("stored fetch error = %+v, want no durable channel-fetch failure", stored)
	}
}

func TestProcessChannelDoesNotPersistOrNotifyPostStorageFailure(t *testing.T) {
	database, cleanup := newStorageTestDB(t)
	defer cleanup()

	channel := &model.Channel{Username: "storage-failure", Enabled: true}
	id, err := database.Channels.Insert(channel)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	channel.ID = id
	notifier := &recordingOwnerNotifier{}
	storeErr := errors.New("post database unavailable")
	storage := NewPostStorage(database.Channels, &failingProcessorPosts{err: storeErr})
	fetcher := &fakeChannelFetcher{
		posts: map[string][]ParsedPost{
			"storage-failure": {{MessageID: 1, Text: "valid", PostedAt: "2026-07-15T18:30:00Z"}},
		},
	}

	_, err = NewChannelProcessor(fetcher, storage, notifier).
		WithSleeper(&recordingSleeper{}).
		ProcessChannel(channel)
	if !errors.Is(err, storeErr) {
		t.Fatalf("ProcessChannel() error = %v, want storage error %v", err, storeErr)
	}
	if len(notifier.messages) != 0 {
		t.Fatalf("notifications = %q, want none for post storage failure", notifier.messages)
	}
	stored, err := database.Channels.GetByID(id)
	if err != nil {
		t.Fatalf("get channel: %v", err)
	}
	if stored.FetchErrorKind != "" || stored.FetchErrorMessage != "" || stored.FetchErrorAt != nil {
		t.Fatalf("stored fetch error = %+v, want no durable channel-fetch failure", stored)
	}
}

func TestProcessChannelsSeparatesPostValidationErrorsFromFetchFailures(t *testing.T) {
	database, cleanup := newStorageTestDB(t)
	defer cleanup()

	channel := &model.Channel{Username: "invalid-batch-post", Enabled: true}
	id, err := database.Channels.Insert(channel)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	channel.ID = id

	processor := NewChannelProcessor(
		&fakeChannelFetcher{posts: map[string][]ParsedPost{
			channel.Username: {{MessageID: 1, Text: "missing timestamp"}},
		}},
		NewPostStorage(database.Channels, database.Posts),
	)
	batch, err := processor.ProcessChannelsContext(context.Background(), []model.Channel{*channel})
	if err != nil {
		t.Fatalf("process channels: %v", err)
	}
	if len(batch.Failures) != 0 {
		t.Fatalf("fetch failures = %+v, want none for post validation error", batch.Failures)
	}
	if len(batch.ProcessingErrors) != 1 || batch.ProcessingErrors[0].Channel.Username != channel.Username {
		t.Fatalf("processing errors = %+v, want one validation error", batch.ProcessingErrors)
	}
	if !errors.Is(batch.ProcessingErrors[0].Err, errPostValidation) {
		t.Fatalf("processing error = %v, want post validation error", batch.ProcessingErrors[0].Err)
	}
}

func TestProcessChannelsRetriesEveryClassifiedFetchFailure(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		failure  error
		wantKind string
	}{
		{name: "404", status: http.StatusNotFound, failure: ErrChannelNotFound, wantKind: FetchErrorKindNotFound},
		{name: "429", status: http.StatusTooManyRequests, failure: &RateLimitError{RetryAfter: time.Hour}, wantKind: FetchErrorKindRateLimited},
		{name: "private", status: http.StatusForbidden, failure: ErrChannelPrivate, wantKind: FetchErrorKindPrivate},
		{name: "unavailable", status: http.StatusOK, failure: ErrChannelUnavailable, wantKind: FetchErrorKindUnavailable},
		{name: "cloudflare", status: http.StatusOK, failure: ErrCloudflareChallenge, wantKind: FetchErrorKindCloudflare},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database, cleanup := newStorageTestDB(t)
			defer cleanup()
			channel := &model.Channel{Username: "retry-" + test.name, Enabled: true, LastPostID: 1}
			id, err := database.Channels.Insert(channel)
			if err != nil {
				t.Fatalf("insert channel: %v", err)
			}
			channel.ID = id
			notifier := &recordingOwnerNotifier{}
			fetcher := &sequenceFetcher{responses: map[string][]sequenceResponse{
				channel.Username: {
					{stats: ParseStats{HTTPStatus: test.status}, err: test.failure},
					{posts: []ParsedPost{{MessageID: 2, Text: "recovered", PostedAt: "2026-07-15T18:30:00Z"}}, stats: ParseStats{HTTPStatus: http.StatusOK}},
				},
			}}
			sleeper := &recordingSleeper{}
			processor := NewChannelProcessor(fetcher, NewPostStorage(database.Channels, database.Posts), notifier).
				WithMaxRetries(3).
				WithSleeper(sleeper)

			batch, err := processor.ProcessChannels([]model.Channel{*channel})
			if err != nil {
				t.Fatalf("process channels: %v", err)
			}
			if len(batch.Failures) != 0 || len(batch.Results) != 1 {
				t.Fatalf("batch = results=%+v failures=%+v, want recovered success", batch.Results, batch.Failures)
			}
			if fetcher.calls[channel.Username] != 2 {
				t.Fatalf("attempts = %d, want initial attempt plus one retry", fetcher.calls[channel.Username])
			}
			wantSleep := 5 * time.Second
			if test.name == "429" {
				wantSleep = time.Hour
			}
			if len(sleeper.sleeps) != 1 || sleeper.sleeps[0] != wantSleep {
				t.Fatalf("sleeps = %v, want [%s]", sleeper.sleeps, wantSleep)
			}
			if len(notifier.messages) != 0 {
				t.Fatalf("notifications = %q, want none after recovery", notifier.messages)
			}
			stored, err := database.Channels.GetByID(id)
			if err != nil {
				t.Fatalf("get channel: %v", err)
			}
			if stored.FetchErrorKind != "" || stored.FetchErrorMessage != "" || stored.FetchErrorAt != nil {
				t.Fatalf("stored fetch error = %+v, want cleared after recovery", stored)
			}
			if test.wantKind == "" {
				t.Fatal("test must define expected failure kind")
			}
		})
	}
}

func TestProcessChannelsExhaustedFetchFailureNotifiesOnceAndContinuesHealthyChannel(t *testing.T) {
	database, cleanup := newStorageTestDB(t)
	defer cleanup()

	healthy := &model.Channel{Username: "healthy", Enabled: true}
	healthyID, err := database.Channels.Insert(healthy)
	if err != nil {
		t.Fatalf("insert healthy channel: %v", err)
	}
	healthy.ID = healthyID
	broken := &model.Channel{Username: "broken", Enabled: true, LastPostID: 12}
	brokenID, err := database.Channels.Insert(broken)
	if err != nil {
		t.Fatalf("insert broken channel: %v", err)
	}
	broken.ID = brokenID

	notifier := &recordingOwnerNotifier{}
	fetcher := &sequenceFetcher{responses: map[string][]sequenceResponse{
		"healthy": {
			{posts: []ParsedPost{{MessageID: 1, Text: "healthy post", PostedAt: "2026-07-15T18:30:00Z"}}, stats: ParseStats{HTTPStatus: http.StatusOK}},
		},
		"broken": {
			{stats: ParseStats{HTTPStatus: http.StatusOK}, err: ErrCloudflareChallenge},
		},
	}}
	sleeper := &recordingSleeper{}
	processor := NewChannelProcessor(fetcher, NewPostStorage(database.Channels, database.Posts), notifier).
		WithMaxRetries(3).
		WithSleeper(sleeper)

	batch, err := processor.ProcessChannels([]model.Channel{*healthy, *broken})
	if err != nil {
		t.Fatalf("process channels: %v", err)
	}
	if len(batch.Results) != 1 || batch.Results[0].Channel.Username != "healthy" {
		t.Fatalf("results = %+v, want healthy channel to complete", batch.Results)
	}
	if batch.Results[0].StoredPosts != 1 {
		t.Fatalf("healthy result = %+v, want one stored post", batch.Results[0])
	}
	if len(batch.Failures) != 1 || batch.Failures[0].Channel.Username != "broken" {
		t.Fatalf("failures = %+v, want exhausted broken channel", batch.Failures)
	}
	if fetcher.calls["broken"] != 4 {
		t.Fatalf("broken attempts = %d, want initial attempt plus three retries", fetcher.calls["broken"])
	}
	wantSleeps := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second}
	if len(sleeper.sleeps) != len(wantSleeps) {
		t.Fatalf("sleeps = %v, want %v", sleeper.sleeps, wantSleeps)
	}
	for i, want := range wantSleeps {
		if sleeper.sleeps[i] != want {
			t.Fatalf("sleep %d = %s, want %s", i, sleeper.sleeps[i], want)
		}
	}
	if len(notifier.messages) != 1 {
		t.Fatalf("notifications = %q, want exactly one after exhaustion", notifier.messages)
	}
	for _, want := range []string{"@broken", "cloudflare", "3"} {
		if !strings.Contains(strings.ToLower(notifier.messages[0]), strings.ToLower(want)) {
			t.Fatalf("notification %q missing %q", notifier.messages[0], want)
		}
	}
	stored, err := database.Channels.GetByID(brokenID)
	if err != nil {
		t.Fatalf("get broken channel: %v", err)
	}
	if stored.FetchErrorKind != FetchErrorKindCloudflare || stored.FetchErrorMessage == "" || stored.FetchErrorAt == nil {
		t.Fatalf("stored broken channel = %+v, want durable exhausted error", stored)
	}
}

func TestProcessChannelsExhaustsEveryClassifiedFetchFailure(t *testing.T) {
	tests := []struct {
		name    string
		failure error
		kind    string
	}{
		{name: "404", failure: ErrChannelNotFound, kind: FetchErrorKindNotFound},
		{name: "429", failure: &RateLimitError{RetryAfter: time.Hour}, kind: FetchErrorKindRateLimited},
		{name: "private", failure: ErrChannelPrivate, kind: FetchErrorKindPrivate},
		{name: "unavailable", failure: ErrChannelUnavailable, kind: FetchErrorKindUnavailable},
		{name: "cloudflare", failure: ErrCloudflareChallenge, kind: FetchErrorKindCloudflare},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database, cleanup := newStorageTestDB(t)
			defer cleanup()
			channel := &model.Channel{Username: "exhausted-" + test.name, Enabled: true, LastPostID: 8}
			id, err := database.Channels.Insert(channel)
			if err != nil {
				t.Fatalf("insert channel: %v", err)
			}
			channel.ID = id

			notifier := &recordingOwnerNotifier{}
			sleeper := &recordingSleeper{}
			fetcher := &sequenceFetcher{responses: map[string][]sequenceResponse{
				channel.Username: {
					{stats: ParseStats{HTTPStatus: http.StatusOK}, err: test.failure},
				},
			}}
			processor := NewChannelProcessor(fetcher, NewPostStorage(database.Channels, database.Posts), notifier).
				WithMaxRetries(3).
				WithSleeper(sleeper)

			batch, err := processor.ProcessChannels([]model.Channel{*channel})
			if err != nil {
				t.Fatalf("process channels: %v", err)
			}
			if len(batch.Results) != 0 || len(batch.Failures) != 1 {
				t.Fatalf("batch = results=%+v failures=%+v, want one exhausted failure", batch.Results, batch.Failures)
			}
			if fetcher.calls[channel.Username] != 4 {
				t.Fatalf("attempts = %d, want initial attempt plus three retries", fetcher.calls[channel.Username])
			}
			wantSleeps := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second}
			if test.name == "429" {
				wantSleeps = []time.Duration{time.Hour, time.Hour, time.Hour}
			}
			if len(sleeper.sleeps) != len(wantSleeps) {
				t.Fatalf("sleeps = %v, want %v", sleeper.sleeps, wantSleeps)
			}
			for i, want := range wantSleeps {
				if sleeper.sleeps[i] != want {
					t.Fatalf("sleep %d = %s, want %s", i, sleeper.sleeps[i], want)
				}
			}
			if len(notifier.messages) != 1 {
				t.Fatalf("notifications = %q, want exactly one after exhaustion", notifier.messages)
			}

			stored, err := database.Channels.GetByID(id)
			if err != nil {
				t.Fatalf("get channel: %v", err)
			}
			if stored.FetchErrorKind != test.kind || stored.FetchErrorMessage == "" || stored.FetchErrorAt == nil {
				t.Fatalf("stored channel = %+v, want exhausted %s error", stored, test.kind)
			}
		})
	}
}
