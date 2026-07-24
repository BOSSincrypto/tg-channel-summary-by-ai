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
	"os"
	"strings"
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
	router                   chi.Router
	apiRouter                chi.Router
	srv                      *http.Server
	serverMu                 sync.RWMutex
	serverStopping           bool
	serverStopOnce           sync.Once
	serverStopDone           chan struct{}
	digestRunnerCloseOnce    sync.Once
	providerService          *ProviderService
	database                 *db.DB
	channelService           *ChannelService
	groupService             *GroupService
	groupScheduler           GroupScheduler
	groupLifecycleMu         sync.Mutex
	forumFence               *forum.MutationFence
	digestRunner             DigestRunner
	settingsApplier          SettingsApplier
	digestJobs               *digestJobStore
	terminalMu               sync.RWMutex
	terminalReason           error
	onTokenRevoked           func(error)
	validatorBrowserMu       sync.RWMutex
	validatorBrowserToken    string
	validatorBrowserInitData string
	authBotTokenValue        string
}

const (
	httpReadHeaderTimeout = 5 * time.Second
	httpReadTimeout       = 15 * time.Second
	httpWriteTimeout      = 15 * time.Second
	httpIdleTimeout       = 60 * time.Second
	httpMaxHeaderBytes    = 1 << 20
	httpShutdownTimeout   = 5 * time.Second
)

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
		r.Get("/webapp/validator", s.handleValidatorBrowser)
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
		s.authBotTokenValue = auth.botToken
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

// SetValidatorBrowserBoundary enables the authenticated browser bootstrap used
// only by the local validator HTTP mode. It is deliberately guarded by both
// the validator opt-in environment and the fake credential shape so a normal
// production server cannot expose a browser authentication bypass.
func (s *Server) SetValidatorBrowserBoundary(runToken, initData string) error {
	if s == nil {
		return errors.New("validator browser boundary: server is not configured")
	}
	if os.Getenv("VALIDATOR_HTTP_ONLY") != "1" {
		return errors.New("validator browser boundary requires VALIDATOR_HTTP_ONLY=1")
	}
	if s.providerService == nil || s.providerService.httpClient == nil {
		return errors.New("validator browser boundary requires a configured HTTP server")
	}
	if strings.TrimSpace(runToken) == "" {
		return errors.New("validator browser boundary requires a run token")
	}
	if !strings.HasPrefix(strings.TrimSpace(s.authBotTokenValue), "validator:") {
		return errors.New("validator browser boundary requires fake validator credentials")
	}
	if strings.TrimSpace(initData) == "" {
		return errors.New("validator browser boundary requires initData")
	}
	s.validatorBrowserMu.Lock()
	s.validatorBrowserToken = strings.TrimSpace(runToken)
	s.validatorBrowserInitData = initData
	s.validatorBrowserMu.Unlock()
	return nil
}

func (s *Server) handleValidatorBrowser(w http.ResponseWriter, r *http.Request) {
	s.validatorBrowserMu.RLock()
	runToken := s.validatorBrowserToken
	initData := s.validatorBrowserInitData
	s.validatorBrowserMu.RUnlock()
	if runToken == "" || initData == "" || r.URL.Query().Get("token") != runToken {
		http.NotFound(w, r)
		return
	}
	scenario := strings.TrimSpace(r.URL.Query().Get("scenario"))
	initDataLiteral := validatorScriptLiteral(initData)
	userLiteral := validatorScriptLiteral(`{"id":715602446,"first_name":"Validator","username":"validator_owner"}`)
	scenarioLiteral := validatorScriptLiteral(scenario)
	page := `<!doctype html>
<html lang="ru">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
  <meta name="theme-color" content="#17212b">
  <title>Digest Control Validator</title>
  <link rel="stylesheet" href="/webapp/style.css">
</head>
<body>
  <main id="app" aria-live="polite"></main>
  <script>
    window.Telegram = { WebApp: {
      initData: ` + initDataLiteral + `,
      initDataUnsafe: { user: ` + userLiteral + ` },
      colorScheme: "light",
      themeParams: {},
      ready: function () {},
      expand: function () {},
      close: function () {},
      onEvent: function () {},
      MainButton: { setText: function () {}, show: function () {}, hide: function () {}, onClick: function () {}, offClick: function () {} },
      BackButton: { show: function () {}, hide: function () {}, onClick: function () {} }
    }};
    window.__WEBAPP_VALIDATOR_SCENARIO__ = ` + scenarioLiteral + `;
    if (window.__WEBAPP_VALIDATOR_SCENARIO__ === "server-down") {
      var validatorFetch = window.fetch.bind(window);
      var validatorFailed = false;
      window.fetch = function (input, options) {
        var requestURL = typeof input === "string" ? input : input && input.url;
        if (!validatorFailed && requestURL && requestURL.indexOf("/api/") >= 0) {
          validatorFailed = true;
          console.error("validator browser boundary: simulated server down");
          return Promise.reject(new TypeError("Failed to fetch: validator server down"));
        }
        return validatorFetch(input, options);
      };
    }
  </script>
  <script src="/webapp/app.js"></script>
</body>
</html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(page))
}

func validatorScriptLiteral(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return strings.NewReplacer(
		"<", `\u003c`,
		">", `\u003e`,
		"&", `\u0026`,
	).Replace(string(encoded))
}

// SetDigestRunner connects the manual WebApp action to the production digest
// service without making the HTTP package depend on scheduler internals.
func (s *Server) SetDigestRunner(runner DigestRunner) {
	if s == nil {
		return
	}
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

// SetAvailableGroupDiscovery connects the authenticated WebApp group picker
// to the running Telegram bot's membership discovery boundary.
func (s *Server) SetAvailableGroupDiscovery(discovery AvailableGroupDiscovery) {
	if s == nil || s.groupService == nil {
		return
	}
	s.groupService.discovery = discovery
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
	s.closeDigestRunner()
}

func (s *Server) closeDigestRunner() {
	if s == nil {
		return
	}
	s.digestRunnerCloseOnce.Do(func() {
		if runner, ok := s.digestRunner.(closableDigestRunner); ok {
			runner.Close()
		}
	})
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
	if s == nil {
		return errors.New("HTTP server requires a server")
	}
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
	if s == nil {
		if listener != nil {
			_ = listener.Close()
		}
		return errors.New("HTTP server requires a server")
	}
	if listener == nil {
		return errors.New("HTTP server requires a listener")
	}
	if terminal, _ := s.terminalState(); terminal {
		_ = listener.Close()
		return errors.New("HTTP server is in terminal state")
	}
	server := &http.Server{
		Addr:              listener.Addr().String(),
		Handler:           s.router,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       httpIdleTimeout,
		MaxHeaderBytes:    httpMaxHeaderBytes,
	}
	s.serverMu.Lock()
	if s.serverStopping {
		s.serverMu.Unlock()
		_ = listener.Close()
		return http.ErrServerClosed
	}
	if s.srv != nil {
		s.serverMu.Unlock()
		_ = listener.Close()
		return errors.New("HTTP server is already serving")
	}
	s.srv = server
	s.serverMu.Unlock()
	err := server.Serve(listener)
	s.serverMu.Lock()
	if s.srv == server {
		s.srv = nil
	}
	s.serverMu.Unlock()
	return err
}

// Stop gracefully shuts down the HTTP server.
func (s *Server) Stop() {
	if s == nil {
		return
	}
	s.closeDigestRunner()
	s.serverMu.RLock()
	stopDone := s.serverStopDone
	s.serverMu.RUnlock()
	if stopDone == nil {
		s.serverMu.Lock()
		if s.serverStopDone == nil {
			s.serverStopDone = make(chan struct{})
		}
		stopDone = s.serverStopDone
		s.serverMu.Unlock()
	}
	s.serverStopOnce.Do(func() {
		s.serverMu.Lock()
		s.serverStopping = true
		server := s.srv
		s.serverMu.Unlock()
		if server != nil {
			ctx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
			defer cancel()
			if err := server.Shutdown(ctx); err != nil {
				// Shutdown is bounded by context. Force-close any connections that
				// did not drain so lifecycle transitions cannot hang indefinitely.
				_ = server.Close()
			}
		}
		close(stopDone)
	})
	<-stopDone
}

func decodeJSON(r *http.Request, w http.ResponseWriter, value any) error {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(value); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Некорректный JSON"})
		return err
	}
	return nil
}
