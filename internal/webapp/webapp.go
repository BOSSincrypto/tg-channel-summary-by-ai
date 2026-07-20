// Package webapp serves the embedded SPA and provides the HTTP API
// for the WebApp admin interface, including health check, initData
// validation, and CRUD endpoints for channels, groups, and providers.
package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/forum"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
	"github.com/boss/tg-channel-summary-by-ai/internal/parser"
	staticwebapp "github.com/boss/tg-channel-summary-by-ai/webapp"
	"github.com/go-chi/chi/v5"
)

// Server handles HTTP requests for the health check and WebApp.
type Server struct {
	router           chi.Router
	apiRouter        chi.Router
	srv              *http.Server
	providerService  *ProviderService
	database         *db.DB
	channelService   *ChannelService
	groupService     *GroupService
	groupScheduler   GroupScheduler
	groupLifecycleMu sync.Mutex
	forumFence       *forum.MutationFence
	digestRunner     DigestRunner
	settingsApplier  SettingsApplier
	digestJobs       *digestJobStore
	terminalMu       sync.RWMutex
	terminalReason   error
	onTokenRevoked   func(error)
}

// SettingsMutation is the validated settings payload shared by authenticated
// WebApp HTTP writes and the Telegram web_app_data production adapter.
type SettingsMutation struct {
	DigestTime   string
	Timezone     string
	DefaultModel string
	Channels     []string
	Version      int64
}

// SettingsApplier persists a settings mutation and refreshes its downstream
// runtime dependencies. The returned version is the persisted config version.
type SettingsApplier func(context.Context, SettingsMutation) (int64, error)

// GroupScheduler is the live scheduler boundary used by WebApp group CRUD.
// RestoreGroup and RemoveGroup are idempotent, allowing request retries and
// restart reconciliation to converge without duplicate jobs.
type GroupScheduler interface {
	RestoreGroup(int64) error
	RemoveGroup(int64)
}

type groupSchedulerLifecycle interface {
	WithLifecycle(func() error) error
}

type groupSchedulerLifecycleMutations interface {
	RestoreGroupWithinLifecycle(int64) error
	RemoveGroupWithinLifecycle(int64)
}

func removeScheduledGroup(scheduler GroupScheduler, groupID int64) {
	if lifecycle, ok := scheduler.(groupSchedulerLifecycleMutations); ok {
		lifecycle.RemoveGroupWithinLifecycle(groupID)
		return
	}
	scheduler.RemoveGroup(groupID)
}

func restoreScheduledGroup(scheduler GroupScheduler, groupID int64) error {
	if lifecycle, ok := scheduler.(groupSchedulerLifecycleMutations); ok {
		return lifecycle.RestoreGroupWithinLifecycle(groupID)
	}
	return scheduler.RestoreGroup(groupID)
}

type groupSchedulerRepository interface {
	RecordSchedulerSync(int64) error
	RecordSchedulerRemoval(int64) error
	SchedulerSyncKind(int64) (string, error)
	ClearSchedulerSync(int64) error
	ListPendingSchedulerSync() ([]int64, error)
}

type tokenRevocationConfigurer interface {
	SetTokenRevocationHandler(func(error))
}

// New creates a new HTTP Server with configured routes.
func New() *Server {
	r := chi.NewRouter()

	s := &Server{
		router:     r,
		digestJobs: newDigestJobStore(),
	}

	r.Use(s.terminalMiddleware)
	r.Get("/health", s.handleHealth)
	if staticFiles, err := staticwebapp.StaticFS(); err == nil {
		r.Get("/webapp", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/webapp/", http.StatusPermanentRedirect)
		})
		r.Handle("/webapp/*", http.StripPrefix("/webapp/", http.FileServer(http.FS(staticFiles))))
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/webapp/", http.StatusTemporaryRedirect)
		})
	}

	return s
}

// NewWithProviders creates a fail-closed provider API server. Production code
// should use NewWithProvidersAuthenticated with configured Telegram auth.
func NewWithProviders(store *db.DB, timeout time.Duration, client *http.Client) *Server {
	return NewWithProvidersAuthenticated(store, timeout, client, nil)
}

// NewWithProvidersAuthenticated creates a provider API server protected by
// Telegram WebApp initData validation and the configured owner check.
func NewWithProvidersAuthenticated(store *db.DB, timeout time.Duration, client *http.Client, auth *WebAppAuth) *Server {
	if auth == nil {
		auth = &WebAppAuth{}
	}
	return newWithProviders(store, timeout, client, auth)
}

// NewWithProvidersForTesting creates an unprotected provider API server for
// unit tests that exercise CRUD behavior without Telegram initData.
func NewWithProvidersForTesting(store *db.DB, timeout time.Duration, client *http.Client) *Server {
	return newWithProviders(store, timeout, client, nil, true)
}

func newWithProviders(store *db.DB, timeout time.Duration, client *http.Client, auth *WebAppAuth, allowPrivateHosts ...bool) *Server {
	s := New()
	s.database = store
	if store == nil {
		return s
	}
	service := NewProviderService(store.Providers, client)
	if len(allowPrivateHosts) > 0 && allowPrivateHosts[0] {
		service.allowPrivateHosts = true
	}
	if timeout > 0 {
		service.validationTimeout = timeout
	}
	s.providerService = service
	s.channelService = NewChannelService(store.Channels, parserChannelVerifier{parser: parser.New()})
	s.groupService = NewGroupService(store.Groups, store.Channels)
	if auth != nil {
		s.groupService.verifier = &telegramGroupVerifier{token: auth.botToken, client: service.httpClient}
	}
	providersHandler := http.Handler(http.HandlerFunc(s.handleProviders))
	providerByIDHandler := http.Handler(http.HandlerFunc(s.handleProviderByID))
	s.apiRouter = chi.NewRouter()
	if auth != nil {
		s.apiRouter.Use(auth.Middleware)
	}
	s.apiRouter.Handle("/providers", providersHandler)
	s.apiRouter.Handle("/providers/{id}", providerByIDHandler)
	s.apiRouter.HandleFunc("/channels", s.handleChannels)
	s.apiRouter.HandleFunc("/channels/{id}", s.handleChannelByID)
	s.apiRouter.HandleFunc("/groups", s.handleGroups)
	s.apiRouter.HandleFunc("/groups/{id}", s.handleGroupByID)
	s.apiRouter.HandleFunc("/groups/{id}/channels", s.handleGroupChannels)
	s.apiRouter.HandleFunc("/groups/{id}/channels/{channelID}", s.handleGroupChannelByID)
	s.apiRouter.HandleFunc("/groups/{id}/topics", s.handleGroupTopics)
	s.apiRouter.HandleFunc("/groups/available", s.handleAvailableGroups)
	s.apiRouter.HandleFunc("/settings", s.handleSettings)
	s.apiRouter.HandleFunc("/digest/test", s.handleDigestTest)
	s.apiRouter.HandleFunc("/digest/status", s.handleDigestStatus)
	s.router.Mount("/api", s.apiRouter)
	return s
}

// SetDigestRunner connects the manual WebApp action to the production digest
// service without making the HTTP package depend on scheduler internals.
func (s *Server) SetDigestRunner(runner DigestRunner) {
	s.digestRunner = runner
}

// SetSettingsApplier connects authenticated HTTP settings writes to the same
// production application boundary used by Telegram WebApp sendData.
func (s *Server) SetSettingsApplier(applier SettingsApplier) {
	if s != nil {
		s.settingsApplier = applier
	}
}

// SetGroupScheduler connects the WebApp group lifecycle to the shared live
// scheduler instance used by production scheduled digests.
func (s *Server) SetGroupScheduler(scheduler GroupScheduler) {
	if s != nil {
		s.groupScheduler = scheduler
	}
}

func (s *Server) withGroupSchedulerLifecycle(fn func() error) error {
	if s == nil {
		return errors.New("group scheduler lifecycle: server is not configured")
	}
	if boundary, ok := s.groupScheduler.(groupSchedulerLifecycle); ok {
		return boundary.WithLifecycle(fn)
	}
	s.groupLifecycleMu.Lock()
	defer s.groupLifecycleMu.Unlock()
	return fn()
}

// ReconcileGroupScheduler retries durable group scheduler intents left by a
// failed create/delete operation. Missing groups are treated as converged
// deletes, and each successful retry clears its intent.
func (s *Server) ReconcileGroupScheduler(ctx context.Context) error {
	if s == nil || s.groupService == nil {
		return errors.New("reconcile group scheduler: group service is not configured")
	}
	if s.groupScheduler == nil {
		return errors.New("reconcile group scheduler: scheduler is not configured")
	}
	return s.withGroupSchedulerLifecycle(func() error {
		return s.reconcileGroupSchedulerLocked(ctx)
	})
}

func (s *Server) reconcileGroupSchedulerLocked(ctx context.Context) error {
	repository, ok := s.groupService.repository.(groupSchedulerRepository)
	if !ok {
		return errors.New("reconcile group scheduler: repository does not support durable intents")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("reconcile group scheduler: %w", err)
	}
	pending, err := repository.ListPendingSchedulerSync()
	if err != nil {
		return fmt.Errorf("reconcile group scheduler: list intents: %w", err)
	}
	var firstErr error
	for _, groupID := range pending {
		kind, kindErr := repository.SchedulerSyncKind(groupID)
		if kindErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("load scheduler intent for group %d: %w", groupID, kindErr)
			}
			continue
		}
		if kind == "remove" {
			removeScheduledGroup(s.groupScheduler, groupID)
			if clearErr := repository.ClearSchedulerSync(groupID); clearErr != nil && firstErr == nil {
				firstErr = fmt.Errorf("clear removed group %d scheduler intent: %w", groupID, clearErr)
			}
			continue
		}
		group, getErr := s.groupService.repository.GetByID(groupID)
		if errors.Is(getErr, db.ErrNotFound) {
			removeScheduledGroup(s.groupScheduler, groupID)
			if clearErr := repository.ClearSchedulerSync(groupID); clearErr != nil && firstErr == nil {
				firstErr = fmt.Errorf("clear deleted group %d scheduler intent: %w", groupID, clearErr)
			}
			continue
		}
		if getErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("load group %d for scheduler reconciliation: %w", groupID, getErr)
			}
			continue
		}
		if group.Status != "" && group.Status != model.GroupStatusActive {
			removeScheduledGroup(s.groupScheduler, groupID)
			if clearErr := repository.ClearSchedulerSync(groupID); clearErr != nil && firstErr == nil {
				firstErr = fmt.Errorf("clear inactive group %d scheduler intent: %w", groupID, clearErr)
			}
			continue
		}
		if restoreErr := restoreScheduledGroup(s.groupScheduler, groupID); restoreErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("restore group %d scheduler job: %w", groupID, restoreErr)
			}
			continue
		}
		if clearErr := repository.ClearSchedulerSync(groupID); clearErr != nil && firstErr == nil {
			firstErr = fmt.Errorf("clear group %d scheduler intent: %w", groupID, clearErr)
		}
	}
	return firstErr
}

// SetChannelVerifier replaces the t.me/s verifier, primarily for deterministic
// integration tests.
func (s *Server) SetChannelVerifier(verifier ChannelVerifier) {
	if s.channelService != nil {
		s.channelService.verifier = verifier
	}
}

// SetChannelVerificationRetry configures the bounded t.me/s verification
// retry policy. The sleeper is injectable for deterministic tests.
func (s *Server) SetChannelVerificationRetry(maxRetries int, sleeper func(context.Context, time.Duration) error) {
	if s.channelService != nil {
		s.channelService.SetVerificationRetry(maxRetries, sleeper)
	}
}

// SetGroupVerifier replaces the Telegram group membership verifier.
func (s *Server) SetGroupVerifier(verifier GroupVerifier) {
	if s == nil || s.groupService == nil {
		return
	}
	s.groupService.verifier = verifier
	if configurer, ok := verifier.(tokenRevocationConfigurer); ok {
		configurer.SetTokenRevocationHandler(s.onTokenRevoked)
	}
}

// SetTokenRevocationHandler connects the production Telegram getChat boundary
// to the shared application lifecycle supervisor.
func (s *Server) SetTokenRevocationHandler(handler func(error)) {
	if s == nil {
		return
	}
	s.onTokenRevoked = handler
	if s.groupService == nil || s.groupService.verifier == nil {
		return
	}
	if verifier, ok := s.groupService.verifier.(tokenRevocationConfigurer); ok {
		verifier.SetTokenRevocationHandler(handler)
	}
}

// SetTopicLifecycle connects group assignment mutations to the production
// Telegram forum-topic boundary. The WebApp package remains independent of
// the bot package and only depends on this narrow interface.
func (s *Server) SetTopicLifecycle(lifecycle TopicLifecycle) {
	if s.groupService != nil {
		s.groupService.SetTopicLifecycle(lifecycle)
	}
}

// SetForumMutationFence shares the Telegram forum lifecycle fence with group
// deletion and any other WebApp mutation boundary.
func (s *Server) SetForumMutationFence(fence *forum.MutationFence) {
	if s != nil {
		s.forumFence = fence
	}
}

// SetTopicCatalog connects forum topic discovery to an injected production
// catalog. Passing nil restores the persisted assignment-backed catalog.
func (s *Server) SetTopicCatalog(catalog TopicCatalog) {
	if s.groupService != nil {
		s.groupService.SetTopicCatalog(catalog)
	}
}

// Handler returns the http.Handler for the server, useful for testing
// with httptest.NewServer.
func (s *Server) Handler() http.Handler {
	return s.router
}

// EnterTerminal marks the HTTP boundary as terminal. Health remains
// observable, while WebApp and API requests receive a bounded 503 response
// instead of continuing normal application work.
func (s *Server) EnterTerminal(reason error) {
	if s == nil {
		return
	}
	s.terminalMu.Lock()
	if s.terminalReason == nil {
		if reason == nil {
			reason = errors.New("application entered terminal state")
		}
		s.terminalReason = reason
	}
	s.terminalMu.Unlock()
}

func (s *Server) terminalState() (bool, error) {
	if s == nil {
		return false, nil
	}
	s.terminalMu.RLock()
	defer s.terminalMu.RUnlock()
	return s.terminalReason != nil, s.terminalReason
}

func (s *Server) terminalMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			if terminal, _ := s.terminalState(); terminal {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{
					"error":  "application is shutting down",
					"status": "terminal",
				})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// handleHealth responds with a JSON health check status.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if terminal, _ := s.terminalState(); terminal {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "terminal",
			"error":  "application is shutting down",
		})
		return
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// Start begins listening on the given port (e.g. ":8080").
func (s *Server) Start(port string) error {
	if terminal, _ := s.terminalState(); terminal {
		return errors.New("HTTP server is in terminal state")
	}
	listener, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return err
	}
	return s.Serve(listener)
}

// Serve starts the HTTP server on an already-bound listener. Callers that
// need to establish ownership of a socket before serving can bind the
// listener first and then pass it here.
func (s *Server) Serve(listener net.Listener) error {
	if listener == nil {
		return errors.New("HTTP server requires a listener")
	}
	if terminal, _ := s.terminalState(); terminal {
		_ = listener.Close()
		return errors.New("HTTP server is in terminal state")
	}
	s.srv = &http.Server{
		Addr:    listener.Addr().String(),
		Handler: s.router,
	}
	return s.srv.Serve(listener)
}

// Stop gracefully shuts down the HTTP server.
func (s *Server) Stop() {
	if s.srv != nil {
		s.srv.Close()
	}
}

func decodeJSON(r *http.Request, w http.ResponseWriter, value any) error {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(value); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Некорректный JSON"})
		return err
	}
	return nil
}
