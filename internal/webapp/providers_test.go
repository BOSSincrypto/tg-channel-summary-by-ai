package webapp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/boss/tg-channel-summary-by-ai/internal/db"
	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

type providerValidationTransport struct {
	requests []*http.Request
	status   int
}

func (t *providerValidationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.requests = append(t.requests, request.Clone(request.Context()))
	status := t.status
	if status == 0 {
		status = http.StatusOK
	}
	body := `{"choices":[{"message":{"content":"ok"}}]}`
	if status != http.StatusOK {
		body = `{"error":{"message":"invalid API key"}}`
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}, nil
}

func TestAuthenticatedProviderAPIRequiresHTTPSBeforeTransportAndPersistsHTTPSProvider(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	transport := &providerValidationTransport{}
	client := &http.Client{Transport: transport}
	auth, err := NewWebAppAuth("unit-bot-token", "715602446")
	if err != nil {
		t.Fatalf("create auth: %v", err)
	}
	server := NewWithProvidersAuthenticated(store, time.Second, client, auth)
	postProvider := func(baseURL, key string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/providers", strings.NewReader(
			`{"name":"Injected-`+key+`","base_url":"`+baseURL+`","api_key":"`+key+`","default_model":"model"}`,
		))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(initDataHeader, signedInitData("unit-bot-token", "715602446", time.Now()))
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		return recorder
	}

	insecure := postProvider("http://provider.example.test/v1", "cleartext-key")
	if insecure.Code != http.StatusBadRequest {
		t.Fatalf("HTTP provider status = %d, want 400, body=%s", insecure.Code, insecure.Body.String())
	}
	if len(transport.requests) != 0 {
		t.Fatalf("HTTP provider validation made %d transport requests, want zero", len(transport.requests))
	}
	var count int
	if err := store.Conn().QueryRow(`SELECT COUNT(*) FROM ai_providers`).Scan(&count); err != nil {
		t.Fatalf("count providers after HTTP rejection: %v", err)
	}
	if count != 0 {
		t.Fatalf("provider count after HTTP rejection = %d, want zero", count)
	}

	secure := postProvider("https://provider.example.test/v1", "https-key")
	if secure.Code != http.StatusCreated {
		t.Fatalf("HTTPS provider status = %d, want 201, body=%s", secure.Code, secure.Body.String())
	}
	if len(transport.requests) != 1 {
		t.Fatalf("HTTPS provider validation made %d transport requests, want one", len(transport.requests))
	}
	request := transport.requests[0]
	if request.URL.Scheme != "https" {
		t.Fatalf("validated request scheme = %q, want https", request.URL.Scheme)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer https-key" {
		t.Fatalf("HTTPS authorization header = %q, want HTTPS key", got)
	}
	if err := store.Conn().QueryRow(`SELECT COUNT(*) FROM ai_providers`).Scan(&count); err != nil {
		t.Fatalf("count providers after HTTPS validation: %v", err)
	}
	if count != 1 {
		t.Fatalf("provider count after HTTPS validation = %d, want one", count)
	}

	updateRequest := httptest.NewRequest(http.MethodPatch, "/api/providers/1", strings.NewReader(
		`{"name":"Updated","base_url":"http://provider.example.test/v1","api_key":"update-key","default_model":"model","version":1}`,
	))
	updateRequest.Header.Set("Content-Type", "application/json")
	updateRequest.Header.Set(initDataHeader, signedInitData("unit-bot-token", "715602446", time.Now()))
	updateRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(updateRecorder, updateRequest)
	if updateRecorder.Code != http.StatusBadRequest {
		t.Fatalf("HTTP provider update status = %d, want 400, body=%s", updateRecorder.Code, updateRecorder.Body.String())
	}
	if len(transport.requests) != 1 {
		t.Fatalf("HTTP provider update made %d total transport requests, want one", len(transport.requests))
	}
	current, err := store.Providers.GetByID(1)
	if err != nil {
		t.Fatalf("load provider after HTTP update rejection: %v", err)
	}
	if current.Name != "Injected-https-key" || current.APIKey != "https-key" {
		t.Fatalf("HTTP provider update changed persisted provider: %#v", current)
	}

	transport.status = http.StatusUnauthorized
	failed := postProvider("https://provider.example.test/v1", "failed-key")
	if failed.Code != http.StatusBadRequest {
		t.Fatalf("failed HTTPS provider status = %d, want 400, body=%s", failed.Code, failed.Body.String())
	}
	if len(transport.requests) != 2 {
		t.Fatalf("failed HTTPS validation made %d total transport requests, want two", len(transport.requests))
	}
	if err := store.Conn().QueryRow(`SELECT COUNT(*) FROM ai_providers`).Scan(&count); err != nil {
		t.Fatalf("count providers after failed HTTPS validation: %v", err)
	}
	if count != 1 {
		t.Fatalf("provider count after failed HTTPS validation = %d, want one", count)
	}
}

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

func TestProviderDefaultTransitionInvalidatesPreviousVersionAtHTTPBoundary(t *testing.T) {
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
		body := `{"name":"` + name + `","base_url":"` + validation.URL +
			`","api_key":"unit-provider-secret","default_model":"model","is_default":` +
			map[bool]string{true: "true", false: "false"}[isDefault] + `}`
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

	previous := create("Previous", true)
	next := create("Next", false)
	previousID := int64(previous["id"].(float64))
	nextID := int64(next["id"].(float64))
	previousVersion := int64(previous["version"].(float64))
	nextVersion := int64(next["version"].(float64))

	transition := doJSON(t, server.Handler(), http.MethodPatch, "/api/providers/"+jsonNumber(nextID),
		`{"name":"Next","base_url":"`+validation.URL+`","api_key":"********","default_model":"model","is_default":true,"version":`+jsonNumber(nextVersion)+`}`)
	if transition.Code != http.StatusOK {
		t.Fatalf("default transition status = %d, body=%s", transition.Code, transition.Body.String())
	}

	previousAfter, err := store.Providers.GetByID(previousID)
	if err != nil {
		t.Fatalf("load previous provider after transition: %v", err)
	}
	nextAfter, err := store.Providers.GetByID(nextID)
	if err != nil {
		t.Fatalf("load next provider after transition: %v", err)
	}
	if previousAfter.IsDefault || previousAfter.Version != previousVersion+1 {
		t.Fatalf("previous provider after transition = %#v, want cleared version %d", previousAfter, previousVersion+1)
	}
	if !nextAfter.IsDefault || nextAfter.Version != nextVersion+1 {
		t.Fatalf("next provider after transition = %#v, want default version %d", nextAfter, nextVersion+1)
	}

	stale := doJSON(t, server.Handler(), http.MethodPatch, "/api/providers/"+jsonNumber(previousID),
		`{"name":"Stale mutation","base_url":"`+validation.URL+`","api_key":"********","default_model":"changed","is_default":false,"version":`+jsonNumber(previousVersion)+`}`)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale previous-default status = %d, body=%s", stale.Code, stale.Body.String())
	}
	previousAfterStale, err := store.Providers.GetByID(previousID)
	if err != nil {
		t.Fatalf("load previous provider after stale mutation: %v", err)
	}
	if *previousAfterStale != *previousAfter {
		t.Fatalf("stale mutation changed previous provider: before=%#v after=%#v", previousAfter, previousAfterStale)
	}

	refreshed := doJSON(t, server.Handler(), http.MethodPatch, "/api/providers/"+jsonNumber(previousID),
		`{"name":"Refreshed mutation","base_url":"`+validation.URL+`","api_key":"********","default_model":"changed","is_default":false,"version":`+jsonNumber(previousAfter.Version)+`}`)
	if refreshed.Code != http.StatusOK {
		t.Fatalf("refreshed previous-default status = %d, body=%s", refreshed.Code, refreshed.Body.String())
	}
	previousFinal, err := store.Providers.GetByID(previousID)
	if err != nil {
		t.Fatalf("load previous provider after refreshed mutation: %v", err)
	}
	if previousFinal.Name != "Refreshed mutation" || previousFinal.IsDefault || previousFinal.Version != previousAfter.Version+1 {
		t.Fatalf("previous provider after refreshed mutation = %#v", previousFinal)
	}

	var defaults int
	if err := store.Conn().QueryRow(`SELECT COUNT(*) FROM ai_providers WHERE is_default = 1`).Scan(&defaults); err != nil {
		t.Fatalf("count default providers: %v", err)
	}
	if defaults != 1 {
		t.Fatalf("default provider count = %d, want exactly one", defaults)
	}
}

func TestProviderHTTPDuplicateNameIdentifiesNameField(t *testing.T) {
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
	first := doJSON(t, server.Handler(), http.MethodPost, "/api/providers",
		`{"name":"First","base_url":"`+validation.URL+`","api_key":"unit-provider-key","default_model":"model"}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first provider status = %d, body=%s", first.Code, first.Body.String())
	}
	duplicate := doJSON(t, server.Handler(), http.MethodPost, "/api/providers",
		`{"name":"fIrSt","base_url":"`+validation.URL+`","api_key":"unit-provider-key","default_model":"model"}`)
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d, body=%s", duplicate.Code, duplicate.Body.String())
	}
	var response map[string]string
	if err := json.Unmarshal(duplicate.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode duplicate response: %v", err)
	}
	if response["field"] != "name" || response["error"] != "Провайдер с таким именем уже существует" {
		t.Fatalf("duplicate response = %#v, want name field error", response)
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
