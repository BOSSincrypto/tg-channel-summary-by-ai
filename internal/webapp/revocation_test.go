package webapp

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/lifecycle"
	"github.com/boss/tg-channel-summary-by-ai/internal/telegram"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type revocationTestStopper struct {
	once sync.Once
	done chan struct{}
}

func (s *revocationTestStopper) Stop() {
	s.once.Do(func() { close(s.done) })
}

func TestAuthenticatedGroupVerification401PropagatesTokenRevocation(t *testing.T) {
	store, err := db.OpenWithEncryptionKey(":memory:", "unit-db-key")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	auth, err := NewWebAppAuth("unit-bot-token", "715602446")
	if err != nil {
		t.Fatalf("create auth: %v", err)
	}
	var telegramCalls int
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		telegramCalls++
		if request.URL.Path != "/botunit-bot-token/getChat" {
			t.Fatalf("Telegram path = %q, want getChat", request.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Status:     "401 Unauthorized",
			Body:       io.NopCloser(strings.NewReader(`{"ok":false,"description":"Unauthorized"}`)),
			Header:     make(http.Header),
			Request:    request,
		}, nil
	})}
	server := NewWithProvidersAuthenticated(store, time.Second, client, auth)
	supervisor := lifecycle.New(time.Second)
	stopped := &revocationTestStopper{done: make(chan struct{})}
	supervisor.Add(server)
	supervisor.Add(stopped)
	revoked := make(chan error, 1)
	server.SetTokenRevocationHandler(func(err error) {
		server.EnterTerminal(err)
		revoked <- err
		supervisor.TokenRevoked(err)
	})

	request := httptest.NewRequest(http.MethodPost, "/api/groups", strings.NewReader(`{"chat_id":"-1001234567890"}`))
	request.Header.Set(initDataHeader, signedInitData("unit-bot-token", "715602446", time.Now()))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("401 verification status = %d, body=%s; want 400", recorder.Code, recorder.Body.String())
	}
	if telegramCalls != 1 {
		t.Fatalf("Telegram getChat calls = %d, want 1", telegramCalls)
	}
	select {
	case err := <-revoked:
		if !errors.Is(err, telegram.ErrTokenRevoked) {
			t.Fatalf("revocation callback error = %v, want ErrTokenRevoked", err)
		}
	case <-time.After(time.Second):
		t.Fatal("revocation callback was not invoked")
	}
	select {
	case <-supervisor.Done():
	case <-time.After(time.Second):
		t.Fatal("lifecycle supervisor did not complete")
	}
	terminal, reason := supervisor.Terminal()
	if !terminal || !errors.Is(reason, telegram.ErrTokenRevoked) {
		t.Fatalf("supervisor terminal=%v reason=%v, want token revocation", terminal, reason)
	}
	select {
	case <-stopped.done:
	case <-time.After(time.Second):
		t.Fatal("shared lifecycle component was not stopped")
	}

	afterRevocation := httptest.NewRecorder()
	server.Handler().ServeHTTP(afterRevocation, httptest.NewRequest(http.MethodGet, "/api/groups", nil))
	if afterRevocation.Code != http.StatusServiceUnavailable {
		t.Fatalf("post-revocation API status = %d, want 503", afterRevocation.Code)
	}
}

func TestAuthenticatedGroupVerificationNon401RemainsOrdinaryFailure(t *testing.T) {
	store, err := db.OpenWithEncryptionKey(":memory:", "unit-db-key")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	auth, err := NewWebAppAuth("unit-bot-token", "715602446")
	if err != nil {
		t.Fatalf("create auth: %v", err)
	}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Status:     "502 Bad Gateway",
			Body:       io.NopCloser(strings.NewReader(`{"ok":false,"description":"Bad Gateway"}`)),
			Header:     make(http.Header),
			Request:    request,
		}, nil
	})}
	server := NewWithProvidersAuthenticated(store, time.Second, client, auth)
	supervisor := lifecycle.New(time.Second)
	server.SetTokenRevocationHandler(func(err error) {
		server.EnterTerminal(err)
		supervisor.TokenRevoked(err)
	})

	request := httptest.NewRequest(http.MethodPost, "/api/groups", strings.NewReader(`{"chat_id":"-1001234567890"}`))
	request.Header.Set(initDataHeader, signedInitData("unit-bot-token", "715602446", time.Now()))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("non-401 verification status = %d, body=%s; want 400", recorder.Code, recorder.Body.String())
	}
	select {
	case <-supervisor.Done():
		t.Fatal("non-401 verification failure entered terminal lifecycle")
	default:
	}
	terminal, reason := supervisor.Terminal()
	if terminal || reason != nil {
		t.Fatalf("non-401 terminal=%v reason=%v, want ordinary failure", terminal, reason)
	}
	health := httptest.NewRecorder()
	server.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("non-401 health status = %d, want 200", health.Code)
	}

}
