package bot

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOwnerNotifierNotifyOwner(t *testing.T) {
	var gotPath string
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	notifier := NewOwnerNotifier("test-token", "12345")
	notifier.baseURL = server.URL
	notifier.httpClient = server.Client()

	if err := notifier.NotifyOwner(context.Background(), "disk usage high"); err != nil {
		t.Fatalf("notify owner: %v", err)
	}

	if gotPath != "/bottest-token/sendMessage" {
		t.Fatalf("path = %q, want %q", gotPath, "/bottest-token/sendMessage")
	}
	for _, want := range []string{`"chat_id":"12345"`, `"text":"disk usage high"`} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("request body %q does not contain %q", gotBody, want)
		}
	}
}

func TestOwnerNotifierNotifyOwnerRejectsMissingConfig(t *testing.T) {
	if err := NewOwnerNotifier("", "123").NotifyOwner(context.Background(), "hi"); err == nil {
		t.Fatal("expected missing bot token error")
	}
	if err := NewOwnerNotifier("token", "").NotifyOwner(context.Background(), "hi"); err == nil {
		t.Fatal("expected missing owner id error")
	}
}
