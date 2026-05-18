package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/storage"
)

func TestSendTestRequestFallsBackFromResponsesToChat(t *testing.T) {
	responsesCalls := 0
	chatCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/responses":
			responsesCalls++
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode responses body: %v", err)
			}
			if _, ok := payload["max_output_tokens"]; ok {
				t.Fatalf("manual Responses test should avoid max_output_tokens for gateway compatibility")
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"bad responses gateway"}}`))
		case "/v1/chat/completions":
			chatCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"},"finish_reason":"stop"}]}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	handler := &Handler{}
	endpoint := &storage.Endpoint{
		Name:                    "gpt",
		APIUrl:                  server.URL,
		APIKey:                  "sk-test",
		Transformer:             "openai2",
		Model:                   "gpt-5.5",
		SupportsOpenAIResponses: true,
		SupportsOpenAIChat:      true,
	}

	response, err := handler.sendTestRequest(endpoint)
	if err != nil {
		t.Fatalf("sendTestRequest returned error: %v", err)
	}
	if response != "pong" {
		t.Fatalf("expected fallback chat response pong, got %q", response)
	}
	if responsesCalls != 1 || chatCalls != 1 {
		t.Fatalf("expected one responses call and one chat call, got responses=%d chat=%d", responsesCalls, chatCalls)
	}
}

func TestEndpointTestRecordsSuccessAndClearsStaleFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	store := newEndpointAPITestStorage(t)
	defer store.Close()

	endpointName := "manual"
	if err := store.SaveEndpoint(&storage.Endpoint{
		Name:               endpointName,
		APIUrl:             upstream.URL,
		APIKey:             "sk-test",
		AuthMode:           config.AuthModeAPIKey,
		Enabled:            true,
		Transformer:        "openai",
		Model:              "gpt-5.5",
		AutoSelect:         true,
		SupportsOpenAIChat: true,
	}); err != nil {
		t.Fatalf("save endpoint: %v", err)
	}

	failureAt := time.Now().Add(-time.Minute).UTC()
	reason := "upstream_5xx"
	statusCode := http.StatusBadGateway
	if _, err := store.UpsertEndpointRuntimeStatus(endpointName, storage.EndpointRuntimeStatusPatch{
		LastFailureAt:         &failureAt,
		LastFailureReason:     &reason,
		LastFailureStatusCode: &statusCode,
	}); err != nil {
		t.Fatalf("seed runtime failure: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.BasicAuthEnabled = false
	handler := NewHandler(cfg, nil, store)
	req := httptest.NewRequest(http.MethodPost, "/api/endpoints/"+endpointName+"/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	var testResult map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &testResult); err != nil {
		t.Fatalf("decode test response: %v", err)
	}
	if testResult["success"] != true {
		t.Fatalf("expected successful test response, got %#v", testResult)
	}

	statuses, err := store.GetEndpointRuntimeStatuses()
	if err != nil {
		t.Fatalf("get runtime statuses: %v", err)
	}
	status := statuses[endpointName]
	if status == nil || status.LastSuccessAt == nil || !status.LastSuccessAt.After(failureAt) {
		t.Fatalf("expected later success status, got %#v", status)
	}
	if status.LastFailureReason != "" || status.LastFailureStatusCode != 0 {
		t.Fatalf("expected stale failure details cleared, got reason=%q status=%d", status.LastFailureReason, status.LastFailureStatusCode)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/endpoints", nil)
	listRec := httptest.NewRecorder()
	handler.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected list status 200, got %d body=%q", listRec.Code, listRec.Body.String())
	}
	var response SuccessResponse
	if err := json.Unmarshal(listRec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	data, ok := response.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("expected response data map, got %#v", response.Data)
	}
	rawEndpoints, ok := data["endpoints"].([]interface{})
	if !ok || len(rawEndpoints) != 1 {
		t.Fatalf("expected one endpoint, got %#v", data["endpoints"])
	}
	listed, ok := rawEndpoints[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected endpoint map, got %#v", rawEndpoints[0])
	}
	if listed["available"] != true || listed["availability"] != "available" {
		t.Fatalf("expected listed endpoint available, got %#v", listed)
	}
}

func TestEndpointTestPersistsChatFallbackForResponsesOnlyGateway(t *testing.T) {
	responsesCalls := 0
	chatCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/responses":
			responsesCalls++
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid character 'e' looking for beginning of value","type":"bad_response_body","code":"bad_response_body"}}`))
		case "/v1/chat/completions":
			chatCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"},"finish_reason":"stop"}]}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	store := newEndpointAPITestStorage(t)
	defer store.Close()

	endpointName := "GPT-1052"
	if err := store.SaveEndpoint(&storage.Endpoint{
		Name:                    endpointName,
		APIUrl:                  upstream.URL,
		APIKey:                  "sk-test",
		AuthMode:                config.AuthModeAPIKey,
		Enabled:                 true,
		Transformer:             "openai2",
		Model:                   "gpt-5.5",
		AutoSelect:              true,
		SupportsOpenAIResponses: true,
	}); err != nil {
		t.Fatalf("save endpoint: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.BasicAuthEnabled = false
	handler := NewHandler(cfg, nil, store)
	req := httptest.NewRequest(http.MethodPost, "/api/endpoints/"+endpointName+"/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	var testResult map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &testResult); err != nil {
		t.Fatalf("decode test response: %v", err)
	}
	if testResult["success"] != true {
		t.Fatalf("expected successful fallback test response, got %#v", testResult)
	}
	if responsesCalls != 1 || chatCalls != 1 {
		t.Fatalf("expected one responses and one chat call, got responses=%d chat=%d", responsesCalls, chatCalls)
	}

	endpoints, err := store.GetEndpoints()
	if err != nil {
		t.Fatalf("get endpoints: %v", err)
	}
	if len(endpoints) != 1 {
		t.Fatalf("expected one endpoint, got %d", len(endpoints))
	}
	updated := endpoints[0]
	if updated.Transformer != "openai2" || !updated.SupportsOpenAIChat || !updated.SupportsOpenAIResponses {
		t.Fatalf("expected chat fallback to add chat support without disabling responses, got transformer=%q chat=%t responses=%t",
			updated.Transformer,
			updated.SupportsOpenAIChat,
			updated.SupportsOpenAIResponses,
		)
	}
	if updated.PreferredOpenAIUpstream != "" {
		t.Fatalf("expected preferred OpenAI upstream to remain automatic, got %q", updated.PreferredOpenAIUpstream)
	}
}

func TestEndpointTestDoesNotPersistChatFallbackForGenericResponsesFailure(t *testing.T) {
	responsesCalls := 0
	chatCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/responses":
			responsesCalls++
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"temporary upstream failure"}}`))
		case "/v1/chat/completions":
			chatCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"},"finish_reason":"stop"}]}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	store := newEndpointAPITestStorage(t)
	defer store.Close()

	endpointName := "GPT-1052"
	if err := store.SaveEndpoint(&storage.Endpoint{
		Name:                    endpointName,
		APIUrl:                  upstream.URL,
		APIKey:                  "sk-test",
		AuthMode:                config.AuthModeAPIKey,
		Enabled:                 true,
		Transformer:             "openai2",
		Model:                   "gpt-5.5",
		AutoSelect:              true,
		SupportsOpenAIResponses: true,
	}); err != nil {
		t.Fatalf("save endpoint: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.BasicAuthEnabled = false
	handler := NewHandler(cfg, nil, store)
	req := httptest.NewRequest(http.MethodPost, "/api/endpoints/"+endpointName+"/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	var testResult map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &testResult); err != nil {
		t.Fatalf("decode test response: %v", err)
	}
	if testResult["success"] != false {
		t.Fatalf("expected failed test response, got %#v", testResult)
	}
	if responsesCalls != 1 || chatCalls != 0 {
		t.Fatalf("expected one responses call and no chat fallback, got responses=%d chat=%d", responsesCalls, chatCalls)
	}

	endpoints, err := store.GetEndpoints()
	if err != nil {
		t.Fatalf("get endpoints: %v", err)
	}
	updated := endpoints[0]
	if updated.Transformer != "openai2" || updated.SupportsOpenAIChat || !updated.SupportsOpenAIResponses {
		t.Fatalf("expected generic failure not to mutate protocol config, got transformer=%q chat=%t responses=%t",
			updated.Transformer,
			updated.SupportsOpenAIChat,
			updated.SupportsOpenAIResponses,
		)
	}
}

func TestEndpointTestDoesNotFallbackWhenAutoSelectDisabled(t *testing.T) {
	responsesCalls := 0
	chatCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/responses":
			responsesCalls++
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid character 'e' looking for beginning of value","type":"bad_response_body","code":"bad_response_body"}}`))
		case "/v1/chat/completions":
			chatCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"},"finish_reason":"stop"}]}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	store := newEndpointAPITestStorage(t)
	defer store.Close()

	endpointName := "manual-responses"
	if err := store.SaveEndpoint(&storage.Endpoint{
		Name:                    endpointName,
		APIUrl:                  upstream.URL,
		APIKey:                  "sk-test",
		AuthMode:                config.AuthModeAPIKey,
		Enabled:                 true,
		Transformer:             "openai2",
		Model:                   "gpt-5.5",
		AutoSelect:              false,
		SupportsOpenAIResponses: true,
	}); err != nil {
		t.Fatalf("save endpoint: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.BasicAuthEnabled = false
	handler := NewHandler(cfg, nil, store)
	req := httptest.NewRequest(http.MethodPost, "/api/endpoints/"+endpointName+"/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	var testResult map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &testResult); err != nil {
		t.Fatalf("decode test response: %v", err)
	}
	if testResult["success"] != false {
		t.Fatalf("expected failed test response, got %#v", testResult)
	}
	if responsesCalls != 1 || chatCalls != 0 {
		t.Fatalf("expected no fallback with auto-select disabled, got responses=%d chat=%d", responsesCalls, chatCalls)
	}
}

func TestBuildEndpointTestAttemptsPrefersEffectiveResponsesUpstream(t *testing.T) {
	endpoint := &storage.Endpoint{
		Name:                    "multi-protocol",
		APIUrl:                  "https://gateway.example.com",
		Transformer:             "openai",
		Model:                   "gpt-5.5",
		AutoSelect:              true,
		SupportsOpenAIResponses: true,
		SupportsOpenAIChat:      true,
	}

	attempts := buildEndpointTestAttempts(endpoint)
	if len(attempts) < 2 {
		t.Fatalf("expected at least responses and chat attempts, got %#v", attempts)
	}
	if attempts[0].transformer != "openai2" {
		t.Fatalf("expected endpoint test to validate effective Responses upstream first, got %#v", attempts)
	}
	if attempts[1].transformer != "openai" {
		t.Fatalf("expected chat fallback attempt second, got %#v", attempts)
	}
}

func TestSendTestRequestUsesKimiPromptAndTokenBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode chat body: %v", err)
		}
		messages, ok := payload["messages"].([]interface{})
		if !ok || len(messages) == 0 {
			t.Fatalf("expected messages payload, got %#v", payload["messages"])
		}
		first, _ := messages[0].(map[string]interface{})
		if first["content"] != endpointTestMessage {
			t.Fatalf("expected test prompt %q, got %#v", endpointTestMessage, first["content"])
		}
		if got := int(payload["max_tokens"].(float64)); got != endpointTestMaxTokens {
			t.Fatalf("expected max_tokens %d, got %d", endpointTestMaxTokens, got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	handler := &Handler{}
	endpoint := &storage.Endpoint{
		Name:        "kimi",
		APIUrl:      server.URL,
		APIKey:      "sk-test",
		Transformer: "kimi",
		Model:       "kimi-k2.6",
	}

	response, err := handler.sendTestRequest(endpoint)
	if err != nil {
		t.Fatalf("sendTestRequest returned error: %v", err)
	}
	if response != "pong" {
		t.Fatalf("expected kimi response pong, got %q", response)
	}
}

func TestHandleFetchModelsUsesStoredEndpointAPIKeyWhenMasked(t *testing.T) {
	tests := []struct {
		name        string
		transformer string
		model       string
		modelsJSON  string
		wantModels  []string
	}{
		{
			name:        "kimi local",
			transformer: "kimi",
			model:       "kimi-k2.6",
			modelsJSON:  `{"object":"list","data":[{"id":"kimi-k2.6","object":"model"},{"id":"kimi-k2.6-thinking","object":"model"}]}`,
			wantModels:  []string{"kimi-k2.6", "kimi-k2.6-thinking"},
		},
		{
			name:        "ds local",
			transformer: "deepseek",
			model:       "deepseek-v4-pro",
			modelsJSON:  `{"object":"list","data":[{"id":"deepseek-v4-flash","object":"model"},{"id":"deepseek-v4-pro","object":"model"}]}`,
			wantModels:  []string{"deepseek-v4-flash", "deepseek-v4-pro"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					t.Fatalf("unexpected path: %s", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer sk-real" {
					http.Error(w, "bad auth", http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.modelsJSON))
			}))
			defer upstream.Close()

			store := newEndpointAPITestStorage(t)
			defer store.Close()

			if err := store.SaveEndpoint(&storage.Endpoint{
				Name:        tt.name,
				APIUrl:      upstream.URL + "/v1",
				APIKey:      "sk-real",
				AuthMode:    config.AuthModeAPIKey,
				Enabled:     true,
				Transformer: tt.transformer,
				Model:       tt.model,
			}); err != nil {
				t.Fatalf("save endpoint: %v", err)
			}

			cfg := config.DefaultConfig()
			cfg.BasicAuthEnabled = false
			handler := NewHandler(cfg, nil, store)
			req := httptest.NewRequest(
				http.MethodPost,
				"/api/endpoints/fetch-models",
				strings.NewReader(`{"endpointName":"`+tt.name+`","apiUrl":"`+upstream.URL+`/v1","apiKey":"****","transformer":"auto"}`),
			)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected status 200, got %d body=%q", rec.Code, rec.Body.String())
			}
			var response SuccessResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			data, ok := response.Data.(map[string]interface{})
			if !ok {
				t.Fatalf("expected response data map, got %#v", response.Data)
			}
			rawModels, ok := data["models"].([]interface{})
			if !ok || len(rawModels) != len(tt.wantModels) {
				t.Fatalf("expected %d models, got %#v", len(tt.wantModels), data["models"])
			}
			for i, want := range tt.wantModels {
				if rawModels[i] != want {
					t.Fatalf("unexpected models: got %#v want %#v", rawModels, tt.wantModels)
				}
			}
		})
	}
}
