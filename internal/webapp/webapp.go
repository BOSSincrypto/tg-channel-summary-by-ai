// Package webapp serves the embedded SPA and provides the HTTP API
// for the WebApp admin interface, including health check, initData
// validation, and CRUD endpoints for channels, groups, and providers.
package webapp

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/go-chi/chi/v5"
)

// Server handles HTTP requests for the health check and WebApp.
type Server struct {
	router          chi.Router
	srv             *http.Server
	providerService *ProviderService
}

// New creates a new HTTP Server with configured routes.
func New() *Server {
	r := chi.NewRouter()

	s := &Server{
		router: r,
	}

	r.Get("/health", s.handleHealth)

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
	service := NewProviderService(store.Providers, client)
	if len(allowPrivateHosts) > 0 && allowPrivateHosts[0] {
		service.allowPrivateHosts = true
	}
	if timeout > 0 {
		service.validationTimeout = timeout
	}
	s.providerService = service
	providersHandler := http.Handler(http.HandlerFunc(s.handleProviders))
	providerByIDHandler := http.Handler(http.HandlerFunc(s.handleProviderByID))
	if auth != nil {
		providersHandler = auth.Middleware(providersHandler)
		providerByIDHandler = auth.Middleware(providerByIDHandler)
	}
	s.router.Handle("/api/providers", providersHandler)
	s.router.Handle("/api/providers/{id}", providerByIDHandler)
	return s
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
