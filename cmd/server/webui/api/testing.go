package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/logger"
	"github.com/lich0821/ccNexus/internal/providercompat"
	"github.com/lich0821/ccNexus/internal/proxy"
	"github.com/lich0821/ccNexus/internal/storage"
)

const (
	endpointTestMessage   = "ping"
	endpointTestMaxTokens = 64
)

// testEndpoint tests an endpoint's connectivity
func (h *Handler) testEndpoint(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		WriteError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	// Get endpoint
	endpoints, err := h.storage.GetEndpoints()
	if err != nil {
		logger.Error("Failed to get endpoints: %v", err)
		WriteError(w, http.StatusInternalServerError, "Failed to get endpoints")
		return
	}

	var endpoint *storage.Endpoint
	for i := range endpoints {
		if endpoints[i].Name == name {
			endpoint = &endpoints[i]
			break
		}
	}

	if endpoint == nil {
		WriteError(w, http.StatusNotFound, "Endpoint not found")
		return
	}

	// Test the endpoint
	start := time.Now()
	response, err := h.sendTestRequest(endpoint)
	latency := time.Since(start).Milliseconds()

	if err != nil {
		WriteJSON(w, http.StatusOK, map[string]interface{}{
			"success": false,
			"latency": latency,
			"error":   err.Error(),
		})
		return
	}

	WriteJSON(w, http.StatusOK, map[string]interface{}{
		"success":  true,
		"latency":  latency,
		"response": response,
	})
}

// sendTestRequest sends a test request to an endpoint
func (h *Handler) sendTestRequest(endpoint *storage.Endpoint) (string, error) {
	apiKey, authErr := h.resolveEndpointAPIKey(endpoint)
	if authErr != nil {
		return "", authErr
	}

	var reqBody []byte
	var url string
	var err error

	normalizedURL := providercompat.NormalizeBaseURL(endpoint.APIUrl)
	transformer := providercompat.NormalizeTransformer(endpoint.Transformer)

	switch transformer {
	case "claude":
		url = providercompat.JoinBaseURLAndPath(normalizedURL, "/v1/messages")
		reqBody, err = json.Marshal(map[string]interface{}{
			"model": "claude-3-5-sonnet-20241022",
			"messages": []map[string]interface{}{
				{
					"role":    "user",
					"content": endpointTestMessage,
				},
			},
			"max_tokens": endpointTestMaxTokens,
		})
	case "openai", "deepseek", "kimi":
		url = providercompat.JoinBaseURLAndPath(normalizedURL, providercompat.OpenAIChatTargetPath(transformer, normalizedURL))
		model := endpoint.Model
		if model == "" {
			model = providercompat.DefaultModel(transformer)
		}
		reqBody, err = json.Marshal(map[string]interface{}{
			"model": model,
			"messages": []map[string]interface{}{
				{
					"role":    "user",
					"content": endpointTestMessage,
				},
			},
			"max_tokens": endpointTestMaxTokens,
		})
	case "openai2":
		url = providercompat.JoinBaseURLAndPath(normalizedURL, "/v1/responses")
		model := endpoint.Model
		if model == "" {
			model = providercompat.DefaultModel(transformer)
		}
		reqBody, err = json.Marshal(map[string]interface{}{
			"model":             model,
			"stream":            true,
			"max_output_tokens": endpointTestMaxTokens,
			"input": []map[string]interface{}{
				{
					"type": "message",
					"role": "user",
					"content": []map[string]interface{}{
						{"type": "input_text", "text": endpointTestMessage},
					},
				},
			},
		})
	case "gemini":
		model := endpoint.Model
		if model == "" {
			model = providercompat.DefaultModel(transformer)
		}
		url = providercompat.JoinBaseURLAndPath(normalizedURL, fmt.Sprintf("/v1beta/models/%s:generateContent", model))
		reqBody, err = json.Marshal(map[string]interface{}{
			"contents": []map[string]interface{}{
				{
					"parts": []map[string]interface{}{
						{
							"text": endpointTestMessage,
						},
					},
				},
			},
			"generationConfig": map[string]int{"maxOutputTokens": endpointTestMaxTokens},
		})
	default:
		return "", fmt.Errorf("unsupported transformer: %s", endpoint.Transformer)
	}

	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %v", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	// Add authentication based on transformer
	switch transformer {
	case "claude":
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	case "openai", "openai2", "deepseek", "kimi":
		req.Header.Set("Authorization", "Bearer "+apiKey)
		if transformer == "openai2" {
			req.Header.Set("Accept", "text/event-stream")
		}
	case "gemini":
		// Gemini uses API key in URL query parameter
		q := req.URL.Query()
		q.Add("key", apiKey)
		req.URL.RawQuery = q.Encode()
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}
	if err := proxy.ValidateSemanticResponseHasOutput(body, resp.Header.Get("Content-Type")); err != nil {
		return "", fmt.Errorf("API returned no usable output: %v", err)
	}

	// Parse response to extract the actual message
	var result map[string]interface{}
	if transformer == "openai2" && isEventStreamResponse(resp.Header.Get("Content-Type"), body) {
		if text := extractResponsesSSEText(body); text != "" {
			return text, nil
		}
		return string(body), nil
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return string(body), nil
	}

	// Extract message based on transformer
	switch transformer {
	case "claude":
		if content, ok := result["content"].([]interface{}); ok && len(content) > 0 {
			if block, ok := content[0].(map[string]interface{}); ok {
				if text, ok := block["text"].(string); ok {
					return text, nil
				}
			}
		}
	case "openai2":
		if text := extractResponsesJSONText(result); text != "" {
			return text, nil
		}
	case "openai", "deepseek", "kimi":
		if choices, ok := result["choices"].([]interface{}); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]interface{}); ok {
				if message, ok := choice["message"].(map[string]interface{}); ok {
					if content, ok := message["content"].(string); ok {
						return content, nil
					}
				}
			}
		}
	case "gemini":
		if candidates, ok := result["candidates"].([]interface{}); ok && len(candidates) > 0 {
			if candidate, ok := candidates[0].(map[string]interface{}); ok {
				if content, ok := candidate["content"].(map[string]interface{}); ok {
					if parts, ok := content["parts"].([]interface{}); ok && len(parts) > 0 {
						if part, ok := parts[0].(map[string]interface{}); ok {
							if text, ok := part["text"].(string); ok {
								return text, nil
							}
						}
					}
				}
			}
		}
	}

	return string(body), nil
}

func isEventStreamResponse(contentType string, body []byte) bool {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return true
	}
	return strings.Contains(string(body), "data:")
}

func extractResponsesSSEText(body []byte) string {
	var builder strings.Builder
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		if delta, ok := event["delta"].(string); ok {
			builder.WriteString(delta)
		}
		if text, ok := event["text"].(string); ok && builder.Len() == 0 {
			builder.WriteString(text)
		}
		if response, ok := event["response"].(map[string]interface{}); ok {
			if text := extractResponsesJSONText(response); text != "" && builder.Len() == 0 {
				builder.WriteString(text)
			}
		}
	}
	return strings.TrimSpace(builder.String())
}

func extractResponsesJSONText(result map[string]interface{}) string {
	if text, ok := result["output_text"].(string); ok && strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text)
	}
	output, ok := result["output"].([]interface{})
	if !ok {
		return ""
	}
	var builder strings.Builder
	for _, rawItem := range output {
		item, ok := rawItem.(map[string]interface{})
		if !ok {
			continue
		}
		content, ok := item["content"].([]interface{})
		if !ok {
			continue
		}
		for _, rawPart := range content {
			part, ok := rawPart.(map[string]interface{})
			if !ok {
				continue
			}
			if text, ok := part["text"].(string); ok {
				builder.WriteString(text)
			}
		}
	}
	return strings.TrimSpace(builder.String())
}

func (h *Handler) resolveEndpointAPIKey(endpoint *storage.Endpoint) (string, error) {
	authMode := config.NormalizeAuthMode(endpoint.AuthMode)
	if config.IsTokenPoolAuthMode(authMode) {
		cred, err := h.storage.GetUsableEndpointCredential(endpoint.Name, time.Now().UTC())
		if err != nil {
			return "", fmt.Errorf("failed to get token from pool: %w", err)
		}
		if cred == nil || strings.TrimSpace(cred.AccessToken) == "" {
			return "", fmt.Errorf("no usable token in token pool")
		}
		return strings.TrimSpace(cred.AccessToken), nil
	}

	apiKey := strings.TrimSpace(endpoint.APIKey)
	if apiKey == "" {
		return "", fmt.Errorf("apiKey is empty")
	}
	return apiKey, nil
}

// handleFetchModels fetches available models from a provider
func (h *Handler) handleFetchModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req struct {
		APIUrl      string `json:"apiUrl"`
		APIKey      string `json:"apiKey"`
		Transformer string `json:"transformer"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	models, err := h.fetchModelsFromProvider(req.APIUrl, req.APIKey, req.Transformer)
	if err != nil {
		logger.Error("Failed to fetch models: %v", err)
		WriteError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to fetch models: %v", err))
		return
	}

	WriteSuccess(w, map[string]interface{}{
		"models": models,
	})
}

// fetchModelsFromProvider fetches available models from a provider
func (h *Handler) fetchModelsFromProvider(apiUrl, apiKey, transformer string) ([]string, error) {
	apiUrl = providercompat.NormalizeBaseURL(apiUrl)
	if transformer == "" {
		transformer = "auto"
	}
	transformer = providercompat.InferEndpointTransformer(apiUrl, "", transformer)
	var urls []string
	var authHeader string

	switch transformer {
	case "openai", "openai2", "deepseek", "kimi":
		candidates, err := providercompat.BuildOpenAIModelURLCandidates(apiUrl, transformer)
		if err != nil {
			return nil, err
		}
		urls = candidates
		authHeader = "Bearer " + apiKey
	case "claude":
		// Claude doesn't have a models endpoint, return known models
		return []string{
			"claude-3-5-sonnet-20241022",
			"claude-3-5-haiku-20241022",
			"claude-3-opus-20240229",
			"claude-3-sonnet-20240229",
			"claude-3-haiku-20240307",
		}, nil
	case "gemini":
		// Gemini models are typically known
		return []string{
			"gemini-pro",
			"gemini-pro-vision",
			"gemini-ultra",
		}, nil
	default:
		return nil, fmt.Errorf("unsupported transformer: %s", transformer)
	}

	var lastErr error
	for _, url := range urls {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, err
		}

		req.Header.Set("Authorization", authHeader)

		client := &http.Client{
			Timeout: 10 * time.Second,
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("API returned status %d: %s", resp.StatusCode, providercompat.TruncateErrorBody(string(body)))
			if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
				continue
			}
			return nil, lastErr
		}

		var result struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			lastErr = fmt.Errorf("failed to parse response from %s: %w", url, err)
			continue
		}
		resp.Body.Close()

		models := make([]string, 0, len(result.Data))
		for _, model := range result.Data {
			models = append(models, model.ID)
		}

		return models, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no models URL candidates")
}
