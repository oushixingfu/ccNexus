package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/storage"
)

func TestVerifyEndpointModelKimiUsesChatCompletionProbe(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("expected chat completions path, got %s", r.URL.Path)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["model"] != "kimi-k2.6" {
			t.Fatalf("expected probe model kimi-k2.6, got %#v", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	verifier := newModelVerifier(upstream.Client())
	result := verifier.verifyEndpointModel(config.Endpoint{Name: "kimi", APIUrl: upstream.URL, APIKey: "test-key", AuthMode: config.AuthModeAPIKey, Transformer: "kimi"}, "kimi-k2.6")
	if result.Status != storage.EndpointModelStatusVerified || result.UpstreamTransformer != "kimi" {
		t.Fatalf("unexpected verification result: %#v", result)
	}
}

func TestVerifyEndpointModelClassifiesUnsupportedModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"model not found","code":"model_not_found"}}`))
	}))
	defer upstream.Close()

	verifier := newModelVerifier(upstream.Client())
	result := verifier.verifyEndpointModel(config.Endpoint{Name: "openai", APIUrl: upstream.URL, APIKey: "test-key", AuthMode: config.AuthModeAPIKey, Transformer: "openai"}, "missing-model")
	if result.FailureKind != "unsupported_model" || result.Status != storage.EndpointModelStatusFailed {
		t.Fatalf("unexpected unsupported result: %#v", result)
	}
}

func TestVerifyEndpointModelTreatsTransientDatabaseLockAsUpstreamError(t *testing.T) {
	result := classifyVerificationHTTPFailure(http.StatusInternalServerError, `{"error":{"message":"database is locked (SQLITE_BUSY): model_not_found while routing request"}}`)
	if result.FailureKind == "unsupported_model" || result.RetryTTL >= 24*time.Hour {
		t.Fatalf("expected transient upstream failure, got %#v", result)
	}
	if isUnsupportedModelHTTPFailure(http.StatusInternalServerError, result.FailureMessage) {
		t.Fatalf("expected transient failure not to be treated as unsupported model")
	}
}

func TestVerifyEndpointModelOpenAIResponsesUsesStructuredInputProbe(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("expected responses path, got %s", r.URL.Path)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if _, ok := body["input"].([]interface{}); !ok {
			t.Fatalf("expected structured input array, got %#v", body["input"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
	}))
	defer upstream.Close()

	verifier := newModelVerifier(upstream.Client())
	result := verifier.verifyEndpointModel(config.Endpoint{Name: "codex", APIUrl: upstream.URL, APIKey: "test-key", AuthMode: config.AuthModeAPIKey, Transformer: "openai2"}, "gpt-5.5")
	if result.Status != storage.EndpointModelStatusVerified || result.UpstreamTransformer != "openai2" {
		t.Fatalf("unexpected verification result: %#v", result)
	}
}
