package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/logger"
	"github.com/lich0821/ccNexus/internal/storage"
)

func TestEnsureCodexResponsesPayload(t *testing.T) {
	raw := []byte(`{"model":"gpt-4.1","stream":true}`)
	out := ensureCodexResponsesPayload(raw)

	var payload map[string]interface{}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	store, ok := payload["store"].(bool)
	if !ok || store {
		t.Fatalf("expected store=false, got %#v", payload["store"])
	}
	stream, ok := payload["stream"].(bool)
	if !ok || !stream {
		t.Fatalf("expected stream=true, got %#v", payload["stream"])
	}
	if instructions, ok := payload["instructions"].(string); !ok || instructions != "" {
		t.Fatalf("expected instructions empty string, got %#v", payload["instructions"])
	}
}

func TestEnsureCodexResponsesPayloadOverridesStoreAndStream(t *testing.T) {
	raw := []byte(`{"model":"gpt-4.1","store":true}`)
	out := ensureCodexResponsesPayload(raw)

	var payload map[string]interface{}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	store, ok := payload["store"].(bool)
	if !ok || store {
		t.Fatalf("expected store=false, got %#v", payload["store"])
	}
	stream, ok := payload["stream"].(bool)
	if !ok || !stream {
		t.Fatalf("expected stream=true, got %#v", payload["stream"])
	}
}

func TestNormalizeTargetPathForBaseURLOnCodexBackend(t *testing.T) {
	got := normalizeTargetPathForBaseURL("https://chatgpt.com/backend-api/codex", "/v1/responses")
	if got != "/responses" {
		t.Fatalf("expected /responses, got %s", got)
	}
}

func TestNormalizeTargetPathForBaseURLAvoidsDuplicateV1(t *testing.T) {
	got := normalizeTargetPathForBaseURL("https://api.moonshot.ai/v1", "/v1/chat/completions")
	if got != "/chat/completions" {
		t.Fatalf("expected /chat/completions, got %s", got)
	}
}

func TestGetTargetPathUsesDeepSeekChatPath(t *testing.T) {
	ep := config.Endpoint{Transformer: "deepseek", APIUrl: "https://api.deepseek.com"}
	got := getTargetPath("/v1/messages", ep, []byte(`{}`), "cc_openai")
	if got != "/chat/completions" {
		t.Fatalf("expected /chat/completions, got %s", got)
	}
}

func TestGetTargetPathUsesV1ForCustomDeepSeekGateway(t *testing.T) {
	ep := config.Endpoint{Transformer: "deepseek", APIUrl: "https://gateway.example.com"}
	got := getTargetPath("/v1/responses", ep, []byte(`{}`), "cx_resp_openai")
	if got != "/v1/chat/completions" {
		t.Fatalf("expected /v1/chat/completions, got %s", got)
	}
}

func TestGetTargetPathPreservesResponsesCompactForOpenAI2(t *testing.T) {
	ep := config.Endpoint{Transformer: "openai2", APIUrl: "https://gateway.example.com/v1"}
	got := getTargetPath("/v1/responses/compact", ep, []byte(`{}`), "cx_resp_openai2")
	if got != "/v1/responses/compact" {
		t.Fatalf("expected /v1/responses/compact, got %s", got)
	}
}

func TestEndpointForClientFormatAutoSelectsRequestLocalUpstreams(t *testing.T) {
	endpoint := config.Endpoint{
		Name:                    "multi",
		Transformer:             "openai2",
		AutoSelect:              true,
		SupportsOpenAIResponses: true,
		SupportsClaudeMessages:  true,
		PreferredClaudeUpstream: "claude",
		Model:                   "gpt-5.5",
	}

	claudeUpstream := endpointForClientFormat(ClientFormatClaude, endpoint)
	responsesUpstream := endpointForClientFormat(ClientFormatOpenAIResponses, endpoint)

	if claudeUpstream.Transformer != "claude" {
		t.Fatalf("expected Claude client to use claude upstream, got %s", claudeUpstream.Transformer)
	}
	if responsesUpstream.Transformer != "openai2" {
		t.Fatalf("expected Responses client to use openai2 upstream, got %s", responsesUpstream.Transformer)
	}
	if endpoint.Transformer != "openai2" {
		t.Fatalf("auto selection must not mutate original endpoint, got %s", endpoint.Transformer)
	}
}

func TestEndpointForClientFormatInfersNativeOpenAI2Capability(t *testing.T) {
	endpoint := config.Endpoint{
		Name:        "responses-only",
		Transformer: "openai2",
		AutoSelect:  true,
		Model:       "gpt-5.5",
	}

	claudeUpstream := endpointForClientFormat(ClientFormatClaude, endpoint)
	if claudeUpstream.Transformer != "openai2" {
		t.Fatalf("expected Claude client to use inferred openai2 upstream, got %s", claudeUpstream.Transformer)
	}
}

func TestEndpointForClientFormatPrefersNativeOpenAI2ForClaudeWithoutPreference(t *testing.T) {
	endpoint := config.Endpoint{
		Name:                    "responses-first",
		Transformer:             "openai2",
		AutoSelect:              true,
		SupportsOpenAIResponses: true,
		SupportsClaudeMessages:  true,
		Model:                   "gpt-5.5",
	}

	claudeUpstream := endpointForClientFormat(ClientFormatClaude, endpoint)
	if claudeUpstream.Transformer != "openai2" {
		t.Fatalf("expected Claude client to use native openai2 upstream, got %s", claudeUpstream.Transformer)
	}
}

func TestEndpointForClientFormatPrefersResponsesForGPTClaudeClientWhenAvailable(t *testing.T) {
	endpoint := config.Endpoint{
		Name:                    "chat-capable-gateway",
		Transformer:             "openai2",
		AutoSelect:              true,
		SupportsOpenAIResponses: true,
		SupportsOpenAIChat:      true,
		Model:                   "gpt-5.5",
	}

	claudeUpstream := endpointForClientFormat(ClientFormatClaude, endpoint)
	if claudeUpstream.Transformer != "openai2" {
		t.Fatalf("expected Claude client with GPT endpoint model to prefer responses upstream, got %s", claudeUpstream.Transformer)
	}
}

func TestEndpointForClientFormatHonorsExplicitClaudeResponsesPreference(t *testing.T) {
	endpoint := config.Endpoint{
		Name:                    "responses-preferred",
		Transformer:             "openai2",
		AutoSelect:              true,
		SupportsOpenAIResponses: true,
		SupportsOpenAIChat:      true,
		PreferredClaudeUpstream: "openai2",
		Model:                   "gpt-5.5",
	}

	claudeUpstream := endpointForClientFormat(ClientFormatClaude, endpoint)
	if claudeUpstream.Transformer != "openai2" {
		t.Fatalf("expected explicit Claude preference to use openai2, got %s", claudeUpstream.Transformer)
	}
}

func TestProtocolFallbackResponsesUnsupportedParameterFallsBackToChat(t *testing.T) {
	endpoint := config.Endpoint{
		Name:                    "gateway",
		Transformer:             "openai2",
		AutoSelect:              true,
		SupportsOpenAIResponses: true,
		SupportsOpenAIChat:      true,
		Model:                   "gpt-5.5",
	}
	upstreamEndpoint := endpoint
	upstreamEndpoint.Transformer = "openai2"

	got := protocolFallbackTransformerForHTTPFailure(
		ClientFormatClaude,
		endpoint,
		upstreamEndpoint,
		"cc_openai2",
		http.StatusBadRequest,
		`{"error":{"message":"Unsupported parameter: max_output_tokens"}}`,
	)
	if got != "openai" {
		t.Fatalf("expected fallback to openai chat, got %q", got)
	}
}

func TestHandleProxyClaudeClientUsesResponsesForGPTGatewayWhenAvailable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("expected responses upstream path, got %s", r.URL.Path)
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("failed to decode upstream payload: %v", err)
		}
		if payload["model"] != "gpt-5.5" {
			t.Fatalf("expected endpoint model override, got %#v", payload["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_test","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"responses ok"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`))
	}))
	defer upstream.Close()

	endpoint := failoverPolicyTestEndpoint("gateway", upstream.URL)
	endpoint.Transformer = "openai2"
	endpoint.Model = "gpt-5.5"
	endpoint.AutoSelect = true
	endpoint.SupportsOpenAIResponses = true
	endpoint.SupportsOpenAIChat = true
	p := newFailoverPolicyTestProxy([]config.Endpoint{endpoint}, upstream.Client())

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5-20250929","max_tokens":16,"stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "responses ok") {
		t.Fatalf("expected Claude-format response converted from responses, got %q", rec.Body.String())
	}
}

func TestHandleProxyResponsesFallsBackToClaudeEndpointWhenNoCodexEndpointAvailable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("expected Claude upstream path, got %s", r.URL.Path)
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("failed to decode upstream payload: %v", err)
		}
		if payload["model"] != "claude-sonnet-4-5-20250929" {
			t.Fatalf("expected endpoint model override, got %#v", payload["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_test","type":"message","role":"assistant","content":[{"type":"text","text":"claude fallback ok"}],"usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	defer upstream.Close()

	endpoint := failoverPolicyTestEndpoint("claude-only", upstream.URL)
	endpoint.Transformer = "claude"
	endpoint.Model = "claude-sonnet-4-5-20250929"
	endpoint.AutoSelect = true
	endpoint.SupportsClaudeMessages = true
	p := newFailoverPolicyTestProxy([]config.Endpoint{endpoint}, upstream.Client())

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":false,"input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "claude fallback ok") {
		t.Fatalf("expected responses-format body converted from Claude, got %q", rec.Body.String())
	}
}

func TestHandleProxyClaudeFallsBackToCodexEndpointWhenNoClaudeEndpointAvailable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("expected responses upstream path, got %s", r.URL.Path)
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("failed to decode upstream payload: %v", err)
		}
		if payload["model"] != "gpt-5.5" {
			t.Fatalf("expected endpoint model override, got %#v", payload["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_test","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"codex fallback ok"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`))
	}))
	defer upstream.Close()

	endpoint := failoverPolicyTestEndpoint("codex-only", upstream.URL)
	endpoint.Transformer = "openai2"
	endpoint.Model = "gpt-5.5"
	endpoint.AutoSelect = true
	endpoint.SupportsOpenAIResponses = true
	p := newFailoverPolicyTestProxy([]config.Endpoint{endpoint}, upstream.Client())

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5-20250929","max_tokens":16,"stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "codex fallback ok") {
		t.Fatalf("expected Claude-format body converted from responses, got %q", rec.Body.String())
	}
}

func TestHandleProxyResponsesUsesKimiBeforeClaudeWhenNoCodexEndpointAvailable(t *testing.T) {
	kimi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("expected Kimi chat upstream path, got %s", r.URL.Path)
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("failed to decode upstream payload: %v", err)
		}
		if payload["model"] != "kimi-k2.6" {
			t.Fatalf("expected endpoint model override, got %#v", payload["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-kimi","choices":[{"message":{"role":"assistant","content":"kimi middle ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	}))
	defer kimi.Close()

	claude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("expected Kimi endpoint to be selected before Claude, got path %s", r.URL.Path)
	}))
	defer claude.Close()

	kimiEndpoint := failoverPolicyTestEndpoint("kimi", kimi.URL)
	kimiEndpoint.Transformer = "kimi"
	kimiEndpoint.Model = "kimi-k2.6"
	kimiEndpoint.AutoSelect = true
	kimiEndpoint.SupportsOpenAIChat = true
	claudeEndpoint := failoverPolicyTestEndpoint("claude", claude.URL)
	claudeEndpoint.Transformer = "claude"
	claudeEndpoint.Model = "claude-sonnet-4-5-20250929"
	claudeEndpoint.AutoSelect = true
	claudeEndpoint.SupportsClaudeMessages = true
	p := newFailoverPolicyTestProxy([]config.Endpoint{claudeEndpoint, kimiEndpoint}, kimi.Client())

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":false,"input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "kimi middle ok") {
		t.Fatalf("expected responses-format body converted from Kimi, got %q", rec.Body.String())
	}
}

func TestHandleProxyClaudeUsesKimiBeforeCodexWhenNoClaudeEndpointAvailable(t *testing.T) {
	kimi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("expected Kimi chat upstream path, got %s", r.URL.Path)
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("failed to decode upstream payload: %v", err)
		}
		if payload["model"] != "kimi-k2.6" {
			t.Fatalf("expected endpoint model override, got %#v", payload["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-kimi","choices":[{"message":{"role":"assistant","content":"kimi claude ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	}))
	defer kimi.Close()

	codex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("expected Kimi endpoint to be selected before codex, got path %s", r.URL.Path)
	}))
	defer codex.Close()

	codexEndpoint := failoverPolicyTestEndpoint("codex", codex.URL)
	codexEndpoint.Transformer = "openai2"
	codexEndpoint.Model = "gpt-5.5"
	codexEndpoint.AutoSelect = true
	codexEndpoint.SupportsOpenAIResponses = true
	kimiEndpoint := failoverPolicyTestEndpoint("kimi", kimi.URL)
	kimiEndpoint.Transformer = "kimi"
	kimiEndpoint.Model = "kimi-k2.6"
	kimiEndpoint.AutoSelect = true
	kimiEndpoint.SupportsOpenAIChat = true
	p := newFailoverPolicyTestProxy([]config.Endpoint{codexEndpoint, kimiEndpoint}, kimi.Client())

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5-20250929","max_tokens":16,"stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "kimi claude ok") {
		t.Fatalf("expected Claude-format body converted from Kimi, got %q", rec.Body.String())
	}
}

func TestHandleProxyAutoSelectOneEndpointServesClaudeAndResponsesConcurrently(t *testing.T) {
	paths := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/messages":
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"claude ok"}],"usage":{"input_tokens":2,"output_tokens":2}}`))
		case "/v1/responses":
			_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"responses ok"}]}],"usage":{"input_tokens":2,"output_tokens":2,"total_tokens":4}}`))
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	endpoint := failoverPolicyTestEndpoint("multi", upstream.URL)
	endpoint.AutoSelect = true
	endpoint.SupportsOpenAIResponses = true
	endpoint.SupportsClaudeMessages = true
	endpoint.PreferredClaudeUpstream = "claude"
	p := newFailoverPolicyTestProxy([]config.Endpoint{endpoint}, upstream.Client())

	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan string, 2)

	go func() {
		defer wg.Done()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5-20250929","max_tokens":16,"stream":false,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		p.handleProxy(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "claude ok") {
			errs <- "claude request failed: status=" + http.StatusText(rec.Code) + " body=" + rec.Body.String()
		}
	}()

	go func() {
		defer wg.Done()
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":false,"input":"hi"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		p.handleProxy(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "responses ok") {
			errs <- "responses request failed: status=" + http.StatusText(rec.Code) + " body=" + rec.Body.String()
		}
	}()

	wg.Wait()
	close(errs)
	for errMsg := range errs {
		t.Fatal(errMsg)
	}
	close(paths)

	seen := map[string]bool{}
	for p := range paths {
		seen[p] = true
	}
	if !seen["/v1/messages"] || !seen["/v1/responses"] {
		t.Fatalf("expected one endpoint to serve both upstream paths, got %#v", seen)
	}
}

func TestHandleProxyResponsesToCustomDeepSeekUsesV1ChatPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("failed to decode upstream payload: %v", err)
		}
		if payload["model"] != "deepseek-v4-pro" {
			t.Fatalf("expected endpoint model override, got %#v", payload["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints([]config.Endpoint{
		{
			Name:        "DeepSeekGateway",
			APIUrl:      upstream.URL,
			APIKey:      "test-key",
			AuthMode:    config.AuthModeAPIKey,
			Enabled:     true,
			Transformer: "deepseek",
			Model:       "deepseek-v4-pro",
		},
	})

	p := &Proxy{
		config:         cfg,
		stats:          NewStats(&noopStatsStorage{}, "test-device"),
		httpClient:     upstream.Client(),
		activeRequests: make(map[string]int),
		endpointCtx:    make(map[string]context.Context),
		endpointCancel: make(map[string]context.CancelFunc),
		currentIndex:   0,
		resolver:       NewEndpointResolverWithFunc(cfg.GetEndpoints),
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":false,"input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"object":"response"`) {
		t.Fatalf("expected responses-format body, got %q", rec.Body.String())
	}
}

func TestHandleProxyChatToCustomDeepSeekUsesEndpointModel(t *testing.T) {
	logger.GetLogger().Clear()
	logger.GetLogger().SetMinLevel(logger.DEBUG)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("failed to decode upstream payload: %v", err)
		}
		if payload["model"] != "deepseek-v4-pro" {
			t.Fatalf("expected endpoint model override, got %#v", payload["model"])
		}
		if stream, ok := payload["stream"].(bool); !ok || !stream {
			t.Fatalf("expected stream=true to be preserved, got %#v", payload["stream"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints([]config.Endpoint{
		{
			Name:        "1052-2nd",
			APIUrl:      upstream.URL,
			APIKey:      "test-key",
			AuthMode:    config.AuthModeAPIKey,
			Enabled:     true,
			Transformer: "deepseek",
			Model:       "deepseek-v4-pro",
		},
	})

	p := &Proxy{
		config:         cfg,
		stats:          NewStats(&noopStatsStorage{}, "test-device"),
		httpClient:     upstream.Client(),
		activeRequests: make(map[string]int),
		endpointCtx:    make(map[string]context.Context),
		endpointCancel: make(map[string]context.CancelFunc),
		currentIndex:   0,
		resolver:       NewEndpointResolverWithFunc(cfg.GetEndpoints),
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d body=%q", rec.Code, rec.Body.String())
	}

	logs := logger.GetLogger().GetLogs()
	joined := ""
	for _, entry := range logs {
		joined += entry.Message + "\n"
	}
	for _, want := range []string{
		"Model mapping: client_model=gpt-5.5 upstream_model=deepseek-v4-pro",
		"Streaming deepseek-v4-pro",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected logs to contain %q; logs:\n%s", want, joined)
		}
	}
}

func TestHandleProxyFiltersByVerifiedModelThenPreservesEndpointPriority(t *testing.T) {
	primaryHits := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits++
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("expected Kimi chat upstream path, got %s", r.URL.Path)
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode primary payload: %v", err)
		}
		if payload["model"] != "gpt-5.5" {
			t.Fatalf("expected requested model to be preserved, got %#v", payload["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-primary","choices":[{"message":{"role":"assistant","content":"primary ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer primary.Close()

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("fallback should not be used while primary supports requested model")
	}))
	defer fallback.Close()

	primaryEndpoint := failoverPolicyTestEndpoint("Primary", primary.URL)
	primaryEndpoint.Transformer = "kimi"
	primaryEndpoint.Model = "legacy-kimi"
	fallbackEndpoint := failoverPolicyTestEndpoint("Fallback", fallback.URL)
	fallbackEndpoint.Transformer = "openai2"
	fallbackEndpoint.Model = "legacy-codex"
	p := newFailoverPolicyTestProxy([]config.Endpoint{primaryEndpoint, fallbackEndpoint}, primary.Client())
	p.modelRegistry = newModelRegistry(&fakeEndpointModelStore{models: []storage.EndpointModel{
		{EndpointName: "Primary", ModelID: "gpt-5.5", Enabled: true, VerificationStatus: storage.EndpointModelStatusVerified, UpstreamTransformer: "kimi"},
		{EndpointName: "Fallback", ModelID: "gpt-5.5", Enabled: true, VerificationStatus: storage.EndpointModelStatusVerified, UpstreamTransformer: "openai2"},
	}})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.5","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	if primaryHits != 1 {
		t.Fatalf("expected primary hit once, got %d", primaryHits)
	}
}

func TestHandleProxyEndpointSelectorModelSuffixUsesVerifiedModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("failed to decode upstream payload: %v", err)
		}
		if payload["model"] != "deepseek-v4-pro" {
			t.Fatalf("expected selector suffix model, got %#v", payload["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	other := failoverPolicyTestEndpoint("other", "https://unused.example.com")
	other.Transformer = "openai"
	other.Model = "gpt-4.1"
	selected := failoverPolicyTestEndpoint("1052-2nd", upstream.URL)
	selected.Transformer = "deepseek"
	selected.Model = "fallback-model"
	p := newFailoverPolicyTestProxy([]config.Endpoint{other, selected}, upstream.Client())
	p.modelRegistry = newModelRegistry(&fakeEndpointModelStore{models: []storage.EndpointModel{
		{EndpointName: "1052-2nd", ModelID: "deepseek-v4-pro", Enabled: true, VerificationStatus: storage.EndpointModelStatusVerified, UpstreamTransformer: "deepseek"},
	}})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"@1052-2nd/deepseek-v4-pro","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-ccNexus-Endpoint"); got != "1052-2nd" {
		t.Fatalf("expected request to select 1052-2nd, got %q", got)
	}
}

func TestHandleProxyMarksVerifiedEndpointModelFailedWhenUpstreamModelNotFound(t *testing.T) {
	primaryHits := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"model_not_found","message":"No available channel for model claude-opus-4-7 under group primary"}}`))
	}))
	defer primary.Close()

	fallbackHits := 0
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits++
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("expected Claude upstream path, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_ok","type":"message","role":"assistant","content":[{"type":"text","text":"fallback ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer fallback.Close()

	primaryEndpoint := failoverPolicyTestEndpoint("Primary", primary.URL)
	primaryEndpoint.Transformer = "claude"
	primaryEndpoint.Model = "claude-opus-4-7"
	fallbackEndpoint := failoverPolicyTestEndpoint("Fallback", fallback.URL)
	fallbackEndpoint.Transformer = "claude"
	fallbackEndpoint.Model = "claude-opus-4-7"
	p := newFailoverPolicyTestProxy([]config.Endpoint{primaryEndpoint, fallbackEndpoint}, primary.Client())
	store := &fakeEndpointModelStore{models: []storage.EndpointModel{
		{EndpointName: "Primary", ModelID: "claude-opus-4-7", Source: storage.EndpointModelSourceLegacy, Enabled: true, VerificationStatus: storage.EndpointModelStatusVerified, UpstreamTransformer: "claude"},
		{EndpointName: "Fallback", ModelID: "claude-opus-4-7", Source: storage.EndpointModelSourceLegacy, Enabled: true, VerificationStatus: storage.EndpointModelStatusVerified, UpstreamTransformer: "claude"},
	}}
	p.modelRegistry = newModelRegistry(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-4-7","max_tokens":16,"stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected fallback success, got %d body=%q", rec.Code, rec.Body.String())
	}
	if primaryHits != 1 || fallbackHits != 1 {
		t.Fatalf("expected one primary model failure then fallback, got primary=%d fallback=%d", primaryHits, fallbackHits)
	}
	model, ok := store.endpointModel("Primary", "claude-opus-4-7")
	if !ok {
		t.Fatal("expected primary endpoint model to remain stored")
	}
	if model.VerificationStatus != storage.EndpointModelStatusFailed {
		t.Fatalf("expected primary model status failed, got %#v", model)
	}
	if model.FailureKind != "unsupported_model" {
		t.Fatalf("expected unsupported_model failure kind, got %#v", model)
	}
	if !strings.Contains(model.FailureMessage, "model_not_found") {
		t.Fatalf("expected upstream failure summary to be stored, got %#v", model.FailureMessage)
	}
	if model.LastAttemptAt == nil || model.NextAttemptAt == nil {
		t.Fatalf("expected retry timestamps to be set, got %#v", model)
	}
	if time.Until(*model.NextAttemptAt) < 6*24*time.Hour {
		t.Fatalf("expected unsupported model retry to be delayed about a week, got %#v", model.NextAttemptAt)
	}
}

func TestOverrideModelInPayload(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.3-codex","stream":true}`)
	out := overrideModelInPayload(raw, "gpt-5.2-codex")

	var payload map[string]interface{}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if payload["model"] != "gpt-5.2-codex" {
		t.Fatalf("expected model override to gpt-5.2-codex, got %#v", payload["model"])
	}
}

func TestEnforceEndpointModelInPayloadSkipsGeminiBody(t *testing.T) {
	endpoint := config.Endpoint{Model: "gemini-2.0-flash"}
	raw := []byte(`{"contents":[]}`)
	out := enforceEndpointModelInPayload(raw, endpoint, "cx_chat_gemini")
	if string(out) != string(raw) {
		t.Fatalf("expected Gemini body to be unchanged, got %s", string(out))
	}
}

func TestExtractModelFromPayload(t *testing.T) {
	got := extractModelFromPayload([]byte(`{"model":"deepseek-v4-pro","messages":[]}`))
	if got != "deepseek-v4-pro" {
		t.Fatalf("expected deepseek-v4-pro, got %q", got)
	}
}

func TestValidateClientJSONRequestBody(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "object", body: `{"model":"gpt-5.5"}`},
		{name: "empty", body: ``, wantErr: true},
		{name: "whitespace", body: `   `, wantErr: true},
		{name: "truncated", body: `{"model":`, wantErr: true},
		{name: "array", body: `[]`, wantErr: true},
		{name: "null", body: `null`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateClientJSONRequestBody([]byte(tt.body))
			if tt.wantErr && err == nil {
				t.Fatal("expected validation error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("did not expect validation error: %v", err)
			}
		})
	}
}

func TestHandleProxyInvalidBodyLogIncludesMethodAndPath(t *testing.T) {
	logger.GetLogger().Clear()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses?token=hidden", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	(&Proxy{}).handleProxy(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400, got %d", rec.Code)
	}

	for _, entry := range logger.GetLogger().GetLogs() {
		if !strings.Contains(entry.Message, "Invalid request body") {
			continue
		}
		if !strings.Contains(entry.Message, "method=POST") {
			t.Fatalf("expected invalid-body log to include method, got %q", entry.Message)
		}
		if !strings.Contains(entry.Message, "path=/v1/responses") {
			t.Fatalf("expected invalid-body log to include path, got %q", entry.Message)
		}
		if strings.Contains(entry.Message, "token=hidden") {
			t.Fatalf("expected invalid-body log to omit query string, got %q", entry.Message)
		}
		return
	}

	t.Fatal("expected invalid-body log entry")
}

func TestForceStreamInPayloadAddsChatUsageOptions(t *testing.T) {
	raw := []byte(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}]}`)
	out := forceStreamInPayload(raw)

	var payload map[string]interface{}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if stream, ok := payload["stream"].(bool); !ok || !stream {
		t.Fatalf("expected stream=true, got %#v", payload["stream"])
	}
	streamOptions, ok := payload["stream_options"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected stream_options object, got %#v", payload["stream_options"])
	}
	if includeUsage, ok := streamOptions["include_usage"].(bool); !ok || !includeUsage {
		t.Fatalf("expected include_usage=true, got %#v", streamOptions["include_usage"])
	}
}

func TestInjectEndpointThinkingInPayloadAddsResponsesEffortLevel(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.5","stream":true,"input":[]}`)
	out := injectEndpointThinkingInPayload(raw, "cx_resp_openai2", "xhigh", "https://1052.cc.cd:5005", "gpt-5.5")

	var payload map[string]interface{}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if payload["effortLevel"] != "xhigh" {
		t.Fatalf("expected effortLevel=xhigh, got %#v", payload["effortLevel"])
	}
	if _, ok := payload["reasoning"]; ok {
		t.Fatalf("did not expect reasoning object, got %#v", payload["reasoning"])
	}
}

func TestInjectEndpointThinkingInPayloadKeepsOfficialOpenAIReasoning(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.5","stream":true,"input":[]}`)
	out := injectEndpointThinkingInPayload(raw, "cx_resp_openai2", "High", "https://api.openai.com/v1", "gpt-5.5")

	var payload map[string]interface{}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	reasoning, ok := payload["reasoning"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected reasoning object, got %#v", payload["reasoning"])
	}
	if reasoning["effort"] != "high" {
		t.Fatalf("expected reasoning.effort=high, got %#v", reasoning["effort"])
	}
	if _, ok := payload["effortLevel"]; ok {
		t.Fatalf("did not expect effortLevel for official OpenAI, got %#v", payload["effortLevel"])
	}
}

func TestInjectEndpointThinkingInPayloadAddsChatReasoningEffort(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	out := injectEndpointThinkingInPayload(raw, "cx_chat_openai", "xhigh", "", "")

	var payload map[string]interface{}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if payload["reasoning_effort"] != "xhigh" {
		t.Fatalf("expected reasoning_effort=xhigh, got %#v", payload["reasoning_effort"])
	}
	if _, ok := payload["reasoning"]; ok {
		t.Fatalf("did not expect responses reasoning on chat payload, got %#v", payload["reasoning"])
	}
}

func TestInjectEndpointThinkingInPayloadSkipsOff(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.5","stream":true,"input":[]}`)
	out := injectEndpointThinkingInPayload(raw, "cx_resp_openai2", "off", "", "")

	var payload map[string]interface{}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if _, ok := payload["effortLevel"]; ok {
		t.Fatalf("did not expect effortLevel when thinking is off, got %#v", payload["effortLevel"])
	}
}

func TestShouldHandleAsStreamingResponseForCodexWithoutContentType(t *testing.T) {
	endpoint := config.Endpoint{
		Name:        "TokenPool",
		APIUrl:      "https://chatgpt.com/backend-api/codex",
		Transformer: "openai2",
	}
	if !shouldHandleAsStreamingResponse("", true, endpoint, "cx_chat_openai2") {
		t.Fatal("expected stream=true Codex response with empty content-type to be treated as streaming")
	}
	if shouldHandleAsStreamingResponse("", false, endpoint, "cx_chat_openai2") {
		t.Fatal("expected non-stream client request to not be treated as streaming when content-type is empty")
	}
	if !shouldHandleAsStreamingResponse("text/event-stream", false, endpoint, "cx_chat_openai2") {
		t.Fatal("expected text/event-stream content-type to be treated as streaming")
	}
}

func TestShouldHandleAsStreamingResponseForForcedOpenAIStreamWithoutContentType(t *testing.T) {
	endpoint := config.Endpoint{
		Name:        "Forced",
		APIUrl:      "https://gateway.example.com",
		Transformer: "openai2",
		ForceStream: true,
	}
	if !shouldHandleAsStreamingResponse("", true, endpoint, "cx_resp_openai2") {
		t.Fatal("expected forced OpenAI stream with empty content-type to be treated as streaming")
	}
	if shouldHandleAsStreamingResponse("", false, endpoint, "cx_resp_openai2") {
		t.Fatal("expected non-stream client request to not be treated as streaming when content-type is empty")
	}
}
