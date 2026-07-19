package scheduler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
