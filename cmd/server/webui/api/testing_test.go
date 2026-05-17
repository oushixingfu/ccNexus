package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
