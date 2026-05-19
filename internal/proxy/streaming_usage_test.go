package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/transformer"
)

type noUsageStreamTransformer struct{}

func (t *noUsageStreamTransformer) Name() string {
	return "test_no_usage"
}

func (t *noUsageStreamTransformer) TransformRequest(claudeReq []byte) ([]byte, error) {
	return claudeReq, nil
}

func (t *noUsageStreamTransformer) TransformResponse(targetResp []byte, isStreaming bool) ([]byte, error) {
	return targetResp, nil
}

func (t *noUsageStreamTransformer) TransformResponseWithContext(targetResp []byte, isStreaming bool, ctx *transformer.StreamContext) ([]byte, error) {
	if !isStreaming {
		return targetResp, nil
	}
	// Simulate transformer that drops usage data.
	return []byte("data: {\"type\":\"response.completed\"}\n\n"), nil
}

func TestHandleStreamingResponseExtractsUsageFromOriginalEvent(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints([]config.Endpoint{
		{
			Name:        "TokenPool",
			APIUrl:      "https://example.com",
			APIKey:      "x",
			AuthMode:    config.AuthModeAPIKey,
			Enabled:     true,
			Transformer: "openai2",
			Model:       "gpt-4.1",
		},
	})

	p := &Proxy{config: cfg}
	endpoint := cfg.GetEndpoints()[0]
	originalSSE := strings.Join([]string{
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":7,"output_tokens":5,"total_tokens":12}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(originalSSE)),
	}
	rec := httptest.NewRecorder()

	result := p.handleStreamingResponse(
		context.Background(),
		rec,
		resp,
		endpoint,
		&noUsageStreamTransformer{},
		"cc_openai2",
		false,
		"gpt-4.1",
		[]byte(`{}`),
		0,
		nil,
		"",
	)
	in, out := result.InputTokens, result.OutputTokens

	if in != 7 || out != 5 {
		t.Fatalf("expected tokens from original stream usage in=7 out=5, got in=%d out=%d", in, out)
	}
}

func TestHandleStreamingResponseSynthesizesResponsesCompletedBeforeDone(t *testing.T) {
	cfg := config.DefaultConfig()
	endpoint := config.Endpoint{
		Name:        "OpenAI2",
		APIUrl:      "https://example.com",
		APIKey:      "x",
		AuthMode:    config.AuthModeAPIKey,
		Enabled:     true,
		Transformer: "openai2",
		Model:       "gpt-5.5",
	}
	cfg.UpdateEndpoints([]config.Endpoint{endpoint})

	p := &Proxy{config: cfg}
	originalSSE := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_passthrough","object":"response","status":"in_progress"}}`,
		"",
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"ok"}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(originalSSE)),
	}
	rec := httptest.NewRecorder()

	result := p.handleStreamingResponse(
		context.Background(),
		rec,
		resp,
		endpoint,
		&passthroughResponseTransformer{},
		"cx_resp_openai2",
		false,
		"gpt-5.5",
		[]byte(`{"model":"gpt-5.5","stream":true,"input":[]}`),
		0,
		nil,
		"",
	)

	if result.Err != nil {
		t.Fatalf("expected synthesized completion to succeed, got err=%v", result.Err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"response.completed"`) {
		t.Fatalf("expected synthetic response.completed, got %q", body)
	}
	if !strings.Contains(body, `"id":"resp_passthrough"`) || !strings.Contains(body, `"text":"ok"`) {
		t.Fatalf("expected synthetic completion to preserve id/text, got %q", body)
	}
	if strings.Index(body, `"type":"response.completed"`) > strings.Index(body, "data: [DONE]") {
		t.Fatalf("expected response.completed before [DONE], got %q", body)
	}
}

func TestHandleStreamingResponseTreatsGenericResponsesToolEventsAsSemantic(t *testing.T) {
	cfg := config.DefaultConfig()
	endpoint := config.Endpoint{
		Name:        "OpenAI2",
		APIUrl:      "https://example.com",
		APIKey:      "x",
		AuthMode:    config.AuthModeAPIKey,
		Enabled:     true,
		Transformer: "openai2",
		Model:       "gpt-5.5",
	}
	cfg.UpdateEndpoints([]config.Endpoint{endpoint})

	p := &Proxy{config: cfg}
	originalSSE := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_tool","object":"response","status":"in_progress"}}`,
		"",
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"ctc_1","call_id":"call_1","type":"custom_tool_call","name":"read_file","input":"","status":"in_progress"}}`,
		"",
		`data: {"type":"response.custom_tool_call_input.delta","output_index":0,"delta":"{\"path\":\"AGENTS.md\"}"}`,
		"",
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"ctc_1","call_id":"call_1","type":"custom_tool_call","name":"read_file","input":"{\"path\":\"AGENTS.md\"}","status":"completed"}}`,
		"",
		`data: {"type":"response.completed","response":{"id":"resp_tool","object":"response","status":"completed","usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7},"output":[]}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(originalSSE)),
	}
	rec := httptest.NewRecorder()

	result := p.handleStreamingResponse(
		context.Background(),
		rec,
		resp,
		endpoint,
		&passthroughResponseTransformer{},
		"cx_resp_openai2",
		false,
		"gpt-5.5",
		[]byte(`{"model":"gpt-5.5","stream":true,"input":[]}`),
		0,
		nil,
		"",
	)

	if result.Err != nil {
		t.Fatalf("expected generic tool stream to succeed, got err=%v", result.Err)
	}
	if !result.WroteSemanticData {
		t.Fatalf("expected generic tool event to count as semantic data, got %#v", result)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"custom_tool_call"`) ||
		!strings.Contains(body, `"type":"response.completed"`) ||
		!strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected full tool stream to be forwarded, got %q", body)
	}
}

func TestHandleStreamingResponseAppendsDoneAfterResponsesCompletedEOF(t *testing.T) {
	cfg := config.DefaultConfig()
	endpoint := config.Endpoint{
		Name:        "OpenAI2",
		APIUrl:      "https://example.com",
		APIKey:      "x",
		AuthMode:    config.AuthModeAPIKey,
		Enabled:     true,
		Transformer: "openai2",
		Model:       "gpt-5.5",
	}
	cfg.UpdateEndpoints([]config.Endpoint{endpoint})

	p := &Proxy{config: cfg}
	originalSSE := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"output_index":0,"content_index":0,"delta":"ok"}`,
		"",
		`event: response.completed`,
		`data: {"response":{"id":"resp_done_eof","object":"response","status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5},"output":[]}}`,
		"",
	}, "\n")

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(originalSSE)),
	}
	rec := httptest.NewRecorder()

	result := p.handleStreamingResponse(
		context.Background(),
		rec,
		resp,
		endpoint,
		&passthroughResponseTransformer{},
		"cx_resp_openai2",
		false,
		"gpt-5.5",
		[]byte(`{"model":"gpt-5.5","stream":true,"input":[]}`),
		0,
		nil,
		"",
	)

	if result.Err != nil {
		t.Fatalf("expected EOF after response.completed to succeed, got err=%v", result.Err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"response.completed"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected response.completed plus appended [DONE], got %q", body)
	}
}

func TestHandleStreamingResponseTreatsEOFBeforeResponsesCompletedAsSyntheticSuccess(t *testing.T) {
	cfg := config.DefaultConfig()
	endpoint := config.Endpoint{
		Name:        "OpenAI2",
		APIUrl:      "https://example.com",
		APIKey:      "x",
		AuthMode:    config.AuthModeAPIKey,
		Enabled:     true,
		Transformer: "openai2",
		Model:       "gpt-5.5",
	}
	cfg.UpdateEndpoints([]config.Endpoint{endpoint})

	p := &Proxy{config: cfg}
	originalSSE := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_truncated","object":"response","status":"in_progress"}}`,
		"",
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"partial"}`,
		"",
	}, "\n")

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(originalSSE)),
	}
	rec := httptest.NewRecorder()

	result := p.handleStreamingResponse(
		context.Background(),
		rec,
		resp,
		endpoint,
		&passthroughResponseTransformer{},
		"cx_resp_openai2",
		false,
		"gpt-5.5",
		[]byte(`{"model":"gpt-5.5","stream":true,"input":[]}`),
		0,
		nil,
		"",
	)

	if result.Err != nil {
		t.Fatalf("expected synthetic completion after semantic EOF to succeed, got result=%#v", result)
	}
	if result.Reason != streamFinishCompleted || !result.Completed {
		t.Fatalf("expected completed result after synthetic completion, got %#v", result)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"type":"response.completed"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected synthetic terminal events for client compatibility, got %q", body)
	}
}
