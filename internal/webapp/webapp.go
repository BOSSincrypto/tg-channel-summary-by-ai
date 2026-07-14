// Package webapp serves the embedded SPA and provides the HTTP API
// for the WebApp admin interface, including health check, initData
// validation, and CRUD endpoints for channels, groups, and providers.
package webapp

import "net/http"

// Server handles HTTP requests for the health check and WebApp.
type Server struct {
	// TODO: chi router, database handle
}

// New creates a new HTTP Server.
func New() *Server {
	return &Server{}
}

// Start begins listening on the given port.
func (s *Server) Start(port string) error {
	// TODO: configure routes, start HTTP server
	_ = http.DefaultServeMux
	return nil
}

// Stop gracefully shuts down the HTTP server.
func (s *Server) Stop() {
	// TODO: shutdown with context
}
