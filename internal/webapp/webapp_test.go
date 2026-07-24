package webapp

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
)

// TestHealthEndpoint verifies that GET /health returns 200 OK with JSON status.
func TestHealthEndpoint(t *testing.T) {
	srv := New()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("failed to GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", contentType)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}

	status, ok := body["status"]
	if !ok {
		t.Fatal("response missing 'status' field")
	}
	if status != "ok" {
		t.Errorf("expected status 'ok', got %q", status)
	}
}

// TestHealthEndpointMethodNotAllowed verifies that non-GET requests to /health
// return 405 Method Not Allowed.
func TestHealthEndpointMethodNotAllowed(t *testing.T) {
	srv := New()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/health", "application/json", nil)
	if err != nil {
		t.Fatalf("failed to POST /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405, got %d", resp.StatusCode)
	}
}

func TestServeUsesExclusiveBoundListenerAndStopReleasesIt(t *testing.T) {
	srv := New()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind listener: %v", err)
	}
	address := listener.Addr().String()
	serverErr := make(chan error, 1)
	serverDone := make(chan struct{})
	go func() {
		serverErr <- srv.Serve(listener)
		close(serverDone)
	}()
	t.Cleanup(func() {
		srv.Stop()
		select {
		case <-serverDone:
		case <-time.After(2 * time.Second):
			t.Error("Serve did not exit during cleanup")
		}
	})

	var response *http.Response
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		response, err = http.Get("http://" + address + "/health")
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET /health through bound listener: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200", response.StatusCode)
	}

	srv.Stop()
	if err := <-serverErr; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve() after Stop() = %v, want ErrServerClosed", err)
	}
	released, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("listener was not released after Stop(): %v", err)
	}
	released.Close()
}

func TestServeConfiguresConservativeHTTPBounds(t *testing.T) {
	srv := New()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind listener: %v", err)
	}
	serverErr := make(chan error, 1)
	serverDone := make(chan struct{})
	go func() {
		serverErr <- srv.Serve(listener)
		close(serverDone)
	}()
	t.Cleanup(func() {
		srv.Stop()
		select {
		case <-serverDone:
		case <-time.After(2 * time.Second):
			t.Error("Serve did not exit during cleanup")
		}
	})

	var configured *http.Server
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		srv.serverMu.RLock()
		configured = srv.srv
		srv.serverMu.RUnlock()
		if configured != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if configured == nil {
		t.Fatal("Serve did not publish HTTP server configuration")
	}
	if configured.ReadHeaderTimeout != httpReadHeaderTimeout ||
		configured.ReadTimeout != httpReadTimeout ||
		configured.WriteTimeout != httpWriteTimeout ||
		configured.IdleTimeout != httpIdleTimeout ||
		configured.MaxHeaderBytes != httpMaxHeaderBytes {
		t.Fatalf("HTTP server bounds = header:%s read:%s write:%s idle:%s max-header:%d",
			configured.ReadHeaderTimeout, configured.ReadTimeout, configured.WriteTimeout,
			configured.IdleTimeout, configured.MaxHeaderBytes)
	}
}

func TestStopWaitsForActiveRequestAndIsIdempotent(t *testing.T) {
	srv := New()
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	var releaseOnce sync.Once
	srv.router.Get("/slow", func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-releaseRequest
		w.WriteHeader(http.StatusNoContent)
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind listener: %v", err)
	}
	address := listener.Addr().String()
	serverErr := make(chan error, 1)
	serverDone := make(chan struct{})
	go func() {
		serverErr <- srv.Serve(listener)
		close(serverDone)
	}()
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseRequest) })
		srv.Stop()
		select {
		case <-serverDone:
		case <-time.After(2 * time.Second):
			t.Error("Serve did not exit during cleanup")
		}
	})

	responseDone := make(chan *http.Response, 1)
	go func() {
		response, requestErr := http.Get("http://" + address + "/slow")
		if requestErr != nil {
			responseDone <- nil
			return
		}
		responseDone <- response
	}()
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("slow request did not start")
	}

	stopDone := make(chan struct{})
	go func() {
		srv.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		t.Fatal("Stop returned before active request completed")
	case <-time.After(50 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(releaseRequest) })
	response := <-responseDone
	if response == nil {
		t.Fatal("active request failed")
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("active request status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not complete after active request drained")
	}
	if err := <-serverErr; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve() after graceful Stop() = %v, want ErrServerClosed", err)
	}
	srv.Stop()
}

func TestTerminalStateBoundsHealthAndStopsNormalHTTPWork(t *testing.T) {
	srv := New()
	srv.EnterTerminal(errors.New("bot token revoked"))

	health := httptest.NewRecorder()
	srv.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusServiceUnavailable {
		t.Fatalf("terminal health status = %d, want %d", health.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(health.Body.String(), `"status":"terminal"`) {
		t.Fatalf("terminal health body = %s, want terminal status", health.Body.String())
	}

	api := httptest.NewRecorder()
	srv.Handler().ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/api/does-not-matter", nil))
	if api.Code != http.StatusServiceUnavailable {
		t.Fatalf("terminal API status = %d, want %d", api.Code, http.StatusServiceUnavailable)
	}
}

// TestNotFoundHandler verifies that unknown routes return 404.
func TestNotFoundHandler(t *testing.T) {
	srv := New()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/nonexistent")
	if err != nil {
		t.Fatalf("failed to GET /nonexistent: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", resp.StatusCode)
	}
}

func TestEmbeddedWebAppIsServed(t *testing.T) {
	srv := New()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/webapp/")
	if err != nil {
		t.Fatalf("failed to GET embedded WebApp: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("embedded WebApp status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("embedded WebApp content type = %q, want HTML", got)
	}
}

func TestEmbeddedWebAppAssetsAreServed(t *testing.T) {
	srv := New()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, asset := range []string{"app.js", "style.css", "offline.html", "sw.js"} {
		resp, err := http.Get(ts.URL + "/webapp/" + asset)
		if err != nil {
			t.Fatalf("failed to GET %s: %v", asset, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d, want 200", asset, resp.StatusCode)
		}
	}
}

func TestValidatorBrowserBoundaryIsExplicitAndAuthenticated(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	auth, err := NewWebAppAuthWithOrigin("validator:fixture-test", "715602446", "http://localhost:8080/webapp/")
	if err != nil {
		t.Fatalf("create validator auth: %v", err)
	}
	server := NewWithProvidersAuthenticated(store, time.Second, http.DefaultClient, auth)
	t.Setenv("VALIDATOR_HTTP_ONLY", "1")
	if err := server.SetValidatorBrowserBoundary("run-token", "signed-validator-init-data"); err != nil {
		t.Fatalf("configure validator browser boundary: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/webapp/validator?token=run-token&scenario=server-down", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("validator browser status = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()
	for _, want := range []string{
		`window.Telegram`,
		`signed-validator-init-data`,
		`window.__WEBAPP_VALIDATOR_SCENARIO__`,
		`simulated server down`,
		`/webapp/app.js`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("validator browser body missing %q", want)
		}
	}
	if strings.Contains(body, "telegram.org/js/telegram-web-app.js") {
		t.Fatal("validator browser boundary should not load the external Telegram SDK")
	}

	unauthorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/webapp/validator?token=wrong", nil))
	if unauthorized.Code != http.StatusNotFound {
		t.Fatalf("wrong validator token status = %d, want 404", unauthorized.Code)
	}

	t.Setenv("VALIDATOR_HTTP_ONLY", "")
	if err := server.SetValidatorBrowserBoundary("run-token", "signed-validator-init-data"); err == nil {
		t.Fatal("validator browser boundary enabled without explicit validator HTTP mode")
	}
}

func TestOfflineFallbackShellIsSelfContainedAndRecoverable(t *testing.T) {
	srv := New()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/webapp/offline.html")
	if err != nil {
		t.Fatalf("failed to GET offline fallback shell: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read offline fallback shell: %v", err)
	}
	content := string(body)
	for _, want := range []string{
		"Не удалось загрузить приложение",
		"Повторить",
		"connection refused",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("offline fallback shell does not contain %q", want)
		}
	}
	for _, forbidden := range []string{"telegram.org", "https://", "http://"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("offline fallback shell contains external reference %q", forbidden)
		}
	}

	swResp, err := http.Get(ts.URL + "/webapp/sw.js")
	if err != nil {
		t.Fatalf("failed to GET offline service worker: %v", err)
	}
	defer swResp.Body.Close()
	swBody, err := io.ReadAll(swResp.Body)
	if err != nil {
		t.Fatalf("read offline service worker: %v", err)
	}
	sw := string(swBody)
	for _, want := range []string{"offline.html", "request.mode !== \"navigate\"", "catch"} {
		if !strings.Contains(sw, want) {
			t.Fatalf("offline service worker does not contain %q", want)
		}
	}
}
