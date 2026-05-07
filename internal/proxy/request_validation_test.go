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
		_, _ = w.Write([]byte(`{"id":"resp-upstream","usage":{"input_tokens":1,"output_tokens":1},"output":[]}`))
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
		_, _ = w.Write([]byte(`{"id":"resp-upstream","usage":{"input_tokens":1,"output_tokens":1},"output":[]}`))
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
