package webapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

func TestProviderServiceCreateMasksAPIKeyAndPersistsEncryptedSecret(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()

	service := NewProviderServiceForTesting(store.Providers, server.Client())
	provider, err := service.Create(context.Background(), ProviderInput{
		Name:         "Local vLLM",
		BaseURL:      server.URL + "/v1",
		APIKey:       "unit-provider-create",
		DefaultModel: "my-model",
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if provider.APIKey != "********" {
		t.Fatalf("returned API key = %q, want masked value", provider.APIKey)
	}

	var stored string
	if err := store.Conn().QueryRow("SELECT api_key FROM ai_providers WHERE id = ?", provider.ID).Scan(&stored); err != nil {
		t.Fatalf("read stored provider: %v", err)
	}
	if stored == "unit-provider-create" || !strings.HasPrefix(stored, "enc:v1:") {
		t.Fatalf("stored API key is not encrypted: %q", stored)
	}
	raw, err := store.Providers.GetByID(provider.ID)
	if err != nil {
		t.Fatalf("read provider: %v", err)
	}
	if raw.APIKey != "unit-provider-create" {
		t.Fatalf("decrypted API key = %q, want original secret", raw.APIKey)
	}
}

func TestProviderServiceUpdatePreservesKeyWhenMaskedOrOmitted(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()

	service := NewProviderServiceForTesting(store.Providers, server.Client())
	created, err := service.Create(context.Background(), ProviderInput{
		Name:         "Local",
		BaseURL:      server.URL,
		APIKey:       "unit-provider-original",
		DefaultModel: "model-a",
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}

	updated, err := service.Update(context.Background(), created.ID, ProviderInput{
		Name:         "Renamed",
		BaseURL:      server.URL,
		APIKey:       "********",
		DefaultModel: "model-b",
		Version:      created.Version,
	})
	if err != nil {
		t.Fatalf("update provider: %v", err)
	}
	if updated.APIKey != "********" {
		t.Fatalf("updated API key = %q, want masked value", updated.APIKey)
	}
	raw, err := store.Providers.GetByID(created.ID)
	if err != nil {
		t.Fatalf("read updated provider: %v", err)
	}
	if raw.APIKey != "unit-provider-original" {
		t.Fatalf("stored secret changed during masked update: %q", raw.APIKey)
	}
}

func TestProviderServiceUpdateDoesNotPersistWhenValidationFails(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	working := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer working.Close()
	service := NewProviderServiceForTesting(store.Providers, working.Client())
	created, err := service.Create(context.Background(), ProviderInput{
		Name: "Stable", BaseURL: working.URL, APIKey: "unit-provider-stable", DefaultModel: "model-a",
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"invalid"}}`, http.StatusUnauthorized)
	}))
	defer failing.Close()
	_, err = service.Update(context.Background(), created.ID, ProviderInput{
		Name: "Should Not Persist", BaseURL: failing.URL, APIKey: "unit-provider-new", DefaultModel: "model-b",
		Version: created.Version,
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
	current, err := store.Providers.GetByID(created.ID)
	if err != nil {
		t.Fatalf("read provider after failed update: %v", err)
	}
	if current.Name != "Stable" || current.DefaultModel != "model-a" || current.APIKey != "unit-provider-stable" {
		t.Fatalf("failed update changed provider: %#v", current)
	}
}

func TestProviderHTTPCreateReturnsValidationAndTimeoutErrors(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
	}))
	defer unauthorized.Close()

	server := NewWithProvidersForTesting(store, 50*time.Millisecond, unauthorized.Client())
	requestBody := `{"name":"Broken","base_url":"` + unauthorized.URL + `","api_key":"unit-provider-bad","default_model":"model"}`
	req := httptest.NewRequest(http.MethodPost, "/api/providers", strings.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "unit-provider-bad") {
		t.Fatalf("response leaked API key: %s", recorder.Body.String())
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer slow.Close()
	server = NewWithProvidersForTesting(store, 20*time.Millisecond, slow.Client())
	requestBody = `{"name":"Slow","base_url":"` + slow.URL + `","api_key":"unit-provider-slow","default_model":"model"}`
	req = httptest.NewRequest(http.MethodPost, "/api/providers", strings.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("timeout status = %d, want 504", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "unit-provider-slow") {
		t.Fatalf("timeout response leaked API key: %s", recorder.Body.String())
	}

	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode timeout response: %v", err)
	}
	if body["error"] == "" {
		t.Fatal("timeout response missing error")
	}
}

func TestProviderServiceListMasksAPIKeys(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()
	if _, err := store.Providers.Insert(&model.AIProvider{
		Name:         "Provider",
		BaseURL:      "https://example.test/v1",
		APIKey:       "unit-provider-list",
		DefaultModel: "model",
	}); err != nil {
		t.Fatalf("insert provider: %v", err)
	}

	service := NewProviderServiceForTesting(store.Providers, http.DefaultClient)
	providers, err := service.List()
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	if len(providers) != 1 || providers[0].APIKey != "********" {
		t.Fatalf("providers = %#v, want one masked provider", providers)
	}
}

func TestProviderMutationsRequireCurrentPositiveVersion(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	validation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer validation.Close()

	server := NewWithProvidersForTesting(store, time.Second, validation.Client())
	create := func(name string, isDefault bool) map[string]any {
		t.Helper()
		body := `{"name":"` + name + `","base_url":"` + validation.URL + `","api_key":"unit-provider-secret","default_model":"model","is_default":` + map[bool]string{true: "true", false: "false"}[isDefault] + `}`
		response := doJSON(t, server.Handler(), http.MethodPost, "/api/providers", body)
		if response.Code != http.StatusCreated {
			t.Fatalf("create provider status = %d, body=%s", response.Code, response.Body.String())
		}
		var provider map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &provider); err != nil {
			t.Fatalf("decode provider: %v", err)
		}
		return provider
	}
	first := create("First", true)
	second := create("Second", false)
	secondID := int64(second["id"].(float64))

	missing := doJSON(t, server.Handler(), http.MethodPatch, "/api/providers/"+jsonNumber(secondID),
		`{"name":"Missing","base_url":"`+validation.URL+`","api_key":"********","default_model":"model","is_default":true}`)
	if missing.Code != http.StatusConflict {
		t.Fatalf("missing provider version status = %d, body=%s", missing.Code, missing.Body.String())
	}
	zero := doJSON(t, server.Handler(), http.MethodPatch, "/api/providers/"+jsonNumber(secondID),
		`{"name":"Zero","base_url":"`+validation.URL+`","api_key":"********","default_model":"model","is_default":true,"version":0}`)
	if zero.Code != http.StatusConflict {
		t.Fatalf("zero provider version status = %d, body=%s", zero.Code, zero.Body.String())
	}

	current := doJSON(t, server.Handler(), http.MethodPatch, "/api/providers/"+jsonNumber(secondID),
		`{"name":"Current","base_url":"`+validation.URL+`","api_key":"********","default_model":"model","is_default":true,"version":1}`)
	if current.Code != http.StatusOK {
		t.Fatalf("current provider version status = %d, body=%s", current.Code, current.Body.String())
	}
	var updated map[string]any
	if err := json.Unmarshal(current.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode updated provider: %v", err)
	}
	if updated["version"].(float64) != 2 || !updated["is_default"].(bool) {
		t.Fatalf("updated provider = %#v, want default version 2", updated)
	}

	stale := doJSON(t, server.Handler(), http.MethodPatch, "/api/providers/"+jsonNumber(secondID),
		`{"name":"Stale","base_url":"`+validation.URL+`","api_key":"********","default_model":"model","is_default":false,"version":1}`)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale provider version status = %d, body=%s", stale.Code, stale.Body.String())
	}
	raw, err := store.Providers.GetByID(secondID)
	if err != nil {
		t.Fatalf("load provider after stale update: %v", err)
	}
	if raw.Name != "Current" || !raw.IsDefault || raw.Version != 2 {
		t.Fatalf("stale update changed provider: %#v", raw)
	}
	firstID := int64(first["id"].(float64))
	firstRaw, err := store.Providers.GetByID(firstID)
	if err != nil {
		t.Fatalf("load first provider: %v", err)
	}
	if firstRaw.IsDefault {
		t.Fatalf("successful default update did not clear previous default: %#v", firstRaw)
	}

	deleteStale := doJSON(t, server.Handler(), http.MethodDelete, "/api/providers/"+jsonNumber(secondID), `{"version":1}`)
	if deleteStale.Code != http.StatusConflict {
		t.Fatalf("stale provider delete status = %d, body=%s", deleteStale.Code, deleteStale.Body.String())
	}
	clearDefault := doJSON(t, server.Handler(), http.MethodPatch, "/api/providers/"+jsonNumber(secondID),
		`{"name":"Current","base_url":"`+validation.URL+`","api_key":"********","default_model":"model","is_default":false,"version":2}`)
	if clearDefault.Code != http.StatusOK {
		t.Fatalf("clear provider default status = %d, body=%s", clearDefault.Code, clearDefault.Body.String())
	}
	deleteCurrent := doJSON(t, server.Handler(), http.MethodDelete, "/api/providers/"+jsonNumber(secondID), `{"version":3}`)
	if deleteCurrent.Code != http.StatusNoContent {
		t.Fatalf("current provider delete status = %d, body=%s", deleteCurrent.Code, deleteCurrent.Body.String())
	}
}
