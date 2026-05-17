package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/providercompat"
	"github.com/lich0821/ccNexus/internal/storage"
)

type modelVerificationResult struct {
	Status              string
	UpstreamTransformer string
	FailureKind         string
	FailureMessage      string
	VerifiedTTL         time.Duration
	RetryTTL            time.Duration
}

type modelVerifier struct {
	client *http.Client
}

func newModelVerifier(client *http.Client) *modelVerifier {
	if client == nil {
		client = http.DefaultClient
	}
	return &modelVerifier{client: client}
}

func (v *modelVerifier) verifyEndpointModel(endpoint config.Endpoint, modelID string) modelVerificationResult {
	transformer := providercompat.NormalizeTransformer(endpoint.Transformer)
	if transformer == "auto" || transformer == "" {
		transformer = providercompat.InferEndpointTransformer(endpoint.APIUrl, modelID, endpoint.Transformer)
	}

	switch transformer {
	case providercompat.TransformerClaude:
		return v.verifyClaude(endpoint, modelID)
	case providercompat.TransformerOpenAI2:
		return v.verifyOpenAIResponses(endpoint, modelID, transformer)
	case providercompat.TransformerOpenAI, providercompat.TransformerDeepSeek, providercompat.TransformerKimi:
		return v.verifyOpenAIChat(endpoint, modelID, transformer)
	case providercompat.TransformerGemini:
		return v.verifyGemini(endpoint, modelID)
	default:
		return modelVerificationResult{
			Status:         storage.EndpointModelStatusFailed,
			FailureKind:    "unsupported_transformer",
			FailureMessage: transformer,
			RetryTTL:       7 * 24 * time.Hour,
		}
	}
}

func (v *modelVerifier) verifyOpenAIChat(endpoint config.Endpoint, modelID string, transformer string) modelVerificationResult {
	payload := []byte(fmt.Sprintf(`{"model":%q,"stream":false,"messages":[{"role":"user","content":"ping"}],"max_tokens":1}`, modelID))
	target := verificationTargetURL(endpoint.APIUrl, providercompat.OpenAIChatTargetPath(transformer, endpoint.APIUrl))
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return transientVerificationFailure("network_error", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(endpoint.APIKey))

	resp, err := v.client.Do(req)
	if err != nil {
		return transientVerificationFailure("network_error", err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classifyVerificationHTTPFailure(resp.StatusCode, readVerificationErrorBody(resp.Body))
	}

	var body struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || len(body.Choices) == 0 {
		return transientVerificationFailure("invalid_response", "missing chat choices")
	}
	return modelVerificationResult{Status: storage.EndpointModelStatusVerified, UpstreamTransformer: transformer, VerifiedTTL: 24 * time.Hour}
}

func (v *modelVerifier) verifyClaude(endpoint config.Endpoint, modelID string) modelVerificationResult {
	payload := []byte(fmt.Sprintf(`{"model":%q,"max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`, modelID))
	target := verificationTargetURL(endpoint.APIUrl, "/v1/messages")
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return transientVerificationFailure("network_error", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", strings.TrimSpace(endpoint.APIKey))
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(endpoint.APIKey))

	resp, err := v.client.Do(req)
	if err != nil {
		return transientVerificationFailure("network_error", err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classifyVerificationHTTPFailure(resp.StatusCode, readVerificationErrorBody(resp.Body))
	}

	var body struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || len(body.Content) == 0 {
		return transientVerificationFailure("invalid_response", "missing claude content")
	}
	return modelVerificationResult{Status: storage.EndpointModelStatusVerified, UpstreamTransformer: providercompat.TransformerClaude, VerifiedTTL: 24 * time.Hour}
}

func (v *modelVerifier) verifyOpenAIResponses(endpoint config.Endpoint, modelID string, transformer string) modelVerificationResult {
	payload := []byte(fmt.Sprintf(`{"model":%q,"stream":false,"input":[{"role":"user","content":[{"type":"input_text","text":"ping"}]}],"max_output_tokens":1}`, modelID))
	normalizedAPIURL := normalizeAPIUrl(strings.TrimRight(strings.TrimSpace(endpoint.APIUrl), "/"))
	if isCodexBackendBaseURL(normalizedAPIURL) {
		payload = ensureCodexResponsesPayload(payload)
	}
	target := verificationTargetURL(endpoint.APIUrl, "/v1/responses")
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return transientVerificationFailure("network_error", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(endpoint.APIKey))
	if isCodexBackendBaseURL(normalizedAPIURL) {
		ensureHeader(req.Header, "Version", codexClientVersion)
		ensureHeader(req.Header, "User-Agent", codexUserAgent)
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Originator", "codex_cli_rs")
	}

	resp, err := v.client.Do(req)
	if err != nil {
		return transientVerificationFailure("network_error", err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classifyVerificationHTTPFailure(resp.StatusCode, readVerificationErrorBody(resp.Body))
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil || !isValidOpenAIResponsesVerificationBody(bodyBytes, resp.Header.Get("Content-Type")) {
		return transientVerificationFailure("invalid_response", "missing responses output")
	}
	return modelVerificationResult{Status: storage.EndpointModelStatusVerified, UpstreamTransformer: transformer, VerifiedTTL: 24 * time.Hour}
}

func (v *modelVerifier) verifyGemini(endpoint config.Endpoint, modelID string) modelVerificationResult {
	payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"ping"}]}],"generationConfig":{"maxOutputTokens":1}}`)
	geminiModelID := strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	target := verificationTargetURL(endpoint.APIUrl, fmt.Sprintf("/v1beta/models/%s:generateContent", url.PathEscape(geminiModelID)))
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return transientVerificationFailure("network_error", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	q := req.URL.Query()
	q.Set("key", strings.TrimSpace(endpoint.APIKey))
	req.URL.RawQuery = q.Encode()

	resp, err := v.client.Do(req)
	if err != nil {
		return transientVerificationFailure("network_error", err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classifyVerificationHTTPFailure(resp.StatusCode, readVerificationErrorBody(resp.Body))
	}

	var body struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || len(body.Candidates) == 0 {
		return transientVerificationFailure("invalid_response", "missing gemini candidates")
	}
	return modelVerificationResult{Status: storage.EndpointModelStatusVerified, UpstreamTransformer: providercompat.TransformerGemini, VerifiedTTL: 24 * time.Hour}
}

func classifyVerificationHTTPFailure(statusCode int, body string) modelVerificationResult {
	lowerBody := strings.ToLower(strings.TrimSpace(body))
	if isUnsupportedModelHTTPFailure(statusCode, body) {
		return modelVerificationResult{Status: storage.EndpointModelStatusFailed, FailureKind: "unsupported_model", FailureMessage: body, RetryTTL: 7 * 24 * time.Hour}
	}
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return modelVerificationResult{Status: storage.EndpointModelStatusFailed, FailureKind: "auth_failed", FailureMessage: body, RetryTTL: 24 * time.Hour}
	case http.StatusTooManyRequests:
		return modelVerificationResult{Status: storage.EndpointModelStatusFailed, FailureKind: "quota_limited", FailureMessage: body, RetryTTL: 30 * time.Minute}
	case http.StatusBadRequest, http.StatusNotFound:
		kind := "unsupported_model"
		if lowerBody != "" &&
			!strings.Contains(lowerBody, "model") &&
			!strings.Contains(lowerBody, "not found") &&
			!strings.Contains(lowerBody, "invalid") &&
			!strings.Contains(lowerBody, "unsupported") {
			kind = "invalid_response"
		}
		return modelVerificationResult{Status: storage.EndpointModelStatusFailed, FailureKind: kind, FailureMessage: body, RetryTTL: 7 * 24 * time.Hour}
	default:
		if statusCode >= 500 {
			return modelVerificationResult{Status: storage.EndpointModelStatusFailed, FailureKind: "upstream_error", FailureMessage: body, RetryTTL: 10 * time.Minute}
		}
		return modelVerificationResult{Status: storage.EndpointModelStatusFailed, FailureKind: "invalid_response", FailureMessage: body, RetryTTL: 30 * time.Minute}
	}
}

func isUnsupportedModelHTTPFailure(statusCode int, body string) bool {
	if statusCode < http.StatusBadRequest {
		return false
	}
	lowerBody := strings.ToLower(strings.TrimSpace(body))
	if lowerBody == "" {
		return false
	}

	for _, marker := range []string{
		"model_not_found",
		"model_not_supported",
		"unsupported_model",
		"model not found",
		"model is not found",
		"model does not exist",
		"model doesn't exist",
		"invalid model",
	} {
		if strings.Contains(lowerBody, marker) {
			return true
		}
	}

	if strings.Contains(lowerBody, "no available channel") && strings.Contains(lowerBody, "model") {
		return true
	}
	if strings.Contains(lowerBody, "model") &&
		(strings.Contains(lowerBody, "not supported") || strings.Contains(lowerBody, "unsupported")) {
		return true
	}
	return false
}

func transientVerificationFailure(kind string, message string) modelVerificationResult {
	return modelVerificationResult{Status: storage.EndpointModelStatusFailed, FailureKind: kind, FailureMessage: message, RetryTTL: 10 * time.Minute}
}

func readVerificationErrorBody(body io.Reader) string {
	if body == nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(body, 2048))
	if err != nil {
		return ""
	}
	return string(data)
}

func verificationTargetURL(baseURL string, targetPath string) string {
	base := normalizeAPIUrl(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	return base + normalizeTargetPathForBaseURL(base, targetPath)
}

func isValidOpenAIResponsesVerificationBody(bodyBytes []byte, contentType string) bool {
	trimmed := bytes.TrimSpace(bodyBytes)
	if len(trimmed) == 0 {
		return false
	}
	lowerContentType := strings.ToLower(strings.TrimSpace(contentType))
	if strings.Contains(lowerContentType, "text/event-stream") || bytes.HasPrefix(trimmed, []byte("data:")) {
		return bytes.Contains(trimmed, []byte("response.completed")) ||
			bytes.Contains(trimmed, []byte("response.output_item.done")) ||
			bytes.Contains(trimmed, []byte("[DONE]"))
	}
	var body struct {
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	return json.Unmarshal(trimmed, &body) == nil && len(body.Output) > 0
}
