package webapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/digest"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/scheduler"
)

type deletionFenceRunner struct {
	started chan struct{}
	release chan struct{}

	mu         sync.Mutex
	calls      int
	startedAt  time.Time
	finishedAt time.Time
}

func (r *deletionFenceRunner) invoke(groupID int64) *digest.Digest {
	r.mu.Lock()
	r.calls++
	r.startedAt = time.Now()
	r.mu.Unlock()
	select {
	case r.started <- struct{}{}:
	default:
	}
	<-r.release
	r.mu.Lock()
	r.finishedAt = time.Now()
	r.mu.Unlock()
	return &digest.Digest{
		GroupID: groupID,
		Outcome: digest.OutcomeNoPosts,
		Message: "Нет новых постов для дайджеста.",
	}
}

func (r *deletionFenceRunner) Generate(groupID int64) (*digest.Digest, error) {
	return r.invoke(groupID), nil
}

func (r *deletionFenceRunner) GenerateManual(groupID int64) (*digest.Digest, error) {
	return r.invoke(groupID), nil
}

func (r *deletionFenceRunner) GenerateManualResult(groupID int64) (*digest.Digest, error) {
	return r.invoke(groupID), nil
}

func (r *deletionFenceRunner) snapshot() (int, time.Time, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.startedAt, r.finishedAt
}

type observedGroupScheduler struct {
	*scheduler.Scheduler
	lifecycleAttempt chan struct{}
	lifecycleOnce    sync.Once
}

func (s *observedGroupScheduler) WithLifecycle(fn func() error) error {
	s.lifecycleOnce.Do(func() { close(s.lifecycleAttempt) })
	return s.Scheduler.WithLifecycle(fn)
}

func newAuthenticatedDeleteFenceServer(t *testing.T) (*Server, *db.DB, *scheduler.Scheduler, *deletionFenceRunner, int64, string, chan struct{}) {
	t.Helper()
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	groupID, err := store.Groups.Insert(&model.Group{
		TelegramChatID: -1009876543210,
		Title:          "Fence group",
		Status:         model.GroupStatusActive,
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := store.Groups.UpdateGroupSettings(&model.GroupSettings{
		GroupID: groupID, DigestTime: "21:00", Timezone: "UTC",
	}); err != nil {
		t.Fatalf("update group settings: %v", err)
	}

	runner := &deletionFenceRunner{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	sched := scheduler.New(runner, scheduler.WithGroupSource(store.Groups))
	if err := sched.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	t.Cleanup(sched.Stop)
	lifecycleAttempt := make(chan struct{})
	observedScheduler := &observedGroupScheduler{
		Scheduler:        sched,
		lifecycleAttempt: lifecycleAttempt,
	}

	auth, err := NewWebAppAuth("unit-bot-token", "715602446")
	if err != nil {
		t.Fatalf("create WebApp auth: %v", err)
	}
	server := NewWithProvidersAuthenticated(store, time.Second, http.DefaultClient, auth)
	server.SetGroupScheduler(observedScheduler)
	server.SetDigestRunner(runner)
	return server, store, sched, runner, groupID,
		signedInitData("unit-bot-token", "715602446", time.Now()), lifecycleAttempt
}

func authenticatedJSON(t *testing.T, handler http.Handler, method, path, body, initData string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(initDataHeader, initData)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func waitForDigestError(t *testing.T, handler http.Handler, jobID, initData string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		response := authenticatedJSON(t, handler, http.MethodGet,
			"/api/digest/status?id="+jobID, "", initData)
		if response.Code == http.StatusOK {
			var job digestJob
			if err := json.Unmarshal(response.Body.Bytes(), &job); err != nil {
				t.Fatalf("decode digest status: %v", err)
			}
			if job.Status == "error" {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("digest job %s did not reach error after group deletion", jobID)
}

func waitForDeleteAfterRunnerRelease(t *testing.T, handler http.Handler, groupID int64, initData string, release, lifecycleAttempt chan struct{}) (*httptest.ResponseRecorder, time.Time) {
	t.Helper()
	deleteDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		deleteDone <- authenticatedJSON(t, handler, http.MethodDelete,
			"/api/groups/"+jsonNumber(groupID), `{"version":1}`, initData)
	}()
	<-lifecycleAttempt
	select {
	case response := <-deleteDone:
		t.Fatalf("group deletion completed while runner was fenced: status=%d body=%s", response.Code, response.Body.String())
	default:
	}

	close(release)
	response := <-deleteDone
	return response, time.Now()
}

func TestAuthenticatedWebAppDeleteDrainsInFlightManualDigest(t *testing.T) {
	server, store, sched, runner, groupID, initData, lifecycleAttempt := newAuthenticatedDeleteFenceServer(t)

	start := authenticatedJSON(t, server.Handler(), http.MethodPost, "/api/digest/test",
		`{"group_id":"`+jsonNumber(groupID)+`"}`, initData)
	if start.Code != http.StatusAccepted {
		t.Fatalf("manual digest status = %d, body=%s", start.Code, start.Body.String())
	}
	<-runner.started

	response, deletedAt := waitForDeleteAfterRunnerRelease(t, server.Handler(), groupID, initData, runner.release, lifecycleAttempt)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body=%s", response.Code, response.Body.String())
	}
	if _, err := store.Groups.GetByID(groupID); err != db.ErrNotFound {
		t.Fatalf("group after deletion = %v, want not found", err)
	}
	if _, ok := sched.ScheduleForGroup(groupID); ok {
		t.Fatal("deleted group still has a live scheduler job")
	}
	calls, startedAt, finishedAt := runner.snapshot()
	if calls != 1 {
		t.Fatalf("manual runner calls = %d, want one", calls)
	}
	if startedAt.IsZero() || finishedAt.IsZero() || finishedAt.After(deletedAt) {
		t.Fatalf("manual runner timeline started=%v finished=%v deleted=%v, want drain before deletion completion",
			startedAt, finishedAt, deletedAt)
	}

	afterDelete := authenticatedJSON(t, server.Handler(), http.MethodPost, "/api/digest/test",
		`{"group_id":"`+jsonNumber(groupID)+`"}`, initData)
	if afterDelete.Code != http.StatusAccepted {
		t.Fatalf("post-delete manual digest status = %d, body=%s", afterDelete.Code, afterDelete.Body.String())
	}
	var job digestJob
	if err := json.Unmarshal(afterDelete.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode post-delete manual job: %v", err)
	}
	waitForDigestError(t, server.Handler(), job.ID, initData)
	calls, _, _ = runner.snapshot()
	if calls != 1 {
		t.Fatalf("post-delete manual runner calls = %d, want no new invocation", calls)
	}
	var deliveryRows int
	if err := store.Conn().QueryRow(`SELECT COUNT(*) FROM digests WHERE group_id = ?`, groupID).Scan(&deliveryRows); err != nil {
		t.Fatalf("count post-delete delivery rows: %v", err)
	}
	if deliveryRows != 0 {
		t.Fatalf("post-delete delivery rows = %d, want none", deliveryRows)
	}
}

func TestAuthenticatedWebAppDeleteFencesInFlightScheduledDigest(t *testing.T) {
	server, store, sched, runner, groupID, initData, lifecycleAttempt := newAuthenticatedDeleteFenceServer(t)

	runDone := make(chan error, 1)
	go func() {
		_, err := sched.RunGroupWithWindow(groupID, "scheduled-delete-fence")
		runDone <- err
	}()
	<-runner.started

	response, deletedAt := waitForDeleteAfterRunnerRelease(t, server.Handler(), groupID, initData, runner.release, lifecycleAttempt)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body=%s", response.Code, response.Body.String())
	}
	if err := <-runDone; err != nil {
		t.Fatalf("scheduled run = %v", err)
	}
	if _, err := store.Groups.GetByID(groupID); err != db.ErrNotFound {
		t.Fatalf("group after deletion = %v, want not found", err)
	}
	if _, ok := sched.ScheduleForGroup(groupID); ok {
		t.Fatal("deleted group still has a live scheduler job")
	}
	calls, startedAt, finishedAt := runner.snapshot()
	if calls != 1 {
		t.Fatalf("scheduled runner calls = %d, want one", calls)
	}
	if startedAt.IsZero() || finishedAt.IsZero() || finishedAt.After(deletedAt) {
		t.Fatalf("scheduled runner timeline started=%v finished=%v deleted=%v, want drain before deletion completion",
			startedAt, finishedAt, deletedAt)
	}
}
