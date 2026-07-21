package digest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/parser"
)

type pipelineFetcher struct {
	posts map[string][]parser.ParsedPost
}

func (f pipelineFetcher) ParseChannel(username string) ([]parser.ParsedPost, error) {
	return append([]parser.ParsedPost(nil), f.posts[username]...), nil
}

type pipelineDelivery struct {
	mu           sync.Mutex
	calls        int
	last         *Digest
	release      chan struct{}
	entered      chan struct{}
	beforeReturn func()
}

func (d *pipelineDelivery) Deliver(_ context.Context, _ int64, digest *Digest) (DeliveryReceipt, error) {
	d.mu.Lock()
	d.calls++
	d.last = digest
	d.mu.Unlock()
	if d.entered != nil {
		close(d.entered)
	}
	if d.release != nil {
		<-d.release
	}
	if d.beforeReturn != nil {
		d.beforeReturn()
	}
	return DeliveryReceipt{MessageID: 42, MessageURL: "https://t.me/c/42"}, nil
}

func newPipelineDatabase(t *testing.T, emptyBehavior string) (*db.DB, int64, int64) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	groupID, err := database.Groups.Insert(&model.Group{TelegramChatID: -100501, Title: "Pipeline"})
	if err != nil {
		database.Close()
		t.Fatalf("insert group: %v", err)
	}
	channelID, err := database.Channels.Insert(&model.Channel{Username: "pipeline", Enabled: true})
	if err != nil {
		database.Close()
		t.Fatalf("insert channel: %v", err)
	}
	if err := database.Groups.AssignChannel(groupID, channelID, nil); err != nil {
		database.Close()
		t.Fatalf("assign channel: %v", err)
	}
	if err := database.Groups.UpdateGroupSettings(&model.GroupSettings{
		GroupID: groupID, DigestTime: "21:00", Timezone: "UTC",
		EmptyDigestBehavior: emptyBehavior,
	}); err != nil {
		database.Close()
		t.Fatalf("update group settings: %v", err)
	}
	return database, groupID, channelID
}

func TestGeneratePersistsDeliveredDigestCheckpointAndPosts(t *testing.T) {
	database, groupID, _ := newPipelineDatabase(t, "silent")
	defer database.Close()

	fetcher := pipelineFetcher{posts: map[string][]parser.ParsedPost{
		"pipeline": {{
			MessageID: 9, Text: "checkpoint post",
			PostedAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
		}},
	}}
	processor := parser.NewChannelProcessor(fetcher, parser.NewPostStorage(database.Channels, database.Posts))
	delivery := &pipelineDelivery{}
	service := NewWithProcessor(database, processor)
	service.SetDelivery(delivery)

	result, err := service.Generate(groupID)
	if err != nil {
		t.Fatalf("generate digest: %v", err)
	}
	if !result.Delivered || result.MessageID == nil {
		t.Fatalf("result = %+v, want delivered checkpoint", result)
	}
	digests, err := database.Digests.ListByGroup(groupID, 1)
	if err != nil {
		t.Fatalf("list digest history: %v", err)
	}
	if len(digests) != 1 || digests[0].MessageID == nil || *digests[0].MessageID != 42 {
		t.Fatalf("digest history = %+v, want message ID 42", digests)
	}
	posts, err := database.Digests.GetPostsForDigest(digests[0].ID)
	if err != nil {
		t.Fatalf("list digest posts: %v", err)
	}
	if len(posts) != 1 || posts[0].Text != "checkpoint post" {
		t.Fatalf("digest posts = %+v, want checkpoint post", posts)
	}
}

func TestResumePendingRetriesRenderedDigestWithoutAIOrParsing(t *testing.T) {
	database, groupID, channelID := newPipelineDatabase(t, "silent")
	defer database.Close()
	post := model.Post{
		ChannelID: channelID, MessageID: 12, Text: "сохранённый пост",
		Summary: func() *string {
			value := "Краткая сводка."
			return &value
		}(),
		PostedAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
		URL:      "https://t.me/pipeline/12", ContentHash: parser.HashContent("сохранённый пост"),
	}
	postID, err := database.Posts.Insert(&post)
	if err != nil {
		t.Fatalf("insert post: %v", err)
	}
	post.ID = postID
	pendingID, err := database.Digests.CreatePending(groupID, "📋 сохранённый дайджест", []model.Post{post})
	if err != nil {
		t.Fatalf("create pending digest: %v", err)
	}
	delivery := &pipelineDelivery{}
	service := NewWithProcessor(database, parser.NewChannelProcessor(
		pipelineFetcher{}, parser.NewPostStorage(database.Channels, database.Posts),
	))
	service.SetDelivery(delivery)

	if err := service.ResumePending(groupID); err != nil {
		t.Fatalf("resume pending: %v", err)
	}
	digest, err := database.Digests.GetByID(pendingID)
	if err != nil {
		t.Fatalf("get resumed digest: %v", err)
	}
	if digest.Status != "sent" || digest.MessageID == nil || *digest.MessageID != 42 {
		t.Fatalf("resumed digest = %+v, want sent with message ID 42", digest)
	}
	if delivery.calls != 1 || delivery.last.Text != "📋 сохранённый дайджест" {
		t.Fatalf("resume delivery = calls:%d digest:%+v", delivery.calls, delivery.last)
	}
}

func TestGenerateEmptyDigestBehaviorIsConfigurable(t *testing.T) {
	tests := []struct {
		name      string
		behavior  string
		wantCalls int
		wantSent  bool
		wantText  string
	}{
		{name: "send message", behavior: "send_message", wantCalls: 1, wantSent: true, wantText: "За сегодня нет новых постов"},
		{name: "silent", behavior: "silent", wantCalls: 0, wantSent: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database, groupID, _ := newPipelineDatabase(t, test.behavior)
			defer database.Close()
			processor := parser.NewChannelProcessor(pipelineFetcher{}, parser.NewPostStorage(database.Channels, database.Posts))
			delivery := &pipelineDelivery{}
			service := NewWithProcessor(database, processor)
			service.SetDelivery(delivery)

			result, err := service.GenerateManualResult(groupID)
			if err != nil {
				t.Fatalf("generate empty digest: %v", err)
			}
			if result.Outcome != OutcomeNoPosts || result.Delivered != test.wantSent {
				t.Fatalf("result = %+v, want no_posts delivered=%v", result, test.wantSent)
			}
			if delivery.calls != test.wantCalls {
				t.Fatalf("delivery calls = %d, want %d", delivery.calls, test.wantCalls)
			}
			if test.wantText != "" && delivery.last.Text != test.wantText {
				t.Fatalf("empty digest text = %q, want %q", delivery.last.Text, test.wantText)
			}
			var digestCount int
			if err := database.Conn().QueryRow("SELECT COUNT(*) FROM digests").Scan(&digestCount); err != nil {
				t.Fatalf("count digests: %v", err)
			}
			if digestCount != test.wantCalls {
				t.Fatalf("stored digests = %d, want %d", digestCount, test.wantCalls)
			}
		})
	}
}

func TestGenerateConfiguredGroupWithoutChannelsSendsActionableWarning(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()
	groupID, err := database.Groups.Insert(&model.Group{TelegramChatID: -100502, Title: "Без каналов"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}

	delivery := &pipelineDelivery{}
	service := NewWithProcessor(database, parser.NewChannelProcessor(
		pipelineFetcher{}, parser.NewPostStorage(database.Channels, database.Posts),
	))
	service.SetDelivery(delivery)

	result, err := service.GenerateManualResult(groupID)
	if err != nil {
		t.Fatalf("generate no-channel digest: %v", err)
	}
	if result.Outcome != OutcomeNoPosts || !result.Delivered {
		t.Fatalf("result = %+v, want no_posts with delivered warning", result)
	}
	if delivery.calls != 1 || !strings.Contains(delivery.last.Text, "не настроены каналы") {
		t.Fatalf("delivery = calls:%d digest:%+v, want actionable configuration warning", delivery.calls, delivery.last)
	}
	digests, err := database.Digests.ListByGroup(groupID, 1)
	if err != nil {
		t.Fatalf("list warning checkpoint: %v", err)
	}
	if len(digests) != 1 || digests[0].Status != "sent" {
		t.Fatalf("warning checkpoints = %+v, want one sent checkpoint", digests)
	}
}

func TestGenerateEmptyDigestCreatesCheckpointBeforeDelivery(t *testing.T) {
	database, groupID, _ := newPipelineDatabase(t, model.EmptyDigestSendMessage)
	defer database.Close()
	delivery := &pipelineDelivery{
		beforeReturn: func() {
			var pending int
			if err := database.Conn().QueryRow(
				`SELECT COUNT(*) FROM digests WHERE group_id = ? AND status = 'pending'`,
				groupID,
			).Scan(&pending); err != nil {
				t.Errorf("count pending checkpoint during delivery: %v", err)
				return
			}
			if pending != 1 {
				t.Errorf("pending checkpoints during delivery = %d, want 1", pending)
			}
		},
	}
	service := NewWithProcessor(database, parser.NewChannelProcessor(
		pipelineFetcher{}, parser.NewPostStorage(database.Channels, database.Posts),
	))
	service.SetDelivery(delivery)

	result, err := service.GenerateManualResult(groupID)
	if err != nil {
		t.Fatalf("generate empty digest: %v", err)
	}
	if !result.Delivered {
		t.Fatalf("result = %+v, want delivered empty digest", result)
	}
}

func TestGenerateRejectsConcurrentRunsForSameGroup(t *testing.T) {
	database, groupID, _ := newPipelineDatabase(t, "silent")
	defer database.Close()
	processor := parser.NewChannelProcessor(pipelineFetcher{}, parser.NewPostStorage(database.Channels, database.Posts))
	delivery := &pipelineDelivery{release: make(chan struct{}), entered: make(chan struct{})}
	service := NewWithProcessor(database, processor)
	service.SetDelivery(delivery)

	// Seed one post so the first run reaches delivery and holds the group lock.
	if _, err := database.Posts.Insert(&model.Post{
		ChannelID: 1, MessageID: 1, Text: "in flight",
		PostedAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
		URL:      "https://t.me/pipeline/1", ContentHash: parser.HashContent("in flight"),
	}); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := service.Generate(groupID)
		firstDone <- err
	}()
	<-delivery.entered

	secondDone := make(chan error, 1)
	go func() {
		_, err := service.GenerateManualResult(groupID)
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		if !errors.Is(err, ErrDigestInProgress) {
			t.Fatalf("second run error = %v, want ErrDigestInProgress", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second run blocked instead of rejecting in-progress group")
	}
	close(delivery.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first run: %v", err)
	}
}
