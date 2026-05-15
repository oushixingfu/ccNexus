package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lich0821/ccNexus/internal/config"
)

func TestHandleProxyRejectsInvalidJSONBeforeEndpointAttempt(t *testing.T) {
	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-upstream","usage":{"input_tokens":1,"output_tokens":1},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
	}))
	defer upstream.Close()

	p := newFailoverPolicyTestProxy([]config.Endpoint{
		failoverPolicyTestEndpoint("Primary", upstream.URL),
	}, upstream.Client())

	tests := []struct {
		name string
		body string
	}{
		{name: "empty body", body: ""},
		{name: "malformed json", body: `{"model":"gpt-5.5"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstreamHits = 0
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			p.handleProxy(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected status 400, got %d body=%q", rec.Code, rec.Body.String())
			}
			if upstreamHits != 0 {
				t.Fatalf("expected invalid request to skip upstream endpoints, got hits=%d", upstreamHits)
			}
			if !strings.Contains(rec.Body.String(), "invalid_request_error") {
				t.Fatalf("expected structured invalid request response, got %q", rec.Body.String())
			}
		})
	}
}

func TestHandleProxyRejectsResponsesRequestMissingInputBeforeEndpointAttempt(t *testing.T) {
	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-upstream","usage":{"input_tokens":1,"output_tokens":1},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
	}))
	defer upstream.Close()

	p := newFailoverPolicyTestProxy([]config.Endpoint{
		failoverPolicyTestEndpoint("Primary", upstream.URL),
	}, upstream.Client())

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.handleProxy(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d body=%q", rec.Code, rec.Body.String())
	}
	if upstreamHits != 0 {
		t.Fatalf("expected missing input to skip upstream endpoints, got hits=%d", upstreamHits)
	}
	if !strings.Contains(rec.Body.String(), "field input is required") {
		t.Fatalf("expected missing input error, got %q", rec.Body.String())
	}
}

func TestHandleProxyNormalizesToolRoleInResponsesInputForOpenAI2Upstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("failed to decode upstream request: %v", err)
		}

		input, ok := body["input"].([]interface{})
		if !ok {
			t.Fatalf("expected input array, got %#v", body["input"])
		}
		for _, rawItem := range input {
			item, ok := rawItem.(map[string]interface{})
			if !ok {
				continue
			}
			if item["role"] == "tool" {
				t.Fatalf("did not expect role=tool to reach upstream: %#v", item)
			}
		}

		toolOutput := input[1].(map[string]interface{})
		if toolOutput["type"] != "function_call_output" {
			t.Fatalf("expected function_call_output, got %#v", toolOutput)
		}
		if toolOutput["call_id"] != "call_1" {
			t.Fatalf("expected call_id=call_1, got %#v", toolOutput["call_id"])
		}
		if toolOutput["output"] != "牧原股份基本面数据" {
			t.Fatalf("expected normalized tool output text, got %#v", toolOutput["output"])
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-upstream","usage":{"input_tokens":1,"output_tokens":1},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
	}))
	defer upstream.Close()

	p := newFailoverPolicyTestProxy([]config.Endpoint{
		failoverPolicyTestEndpoint("Primary", upstream.URL),
	}, upstream.Client())

	body := `{
		"model":"gpt-5.5",
		"stream":false,
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"lookup"}]},
			{"type":"message","role":"tool","tool_call_id":"call_1","content":"牧原股份基本面数据"}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected upstream success, got status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestHandleProxyAutoForceStreamsOpenAI2WhenUpstreamRequiresStream(t *testing.T) {
	var upstreamHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("failed to decode upstream request: %v", err)
		}

		stream, _ := body["stream"].(bool)
		if upstreamHits == 1 {
			if stream {
				t.Fatalf("first upstream attempt should preserve non-stream client request, got stream=true")
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"400: {\"detail\":\"Stream must be set to true\"}","type":"bad_response_status_code","code":"bad_response_status_code"}}`))
			return
		}
		if !stream {
			t.Fatalf("second upstream attempt should force stream=true, got body=%#v", body)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"type":"response.completed","response":{"id":"resp-stream","object":"response","status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}}`,
			"",
			"data: [DONE]",
			"",
		}, "\n")))
	}))
	defer upstream.Close()

	p := newFailoverPolicyTestProxy([]config.Endpoint{
		failoverPolicyTestEndpoint("Primary", upstream.URL),
	}, upstream.Client())

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":false,"input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected forced streaming retry to succeed, got status=%d body=%q", rec.Code, rec.Body.String())
	}
	if upstreamHits != 2 {
		t.Fatalf("expected one compatibility retry, got upstream hits=%d", upstreamHits)
	}
	if !strings.Contains(rec.Body.String(), `"id":"resp-stream"`) {
		t.Fatalf("expected aggregated Responses payload, got %q", rec.Body.String())
	}
}

func TestHandleProxyAutoForceStreamsOpenAI2WhenUpstreamReturnsBadResponseBody(t *testing.T) {
	var upstreamHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("failed to decode upstream request: %v", err)
		}

		stream, _ := body["stream"].(bool)
		if upstreamHits == 1 {
			if stream {
				t.Fatalf("first upstream attempt should preserve non-stream client request, got stream=true")
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid character 'e' looking for beginning of value","type":"bad_response_body","code":"bad_response_body"}}`))
			return
		}
		if !stream {
			t.Fatalf("second upstream attempt should force stream=true, got body=%#v", body)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`event: response.created`,
			`data: {"response":{"id":"resp-stream","object":"response","status":"in_progress"}}`,
			"",
			`event: response.output_text.delta`,
			`data: {"output_index":0,"content_index":0,"delta":"ok"}`,
			"",
			`event: response.completed`,
			`data: {"response":{"id":"resp-stream","object":"response","status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5},"output":[]}}`,
			"",
		}, "\n")))
	}))
	defer upstream.Close()

	p := newFailoverPolicyTestProxy([]config.Endpoint{
		failoverPolicyTestEndpoint("Primary", upstream.URL),
	}, upstream.Client())

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":false,"input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected forced streaming retry to succeed, got status=%d body=%q", rec.Code, rec.Body.String())
	}
	if upstreamHits != 2 {
		t.Fatalf("expected one compatibility retry, got upstream hits=%d", upstreamHits)
	}
	if !strings.Contains(rec.Body.String(), `"id":"resp-stream"`) ||
		!strings.Contains(rec.Body.String(), `"text":"ok"`) {
		t.Fatalf("expected aggregated Responses payload with patched text, got %q", rec.Body.String())
	}
}

func TestHandleProxyTreatsWrappedInvalidRequest500AsClientError(t *testing.T) {
	var upstreamHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"field messages is required","type":"new_api_error","code":"invalid_request"}}`))
	}))
	defer upstream.Close()

	p := newFailoverPolicyTestProxy([]config.Endpoint{
		failoverPolicyTestEndpoint("Primary", upstream.URL),
	}, upstream.Client())

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":false,"input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.handleProxy(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected wrapped invalid_request to be returned as 400, got %d body=%q", rec.Code, rec.Body.String())
	}
	if upstreamHits != 1 {
		t.Fatalf("expected wrapped invalid_request not to retry, got hits=%d", upstreamHits)
	}
	if !strings.Contains(rec.Body.String(), "invalid_request") {
		t.Fatalf("expected upstream invalid request body, got %q", rec.Body.String())
	}
	p.cooldownMu.RLock()
	cooldowns := len(p.endpointCooldowns)
	p.cooldownMu.RUnlock()
	if cooldowns != 0 {
		t.Fatalf("expected no endpoint cooldown for client invalid request, got %d", cooldowns)
	}
}

func TestHandleProxyFallsBackResponsesToKimiChatOnMissingMessages(t *testing.T) {
	var upstreamPaths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPaths = append(upstreamPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/responses" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"field messages is required","type":"new_api_error","code":"invalid_request"}}`))
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected fallback path: %s", r.URL.Path)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("failed to decode fallback request: %v", err)
		}
		if body["model"] != "kimi-k2.6" {
			t.Fatalf("expected kimi model override, got %#v", body["model"])
		}
		messages, ok := body["messages"].([]interface{})
		if !ok || len(messages) < 2 {
			t.Fatalf("expected fallback chat messages, got %#v", body["messages"])
		}
		first, ok := messages[0].(map[string]interface{})
		if !ok {
			t.Fatalf("expected first fallback message object, got %#v", messages[0])
		}
		if first["role"] != "system" {
			t.Fatalf("expected Kimi fallback to convert developer role to system, got %#v", first["role"])
		}
		_, _ = w.Write([]byte(`{"id":"chatcmpl-kimi","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	endpoint := failoverPolicyTestEndpoint("Kimi", upstream.URL)
	endpoint.Model = "kimi-k2.6"
	p := newFailoverPolicyTestProxy([]config.Endpoint{endpoint}, upstream.Client())

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":false,"input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"policy"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected fallback to succeed, got %d body=%q", rec.Code, rec.Body.String())
	}
	if len(upstreamPaths) != 2 || upstreamPaths[0] != "/v1/responses" || upstreamPaths[1] != "/v1/chat/completions" {
		t.Fatalf("expected responses then chat fallback, got %#v", upstreamPaths)
	}
	updated := p.config.GetEndpoints()[0]
	if !updated.AutoSelect || !updated.SupportsOpenAIChat || updated.SupportsOpenAIResponses {
		t.Fatalf("expected fallback success to persist chat capabilities, got %#v", updated)
	}
	if updated.Transformer != "kimi" {
		t.Fatalf("expected fallback to persist kimi transformer, got %s", updated.Transformer)
	}
}
