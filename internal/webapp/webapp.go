// Package webapp serves the embedded SPA and provides the HTTP API
// for the WebApp admin interface, including health check, initData
// validation, and CRUD endpoints for channels, groups, and providers.
package webapp

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/parser"
	staticwebapp "github.com/boss/tg-channel-summary-by-ai/webapp"
	"github.com/go-chi/chi/v5"
)

// Server handles HTTP requests for the health check and WebApp.
type Server struct {
	router          chi.Router
	apiRouter       chi.Router
	srv             *http.Server
	providerService *ProviderService
	database        *db.DB
	channelService  *ChannelService
	groupService    *GroupService
	digestRunner    DigestRunner
	digestJobs      *digestJobStore
}

// New creates a new HTTP Server with configured routes.
func New() *Server {
	r := chi.NewRouter()

	s := &Server{
		router:     r,
		digestJobs: newDigestJobStore(),
	}

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
		s.groupService.verifier = telegramGroupVerifier{token: auth.botToken, client: service.httpClient}
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
	if s.groupService != nil {
		s.groupService.verifier = verifier
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

// handleHealth responds with a JSON health check status.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
	})
}

// Start begins listening on the given port (e.g. ":8080").
func (s *Server) Start(port string) error {
	s.srv = &http.Server{
		Addr:    ":" + port,
		Handler: s.router,
	}
	return s.srv.ListenAndServe()
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
