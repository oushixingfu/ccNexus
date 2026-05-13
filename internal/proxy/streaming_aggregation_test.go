package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/transformer/cc"
)

func TestHandleStreamingAsNonStreamingAggregatesOpenAIChatChunks(t *testing.T) {
	rawSSE := strings.Join([]string{
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1710000000,"model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant","content":"hello "},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1710000000,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"world","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\""}}]},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1710000000,"model":"gpt-test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"codex\"}"}}]},"finish_reason":"tool_calls"}]}`,
		"",
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1710000000,"model":"gpt-test","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(rawSSE)),
	}
	rec := httptest.NewRecorder()
	p := &Proxy{}

	in, out, text, err := p.handleStreamingAsNonStreaming(rec, resp, config.Endpoint{Name: "OpenAI"}, &passthroughResponseTransformer{}, 0)
	if err != nil {
		t.Fatalf("handleStreamingAsNonStreaming failed: %v", err)
	}
	if in != 11 || out != 7 {
		t.Fatalf("expected usage in=11 out=7, got in=%d out=%d", in, out)
	}
	if text != "hello world" {
		t.Fatalf("unexpected output text: %q", text)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not json: %v", err)
	}
	if payload["object"] != "chat.completion" {
		t.Fatalf("expected chat.completion object, got %#v", payload["object"])
	}

	choices := payload["choices"].([]interface{})
	message := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	if message["content"] != "hello world" {
		t.Fatalf("unexpected message content: %#v", message["content"])
	}
	toolCalls := message["tool_calls"].([]interface{})
	function := toolCalls[0].(map[string]interface{})["function"].(map[string]interface{})
	if function["name"] != "lookup" || function["arguments"] != `{"q":"codex"}` {
		t.Fatalf("unexpected tool call function: %#v", function)
	}
}

func TestHandleStreamingAsNonStreamingBackfillsResponsesOutputFromDelta(t *testing.T) {
	rawSSE := strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning","summary":[]},"output_index":0}`,
		"",
		`data: {"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":"pong"}`,
		"",
		`data: {"type":"response.output_text.done","output_index":1,"content_index":0,"text":"pong"}`,
		"",
		`data: {"type":"response.completed","response":{"id":"resp-stream","object":"response","status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5},"output":[]}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(rawSSE)),
	}
	rec := httptest.NewRecorder()
	p := &Proxy{}

	in, out, text, err := p.handleStreamingAsNonStreaming(rec, resp, config.Endpoint{Name: "OpenAI2"}, &passthroughResponseTransformer{}, 0)
	if err != nil {
		t.Fatalf("handleStreamingAsNonStreaming failed: %v", err)
	}
	if in != 2 || out != 3 {
		t.Fatalf("expected usage in=2 out=3, got in=%d out=%d", in, out)
	}
	if text != "pong" {
		t.Fatalf("unexpected output text: %q", text)
	}
	if !strings.Contains(rec.Body.String(), `"text":"pong"`) {
		t.Fatalf("expected patched Responses output text, got %q", rec.Body.String())
	}
}

func TestHandleStreamingAsNonStreamingBackfillsResponsesFunctionCall(t *testing.T) {
	rawSSE := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","call_id":"call_1","type":"function_call","name":"read_file","arguments":"","status":"in_progress"}}`,
		"",
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"path\":\""}`,
		"",
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"AGENTS.md\"}"}`,
		"",
		`data: {"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"path\":\"AGENTS.md\"}"}`,
		"",
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","call_id":"call_1","type":"function_call","name":"read_file","arguments":"{\"path\":\"AGENTS.md\"}","status":"completed"}}`,
		"",
		`data: {"type":"response.completed","response":{"id":"resp-tool","object":"response","status":"completed","usage":{"input_tokens":8,"output_tokens":4,"total_tokens":12},"output":[]}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(rawSSE)),
	}
	rec := httptest.NewRecorder()
	p := &Proxy{}
	trans := cc.NewOpenAI2TransformerWithAPIURL("gpt-5.5", "", "https://example.com")

	in, out, text, err := p.handleStreamingAsNonStreaming(rec, resp, config.Endpoint{Name: "OpenAI2"}, trans, 0)
	if err != nil {
		t.Fatalf("handleStreamingAsNonStreaming failed: %v", err)
	}
	if in != 8 || out != 4 {
		t.Fatalf("expected usage in=8 out=4, got in=%d out=%d", in, out)
	}
	if text != "" {
		t.Fatalf("unexpected text output for tool call: %q", text)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"tool_use"`) ||
		!strings.Contains(body, `"name":"read_file"`) ||
		!strings.Contains(body, `"path":"AGENTS.md"`) {
		t.Fatalf("expected Claude tool_use response, got %q", body)
	}
}

func TestHandleStreamingAsNonStreamingSynthesizesResponsesPayloadFromDeltaWithoutCompleted(t *testing.T) {
	rawSSE := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp-stream","object":"response","status":"in_progress"}}`,
		"",
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"pong"}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(rawSSE)),
	}
	rec := httptest.NewRecorder()
	p := &Proxy{}

	_, _, text, err := p.handleStreamingAsNonStreaming(rec, resp, config.Endpoint{Name: "OpenAI2"}, &passthroughResponseTransformer{}, 0)
	if err != nil {
		t.Fatalf("handleStreamingAsNonStreaming failed: %v", err)
	}
	if text != "pong" {
		t.Fatalf("unexpected output text: %q", text)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"object":"response"`) ||
		!strings.Contains(body, `"status":"completed"`) ||
		!strings.Contains(body, `"text":"pong"`) {
		t.Fatalf("expected synthesized Responses payload, got %q", body)
	}
}
