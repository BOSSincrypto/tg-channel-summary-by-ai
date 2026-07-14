// Package webapp serves the embedded SPA and provides the HTTP API
// for the WebApp admin interface, including health check, initData
// validation, and CRUD endpoints for channels, groups, and providers.
package webapp

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Server handles HTTP requests for the health check and WebApp.
type Server struct {
	router chi.Router
	srv    *http.Server
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
