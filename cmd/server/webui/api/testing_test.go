package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
