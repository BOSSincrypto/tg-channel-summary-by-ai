package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/digest"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/parser"
	"github.com/robfig/cron/v3"
)

type fakeRunner struct {
	groupIDs []int64
}

func (f *fakeRunner) Generate(groupID int64) (*digest.Digest, error) {
	f.groupIDs = append(f.groupIDs, groupID)
	return &digest.Digest{GroupID: groupID}, nil
}

type windowRecordingRunner struct {
	calls []struct {
		groupID  int64
		windowID string
	}
}

type blockingRunner struct {
	started chan struct{}
	release chan struct{}
	calls   chan int64
}

func (r *blockingRunner) Generate(groupID int64) (*digest.Digest, error) {
	select {
	case r.started <- struct{}{}:
	default:
	}
	<-r.release
	select {
	case r.calls <- groupID:
	default:
	}
	return &digest.Digest{GroupID: groupID}, nil
}

func (r *windowRecordingRunner) Generate(groupID int64) (*digest.Digest, error) {
	return &digest.Digest{GroupID: groupID}, nil
}

func (r *windowRecordingRunner) GenerateWithWindow(groupID int64, windowID string) (*digest.Digest, error) {
	r.calls = append(r.calls, struct {
		groupID  int64
		windowID string
	}{groupID: groupID, windowID: windowID})
	return &digest.Digest{GroupID: groupID, WindowID: windowID}, nil
}

type recordingDigestRunner struct {
	service  *digest.Service
	groupIDs []int64
	result   *digest.Digest
}

func (r *recordingDigestRunner) Generate(groupID int64) (*digest.Digest, error) {
	r.groupIDs = append(r.groupIDs, groupID)
	result, err := r.service.Generate(groupID)
	if err != nil {
		return nil, err
	}
	r.result = result
	return result, nil
}

type schedulerDeliverySpy struct {
	calls int
}

func (d *schedulerDeliverySpy) Deliver(context.Context, int64, *digest.Digest) (digest.DeliveryReceipt, error) {
	d.calls++
	return digest.DeliveryReceipt{MessageID: 1}, nil
}

type fakeGroupSource struct {
	groups   []model.Group
	settings map[int64]*model.GroupSettings
}

func (f fakeGroupSource) List() ([]model.Group, error) {
	return f.groups, nil
}

func (f fakeGroupSource) GetGroupSettings(groupID int64) (*model.GroupSettings, error) {
	return f.settings[groupID], nil
}

type fakeCronEntry struct {
	spec string
	cmd  func()
}

type fakeCronEngine struct {
	entries  map[cron.EntryID]fakeCronEntry
	removed  []cron.EntryID
	started  bool
	stopped  bool
	nextID   cron.EntryID
	startCnt int
	stopCnt  int
	addErr   error
}

func newFakeCronEngine() *fakeCronEngine {
	return &fakeCronEngine{entries: make(map[cron.EntryID]fakeCronEntry)}
}

func (f *fakeCronEngine) AddFunc(spec string, cmd func()) (cron.EntryID, error) {
	if f.addErr != nil {
		err := f.addErr
		f.addErr = nil
		return 0, err
	}
	f.nextID++
	f.entries[f.nextID] = fakeCronEntry{spec: spec, cmd: cmd}
	return f.nextID, nil
}

func (f *fakeCronEngine) Start() {
	f.started = true
	f.startCnt++
}

func (f *fakeCronEngine) Stop() context.Context {
	f.stopped = true
	f.stopCnt++
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func (f *fakeCronEngine) Remove(id cron.EntryID) {
	f.removed = append(f.removed, id)
	delete(f.entries, id)
}

func (f *fakeCronEngine) RunAll() {
	for _, entry := range f.entries {
		entry.cmd()
	}
}

func TestSchedulerStartRegistersGroupJobsAndStopRemovesThem(t *testing.T) {
	engine := newFakeCronEngine()
	runner := &fakeRunner{}
	source := fakeGroupSource{
		groups: []model.Group{{ID: 1}, {ID: 2}},
		settings: map[int64]*model.GroupSettings{
			1: {GroupID: 1, DigestTime: "09:15", Timezone: "UTC"},
			2: {GroupID: 2, DigestTime: "21:00", Timezone: "Europe/Moscow"},
		},
	}

	s := New(runner, WithGroupSource(source), withCronEngine(engine))
	if err := s.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}

	if !engine.started || engine.startCnt != 1 {
		t.Fatalf("scheduler start state = started:%v startCnt:%d, want started once", engine.started, engine.startCnt)
	}
	if len(engine.entries) != 2 {
		t.Fatalf("registered jobs = %d, want 2", len(engine.entries))
	}

	gotSpecs := map[string]bool{}
	for _, entry := range engine.entries {
		gotSpecs[entry.spec] = true
	}
	if !gotSpecs["CRON_TZ=UTC 15 9 * * *"] {
		t.Fatalf("missing UTC schedule in %v", gotSpecs)
	}
	if !gotSpecs["CRON_TZ=Europe/Moscow 0 21 * * *"] {
		t.Fatalf("missing Europe/Moscow schedule in %v", gotSpecs)
	}

	engine.RunAll()
	if len(runner.groupIDs) != 2 {
		t.Fatalf("runner group calls = %v, want 2 invocations", runner.groupIDs)
	}

	s.Stop()
	if !engine.stopped || engine.stopCnt != 1 {
		t.Fatalf("scheduler stop state = stopped:%v stopCnt:%d, want stopped once", engine.stopped, engine.stopCnt)
	}
	if len(engine.removed) != 2 {
		t.Fatalf("removed jobs = %d, want 2", len(engine.removed))
	}
	if len(engine.entries) != 0 {
		t.Fatalf("remaining jobs = %d, want 0", len(engine.entries))
	}
}

func TestSchedulerRestoreGroupReRegistersExactlyOneJob(t *testing.T) {
	engine := newFakeCronEngine()
	runner := &fakeRunner{}
	source := fakeGroupSource{
		groups: []model.Group{{ID: 7, Status: model.GroupStatusActive}},
		settings: map[int64]*model.GroupSettings{
			7: {GroupID: 7, DigestTime: "09:15", Timezone: "UTC"},
		},
	}
	s := New(runner, WithGroupSource(source), withCronEngine(engine))
	if err := s.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	if len(engine.entries) != 1 {
		t.Fatalf("initial jobs = %d, want 1", len(engine.entries))
	}

	s.RemoveGroup(7)
	if len(engine.entries) != 0 {
		t.Fatalf("jobs after removal = %d, want 0", len(engine.entries))
	}
	if err := s.RestoreGroup(7); err != nil {
		t.Fatalf("restore group: %v", err)
	}
	if err := s.RestoreGroup(7); err != nil {
		t.Fatalf("idempotent restore group: %v", err)
	}
	if len(engine.entries) != 1 {
		t.Fatalf("jobs after restore = %d, want exactly 1", len(engine.entries))
	}
	s.Stop()
}

func TestSchedulerRefreshGroupReplacesScheduleInSharedInstance(t *testing.T) {
	engine := newFakeCronEngine()
	runner := &fakeRunner{}
	source := fakeGroupSource{
		groups: []model.Group{{ID: 8, Status: model.GroupStatusActive}},
		settings: map[int64]*model.GroupSettings{
			8: {GroupID: 8, DigestTime: "21:00", Timezone: "UTC"},
		},
	}
	s := New(runner, WithGroupSource(source), withCronEngine(engine))
	if err := s.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer s.Stop()
	if got, ok := s.ScheduleForGroup(8); !ok || got != "CRON_TZ=UTC 0 21 * * *" {
		t.Fatalf("initial schedule = %q, registered=%v", got, ok)
	}

	source.settings[8].DigestTime = "09:30"
	if err := s.RefreshGroup(8); err != nil {
		t.Fatalf("refresh group: %v", err)
	}
	if len(engine.entries) != 1 {
		t.Fatalf("entries after refresh = %d, want one", len(engine.entries))
	}
	if got, ok := s.ScheduleForGroup(8); !ok || got != "CRON_TZ=UTC 30 9 * * *" {
		t.Fatalf("refreshed schedule = %q, registered=%v", got, ok)
	}
}

func TestSchedulerSettingsRefreshCompensatesLateRegistrationFailure(t *testing.T) {
	engine := newFakeCronEngine()
	runner := &fakeRunner{}
	source := fakeGroupSource{
		groups: []model.Group{{ID: 8, Status: model.GroupStatusActive}},
		settings: map[int64]*model.GroupSettings{
			8: {GroupID: 8, DigestTime: "21:00", Timezone: "UTC"},
		},
	}
	s := New(runner, WithGroupSource(source), withCronEngine(engine))
	if err := s.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer s.Stop()
	plan, err := s.PrepareSettingsRefresh(map[int64]*model.GroupSettings{
		8: {GroupID: 8, DigestTime: "09:30", Timezone: "UTC"},
	})
	if err != nil {
		t.Fatalf("prepare settings refresh: %v", err)
	}
	engine.addErr = errors.New("injected scheduler registration failure")
	if err := plan.Apply(); err == nil {
		t.Fatal("settings refresh succeeded despite injected registration failure")
	}
	if got, ok := s.ScheduleForGroup(8); !ok || got != "CRON_TZ=UTC 0 21 * * *" {
		t.Fatalf("schedule after compensated failure = %q, registered=%v, want previous schedule", got, ok)
	}
	if len(engine.entries) != 1 {
		t.Fatalf("entries after compensated failure = %d, want one", len(engine.entries))
	}
}

func TestSchedulerRejectsStaleSettingsPlanAfterConcurrentGroupLifecycleMutation(t *testing.T) {
	engine := newFakeCronEngine()
	source := &fakeGroupSource{
		groups: []model.Group{{ID: 1, Status: model.GroupStatusActive}},
		settings: map[int64]*model.GroupSettings{
			1: {GroupID: 1, DigestTime: "21:00", Timezone: "UTC"},
			2: {GroupID: 2, DigestTime: "09:00", Timezone: "UTC"},
		},
	}
	s := New(&fakeRunner{}, WithGroupSource(source), withCronEngine(engine))
	if err := s.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer s.Stop()

	plan, err := s.PrepareSettingsRefresh(map[int64]*model.GroupSettings{
		1: source.settings[1],
	})
	if err != nil {
		t.Fatalf("prepare settings refresh: %v", err)
	}
	source.groups = append(source.groups, model.Group{ID: 2, Status: model.GroupStatusActive})
	if err := s.RestoreGroup(2); err != nil {
		t.Fatalf("restore concurrently created group: %v", err)
	}
	if err := plan.Apply(); !errors.Is(err, ErrStaleSettingsRefresh) {
		t.Fatalf("stale plan error = %v, want ErrStaleSettingsRefresh", err)
	}
	if _, ok := s.ScheduleForGroup(2); !ok {
		t.Fatal("stale settings plan removed concurrently created group job")
	}

	delete(source.settings, 1)
	source.groups = []model.Group{{ID: 2, Status: model.GroupStatusActive}}
	s.RemoveGroup(1)
	if _, ok := s.ScheduleForGroup(1); ok {
		t.Fatal("deleted group still has a scheduler job")
	}
}

func TestSchedulerLifecycleFencesRunAgainstGroupDeletion(t *testing.T) {
	engine := newFakeCronEngine()
	runner := &blockingRunner{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
		calls:   make(chan int64, 1),
	}
	source := &fakeGroupSource{
		groups: []model.Group{{ID: 9, Status: model.GroupStatusActive}},
		settings: map[int64]*model.GroupSettings{
			9: {GroupID: 9, DigestTime: "21:00", Timezone: "UTC"},
		},
	}
	s := New(runner, WithGroupSource(source), withCronEngine(engine))
	if err := s.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer s.Stop()

	runDone := make(chan error, 1)
	go func() {
		_, err := s.RunGroupWithWindow(9, "delete-fence")
		runDone <- err
	}()
	<-runner.started

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- s.WithLifecycle(func() error {
			source.groups = nil
			s.RemoveGroupWithinLifecycle(9)
			return nil
		})
	}()
	select {
	case err := <-deleteDone:
		t.Fatalf("deletion crossed in-flight runner fence: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	close(runner.release)
	if err := <-runDone; err != nil {
		t.Fatalf("in-flight run: %v", err)
	}
	if err := <-deleteDone; err != nil {
		t.Fatalf("delete lifecycle: %v", err)
	}
	select {
	case groupID := <-runner.calls:
		if groupID != 9 {
			t.Fatalf("runner group ID = %d, want 9", groupID)
		}
	default:
		t.Fatal("in-flight runner did not reach runner")
	}

	if _, err := s.RunGroupWithWindow(9, "after-delete"); err == nil {
		t.Fatal("run after successful deletion unexpectedly reached runner")
	}
	select {
	case <-runner.calls:
		t.Fatal("runner invoked after successful deletion")
	default:
	}
}

func TestSchedulerSharesWindowIDAcrossGroupsAndChangesItPerWindow(t *testing.T) {
	engine := newFakeCronEngine()
	runner := &windowRecordingRunner{}
	source := fakeGroupSource{
		groups: []model.Group{{ID: 1}, {ID: 2}},
		settings: map[int64]*model.GroupSettings{
			1: {GroupID: 1, DigestTime: "09:15", Timezone: "UTC"},
			2: {GroupID: 2, DigestTime: "09:15", Timezone: "UTC"},
		},
	}

	s := New(runner, WithGroupSource(source), withCronEngine(engine))
	if err := s.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	engine.RunAll()
	engine.RunAll()

	if len(runner.calls) != 4 {
		t.Fatalf("window calls = %#v, want four group runs", runner.calls)
	}
	firstWindow := runner.calls[0].windowID
	secondWindow := runner.calls[2].windowID
	if firstWindow == "" || secondWindow == "" || firstWindow == secondWindow {
		t.Fatalf("window IDs = %q and %q, want distinct non-empty windows", firstWindow, secondWindow)
	}
	if runner.calls[1].windowID != firstWindow || runner.calls[3].windowID != secondWindow {
		t.Fatalf("window IDs = %#v, want shared ID per scheduler cycle", runner.calls)
	}
	if runner.calls[0].groupID == runner.calls[1].groupID {
		t.Fatalf("group calls = %#v, want both groups in each window", runner.calls)
	}
	s.Stop()
}

func TestSchedulerStartInvokesDigestPipelineThroughParserStorage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`
			<div class="tgme_widget_message" data-post="scheduled_channel/11">
				<div class="tgme_widget_message_text">scheduled digest post</div>
				<time datetime="2099-07-15T18:30:00Z"></time>
			</div>
			<div class="tgme_widget_message" data-post="scheduled_channel/10">
				<div class="tgme_widget_message_text">already seen post</div>
				<time datetime="2099-07-15T18:00:00Z"></time>
			</div>`))
	}))
	defer server.Close()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	channelID, err := database.Channels.Insert(&model.Channel{Username: "scheduled_channel", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	summary := "already summarized"
	if _, err := database.Posts.Insert(&model.Post{
		ChannelID:   channelID,
		MessageID:   10,
		Text:        "already seen post",
		Summary:     &summary,
		PostedAt:    "2099-07-15T18:00:00Z",
		URL:         "https://t.me/scheduled_channel/10",
		ContentHash: parser.HashContent("already seen post"),
	}); err != nil {
		t.Fatalf("insert already seen post: %v", err)
	}
	if err := database.Channels.UpdateLastPostID(channelID, 10); err != nil {
		t.Fatalf("advance channel cursor: %v", err)
	}
	groupID, err := database.Groups.Insert(&model.Group{TelegramChatID: -1002, Title: "Scheduled"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := database.Groups.AssignChannel(groupID, channelID, nil); err != nil {
		t.Fatalf("assign channel: %v", err)
	}
	if err := database.Groups.UpdateGroupSettings(&model.GroupSettings{GroupID: groupID, DigestTime: "21:00", Timezone: "UTC"}); err != nil {
		t.Fatalf("update group settings: %v", err)
	}

	fetcher := parser.NewWithOptions(parser.Options{Client: server.Client(), BaseURL: server.URL})
	processor := parser.NewChannelProcessor(fetcher, parser.NewPostStorage(database.Channels, database.Posts))
	digestService := digest.NewWithProcessor(database, processor)
	runner := &recordingDigestRunner{service: digestService}
	engine := newFakeCronEngine()

	s := New(runner, WithGroupSource(database.Groups), withCronEngine(engine))
	if err := s.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer s.Stop()

	if len(engine.entries) != 1 {
		t.Fatalf("registered jobs = %d, want 1", len(engine.entries))
	}

	engine.RunAll()

	if len(runner.groupIDs) != 1 || runner.groupIDs[0] != groupID {
		t.Fatalf("runner group calls = %v, want [%d]", runner.groupIDs, groupID)
	}
	if runner.result == nil || runner.result.PostCount != 1 {
		t.Fatalf("scheduled digest result = %+v, want one new post", runner.result)
	}

	storedChannel, err := database.Channels.GetByID(channelID)
	if err != nil {
		t.Fatalf("get channel: %v", err)
	}
	if storedChannel.LastPostID != 11 {
		t.Fatalf("last_post_id = %d, want 11", storedChannel.LastPostID)
	}

	storedPost, err := database.Posts.GetByChannelAndMessageID(channelID, 11)
	if err != nil {
		t.Fatalf("get stored post: %v", err)
	}
	if storedPost.URL != "https://t.me/scheduled_channel/11" {
		t.Fatalf("stored URL = %q, want canonical URL", storedPost.URL)
	}
	var postCount int
	if err := database.Conn().QueryRow("SELECT COUNT(*) FROM posts WHERE channel_id = ?", channelID).Scan(&postCount); err != nil {
		t.Fatalf("count stored posts: %v", err)
	}
	if postCount != 2 {
		t.Fatalf("stored post count = %d, want existing post plus one new post", postCount)
	}
}

func TestSchedulerZeroPostsWithFailedChannelPreservesNoPostsAndSkipsDelivery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/s/broken" {
			http.Error(w, "channel unavailable", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`<html><body><div class="tgme_channel_info"></div></body></html>`))
	}))
	defer server.Close()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	groupID, err := database.Groups.Insert(&model.Group{TelegramChatID: -1003, Title: "Scheduled empty"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	for _, username := range []string{"empty", "broken"} {
		channelID, insertErr := database.Channels.Insert(&model.Channel{Username: username, Enabled: true})
		if insertErr != nil {
			t.Fatalf("insert channel %s: %v", username, insertErr)
		}
		if assignErr := database.Groups.AssignChannel(groupID, channelID, nil); assignErr != nil {
			t.Fatalf("assign channel %s: %v", username, assignErr)
		}
	}
	if err := database.Groups.UpdateGroupSettings(&model.GroupSettings{
		GroupID: groupID, DigestTime: "21:00", Timezone: "UTC",
	}); err != nil {
		t.Fatalf("update group settings: %v", err)
	}

	fetcher := parser.NewWithOptions(parser.Options{Client: server.Client(), BaseURL: server.URL})
	processor := parser.NewChannelProcessor(fetcher, parser.NewPostStorage(database.Channels, database.Posts))
	digestService := digest.NewWithProcessor(database, processor)
	delivery := &schedulerDeliverySpy{}
	digestService.SetDelivery(delivery)
	runner := &recordingDigestRunner{service: digestService}
	engine := newFakeCronEngine()
	scheduler := New(runner, WithGroupSource(database.Groups), withCronEngine(engine))
	if err := scheduler.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer scheduler.Stop()

	engine.RunAll()

	if runner.result == nil {
		t.Fatal("scheduler did not return a digest result")
	}
	if runner.result.Outcome != digest.OutcomeNoPosts || runner.result.Delivered {
		t.Fatalf("scheduled result = %+v, want no_posts and not delivered", runner.result)
	}
	if len(runner.result.FailedChannels) != 1 || runner.result.FailedChannels[0] != "@broken" {
		t.Fatalf("failed channels = %v, want [@broken]", runner.result.FailedChannels)
	}
	if len(runner.result.FailureDetails) != 1 || runner.result.FailureDetails[0] == "" {
		t.Fatalf("failure details = %v, want channel error metadata", runner.result.FailureDetails)
	}
	if delivery.calls != 0 {
		t.Fatalf("delivery calls = %d, want none", delivery.calls)
	}
}

// --- Catch-up on restart tests (VAL-DIGEST-003) ---

type fakeDigestHistory struct {
	digests map[int64][]model.Digest
}

func (f *fakeDigestHistory) ListByGroup(groupID int64, limit int) ([]model.Digest, error) {
	digests := f.digests[groupID]
	if limit > 0 && len(digests) > limit {
		digests = digests[:limit]
	}
	return digests, nil
}

func TestSchedulerCatchUpFiresForMissedSchedule(t *testing.T) {
	engine := newFakeCronEngine()
	runner := &fakeRunner{}
	// digest_time 09:00 UTC; "now" is 09:30 the same day (bot restarted 30m late)
	now := time.Date(2026, 7, 21, 9, 30, 0, 0, time.UTC)
	source := fakeGroupSource{
		groups: []model.Group{{ID: 5, Status: model.GroupStatusActive}},
		settings: map[int64]*model.GroupSettings{
			5: {GroupID: 5, DigestTime: "09:00", Timezone: "UTC"},
		},
	}
	history := &fakeDigestHistory{digests: map[int64][]model.Digest{}}

	s := New(runner, WithGroupSource(source), withCronEngine(engine),
		WithDigestHistory(history), WithNowFunc(func() time.Time { return now }))
	if err := s.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer s.Stop()

	if err := s.CatchUp(); err != nil {
		t.Fatalf("catch-up: %v", err)
	}

	if len(runner.groupIDs) != 1 || runner.groupIDs[0] != 5 {
		t.Fatalf("catch-up runner calls = %v, want [5]", runner.groupIDs)
	}
}

func TestSchedulerCatchUpSkipsWhenDigestAlreadySent(t *testing.T) {
	engine := newFakeCronEngine()
	runner := &fakeRunner{}
	// digest_time 09:00 UTC; "now" is 09:30 the same day
	now := time.Date(2026, 7, 21, 9, 30, 0, 0, time.UTC)
	source := fakeGroupSource{
		groups: []model.Group{{ID: 5, Status: model.GroupStatusActive}},
		settings: map[int64]*model.GroupSettings{
			5: {GroupID: 5, DigestTime: "09:00", Timezone: "UTC"},
		},
	}
	// A digest was already sent at 09:05 (after the 09:00 scheduled time)
	sentAt := "2026-07-21 09:05:00"
	history := &fakeDigestHistory{
		digests: map[int64][]model.Digest{
			5: {{ID: 1, GroupID: 5, SentAt: sentAt, PostCount: 3}},
		},
	}

	s := New(runner, WithGroupSource(source), withCronEngine(engine),
		WithDigestHistory(history), WithNowFunc(func() time.Time { return now }))
	if err := s.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer s.Stop()

	if err := s.CatchUp(); err != nil {
		t.Fatalf("catch-up: %v", err)
	}

	if len(runner.groupIDs) != 0 {
		t.Fatalf("catch-up runner calls = %v, want no calls (digest already sent)", runner.groupIDs)
	}
}

func TestSchedulerCatchUpSkipsFutureSchedule(t *testing.T) {
	engine := newFakeCronEngine()
	runner := &fakeRunner{}
	// digest_time 21:00 UTC; "now" is 08:00 — schedule hasn't arrived yet today
	now := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	source := fakeGroupSource{
		groups: []model.Group{{ID: 5, Status: model.GroupStatusActive}},
		settings: map[int64]*model.GroupSettings{
			5: {GroupID: 5, DigestTime: "21:00", Timezone: "UTC"},
		},
	}
	// A digest was sent yesterday at 21:05 (after yesterday's 21:00 schedule)
	sentAt := "2026-07-20 21:05:00"
	history := &fakeDigestHistory{
		digests: map[int64][]model.Digest{
			5: {{ID: 1, GroupID: 5, SentAt: sentAt, PostCount: 2}},
		},
	}

	s := New(runner, WithGroupSource(source), withCronEngine(engine),
		WithDigestHistory(history), WithNowFunc(func() time.Time { return now }))
	if err := s.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer s.Stop()

	if err := s.CatchUp(); err != nil {
		t.Fatalf("catch-up: %v", err)
	}

	if len(runner.groupIDs) != 0 {
		t.Fatalf("catch-up runner calls = %v, want no calls (future schedule, yesterday already sent)", runner.groupIDs)
	}
}

func TestSchedulerCatchUpFiresForYesterdayMissedWhenTodayIsFuture(t *testing.T) {
	engine := newFakeCronEngine()
	runner := &fakeRunner{}
	// digest_time 21:00 UTC; "now" is 08:00 — today's schedule hasn't arrived
	// but yesterday's 21:00 was missed (bot was down, no digest sent)
	now := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	source := fakeGroupSource{
		groups: []model.Group{{ID: 5, Status: model.GroupStatusActive}},
		settings: map[int64]*model.GroupSettings{
			5: {GroupID: 5, DigestTime: "21:00", Timezone: "UTC"},
		},
	}
	history := &fakeDigestHistory{digests: map[int64][]model.Digest{}}

	s := New(runner, WithGroupSource(source), withCronEngine(engine),
		WithDigestHistory(history), WithNowFunc(func() time.Time { return now }))
	if err := s.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer s.Stop()

	if err := s.CatchUp(); err != nil {
		t.Fatalf("catch-up: %v", err)
	}

	if len(runner.groupIDs) != 1 || runner.groupIDs[0] != 5 {
		t.Fatalf("catch-up runner calls = %v, want [5] (yesterday missed)", runner.groupIDs)
	}
}

// --- DST transition tests (VAL-DIGEST-043) ---

func TestSchedulerDSTSpringForwardSkipsNonexistentTimeAndNotifies(t *testing.T) {
	engine := newFakeCronEngine()
	runner := &fakeRunner{}
	// America/New_York spring-forward: March 8, 2026 at 2:00 AM → 3:00 AM.
	// 02:30 does not exist. "now" is 03:00 AM EDT (after the gap).
	nyLoc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}
	now := time.Date(2026, 3, 8, 3, 0, 0, 0, nyLoc)
	source := fakeGroupSource{
		groups: []model.Group{{ID: 10, Title: "DST Group", Status: model.GroupStatusActive}},
		settings: map[int64]*model.GroupSettings{
			10: {GroupID: 10, DigestTime: "02:30", Timezone: "America/New_York"},
		},
	}
	// Yesterday's 02:30 was sent, so no catch-up fires — only the DST skip
	// notification should be produced.
	sentAt := "2026-03-07 07:35:00" // 02:35 EST = 07:35 UTC
	history := &fakeDigestHistory{
		digests: map[int64][]model.Digest{
			10: {{ID: 1, GroupID: 10, SentAt: sentAt, PostCount: 1}},
		},
	}

	var dstNotifications []string
	notifier := func(groupID int64, groupTitle, digestTime, timezone, reason string) {
		dstNotifications = append(dstNotifications, fmt.Sprintf("%d|%s|%s|%s|%s",
			groupID, groupTitle, digestTime, timezone, reason))
	}

	s := New(runner, WithGroupSource(source), withCronEngine(engine),
		WithDigestHistory(history), WithNowFunc(func() time.Time { return now }),
		WithDSTSkipNotifier(notifier))
	if err := s.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer s.Stop()

	if err := s.CatchUp(); err != nil {
		t.Fatalf("catch-up: %v", err)
	}

	if len(dstNotifications) != 1 {
		t.Fatalf("DST notifications = %v, want 1", dstNotifications)
	}
	if !strings.Contains(dstNotifications[0], "10|") ||
		!strings.Contains(dstNotifications[0], "02:30") ||
		!strings.Contains(dstNotifications[0], "America/New_York") {
		t.Fatalf("DST notification = %q, want group 10, time 02:30, tz America/New_York", dstNotifications[0])
	}
	if len(runner.groupIDs) != 0 {
		t.Fatalf("runner calls = %v, want 0 (yesterday already sent, today nonexistent)", runner.groupIDs)
	}
}

func TestSchedulerDSTSpringForwardCatchUpFiresForYesterdayMissed(t *testing.T) {
	engine := newFakeCronEngine()
	runner := &fakeRunner{}
	nyLoc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}
	now := time.Date(2026, 3, 8, 3, 0, 0, 0, nyLoc)
	source := fakeGroupSource{
		groups: []model.Group{{ID: 10, Title: "DST Group", Status: model.GroupStatusActive}},
		settings: map[int64]*model.GroupSettings{
			10: {GroupID: 10, DigestTime: "02:30", Timezone: "America/New_York"},
		},
	}
	// No digest sent — yesterday's 02:30 was missed
	history := &fakeDigestHistory{digests: map[int64][]model.Digest{}}

	var dstNotifications []string
	notifier := func(groupID int64, groupTitle, digestTime, timezone, reason string) {
		dstNotifications = append(dstNotifications, reason)
	}

	s := New(runner, WithGroupSource(source), withCronEngine(engine),
		WithDigestHistory(history), WithNowFunc(func() time.Time { return now }),
		WithDSTSkipNotifier(notifier))
	if err := s.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer s.Stop()

	if err := s.CatchUp(); err != nil {
		t.Fatalf("catch-up: %v", err)
	}

	// DST notification for today's nonexistent time
	if len(dstNotifications) != 1 {
		t.Fatalf("DST notifications = %v, want 1", dstNotifications)
	}
	// Catch-up fires for yesterday's missed 02:30
	if len(runner.groupIDs) != 1 || runner.groupIDs[0] != 10 {
		t.Fatalf("catch-up runner calls = %v, want [10] (yesterday missed)", runner.groupIDs)
	}
}

func TestSchedulerDSTFallBackFiresOnceNotTwice(t *testing.T) {
	engine := newFakeCronEngine()
	runner := &fakeRunner{}
	nyLoc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}
	// America/New_York fall-back: November 1, 2026 at 2:00 AM EDT → 1:00 AM EST.
	// 01:30 occurs twice: first in EDT (offset -14400), then in EST (offset -18000).
	firstOccurrence := time.Date(2026, 11, 1, 1, 30, 0, 0, nyLoc) // 01:30 EDT
	secondOccurrence := firstOccurrence.Add(1 * time.Hour)        // 01:30 EST

	currentTime := firstOccurrence
	source := fakeGroupSource{
		groups: []model.Group{{ID: 11, Status: model.GroupStatusActive}},
		settings: map[int64]*model.GroupSettings{
			11: {GroupID: 11, DigestTime: "01:30", Timezone: "America/New_York"},
		},
	}

	s := New(runner, WithGroupSource(source), withCronEngine(engine),
		WithNowFunc(func() time.Time { return currentTime }))
	if err := s.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer s.Stop()

	if len(engine.entries) != 1 {
		t.Fatalf("registered jobs = %d, want 1", len(engine.entries))
	}

	// First occurrence (EDT) — should fire
	engine.RunAll()
	if len(runner.groupIDs) != 1 {
		t.Fatalf("after first fire: runner calls = %v, want 1", runner.groupIDs)
	}

	// Second occurrence (EST, same wall-clock 01:30 but different offset) — should be skipped
	currentTime = secondOccurrence
	engine.RunAll()
	if len(runner.groupIDs) != 1 {
		t.Fatalf("after second fire: runner calls = %v, want still 1 (DST fall-back dedup)", runner.groupIDs)
	}
}

// --- Per-group timezone tests (VAL-DIGEST-032) ---

func TestSchedulerPerGroupTimezoneProducesDifferentUTCSpecs(t *testing.T) {
	engine := newFakeCronEngine()
	runner := &fakeRunner{}
	// Group A: Europe/Moscow (UTC+3), Group B: Asia/Tashkent (UTC+5)
	// Both at 09:00 local → Group A fires at 06:00 UTC, Group B at 04:00 UTC
	source := fakeGroupSource{
		groups: []model.Group{
			{ID: 1, Status: model.GroupStatusActive},
			{ID: 2, Status: model.GroupStatusActive},
		},
		settings: map[int64]*model.GroupSettings{
			1: {GroupID: 1, DigestTime: "09:00", Timezone: "Europe/Moscow"},
			2: {GroupID: 2, DigestTime: "09:00", Timezone: "Asia/Tashkent"},
		},
	}

	s := New(runner, WithGroupSource(source), withCronEngine(engine))
	if err := s.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer s.Stop()

	specA, ok := s.ScheduleForGroup(1)
	if !ok {
		t.Fatal("missing schedule for group 1 (Moscow)")
	}
	specB, ok := s.ScheduleForGroup(2)
	if !ok {
		t.Fatal("missing schedule for group 2 (Tashkent)")
	}
	// Both at minute 0, hour 9, but with different CRON_TZ
	if specA != "CRON_TZ=Europe/Moscow 0 9 * * *" {
		t.Fatalf("Moscow spec = %q, want CRON_TZ=Europe/Moscow 0 9 * * *", specA)
	}
	if specB != "CRON_TZ=Asia/Tashkent 0 9 * * *" {
		t.Fatalf("Tashkent spec = %q, want CRON_TZ=Asia/Tashkent 0 9 * * *", specB)
	}
	// Verify the specs are different (different timezones → different UTC fire times)
	if specA == specB {
		t.Fatalf("Moscow and Tashkent specs are identical: %q", specA)
	}
}

// --- Reschedule on config change test (VAL-CROSS-007) ---

func TestSchedulerRescheduleOnConfigChangeOldTimeNotTriggered(t *testing.T) {
	engine := newFakeCronEngine()
	runner := &fakeRunner{}
	source := &fakeGroupSource{
		groups: []model.Group{{ID: 8, Status: model.GroupStatusActive}},
		settings: map[int64]*model.GroupSettings{
			8: {GroupID: 8, DigestTime: "21:00", Timezone: "Europe/Moscow"},
		},
	}
	s := New(runner, WithGroupSource(source), withCronEngine(engine))
	if err := s.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer s.Stop()

	// Verify initial schedule at 21:00
	if got, ok := s.ScheduleForGroup(8); !ok || got != "CRON_TZ=Europe/Moscow 0 21 * * *" {
		t.Fatalf("initial schedule = %q, registered=%v", got, ok)
	}

	// Change digest_time from 21:00 to 14:00
	source.settings[8].DigestTime = "14:00"
	if err := s.RefreshGroup(8); err != nil {
		t.Fatalf("refresh group: %v", err)
	}

	// Verify exactly one job registered (old removed, new added)
	if len(engine.entries) != 1 {
		t.Fatalf("entries after refresh = %d, want exactly 1", len(engine.entries))
	}

	// Verify new schedule at 14:00
	if got, ok := s.ScheduleForGroup(8); !ok || got != "CRON_TZ=Europe/Moscow 0 14 * * *" {
		t.Fatalf("refreshed schedule = %q, registered=%v, want 14:00", got, ok)
	}

	// Verify the old 21:00 spec is no longer present
	for _, entry := range engine.entries {
		if entry.spec == "CRON_TZ=Europe/Moscow 0 21 * * *" {
			t.Fatalf("old 21:00 schedule still registered after refresh: %q", entry.spec)
		}
	}
}

// --- Catch-up with real DB integration (VAL-DIGEST-003 production boundary) ---

func TestSchedulerCatchUpWithRealDBFiresForMissedSchedule(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	groupID, err := database.Groups.Insert(&model.Group{TelegramChatID: -1005, Title: "Catch-up DB"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := database.Groups.UpdateGroupSettings(&model.GroupSettings{
		GroupID: groupID, DigestTime: "09:00", Timezone: "UTC",
	}); err != nil {
		t.Fatalf("update group settings: %v", err)
	}

	runner := &recordingDigestRunner{service: digest.NewWithProcessor(database, nil)}
	engine := newFakeCronEngine()
	now := time.Date(2026, 7, 21, 9, 30, 0, 0, time.UTC)

	s := New(runner, WithGroupSource(database.Groups), withCronEngine(engine),
		WithDigestHistory(database.Digests), WithNowFunc(func() time.Time { return now }))
	if err := s.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer s.Stop()

	if err := s.CatchUp(); err != nil {
		t.Fatalf("catch-up: %v", err)
	}

	if len(runner.groupIDs) != 1 || runner.groupIDs[0] != groupID {
		t.Fatalf("catch-up runner calls = %v, want [%d]", runner.groupIDs, groupID)
	}
}
