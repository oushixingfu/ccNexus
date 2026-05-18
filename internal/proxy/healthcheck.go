package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/endpointstate"
	"github.com/lich0821/ccNexus/internal/logger"
	"github.com/lich0821/ccNexus/internal/providercompat"
)

const (
	defaultHealthCheckInterval = 60 * time.Second
	healthCheckReadLimitBytes  = 256 * 1024
	healthCheckPrompt          = "Reply with exactly: pong"
	healthCheckMaxTokens       = 128
)

// healthCheckResult holds the outcome of a single endpoint health probe.
type healthCheckResult struct {
	EndpointName string
	Success      bool
	StatusCode   int
	Headers      http.Header
	Reason       string
	Error        string
}

// registerForHealthCheck adds an endpoint to the periodic health check watch set.
func (p *Proxy) registerForHealthCheck(endpointName string) {
	if strings.TrimSpace(endpointName) == "" {
		return
	}
	p.healthCheckWatchedMu.Lock()
	if p.healthCheckWatched == nil {
		p.healthCheckWatched = make(map[string]struct{})
	}
	p.healthCheckWatched[endpointName] = struct{}{}
	p.healthCheckWatchedMu.Unlock()
	logger.Debug("[HEALTHCHECK] Registered %s for health check polling", endpointName)
	p.kickHealthCheck()
}

// unregisterFromHealthCheck removes an endpoint from the health check watch set.
func (p *Proxy) unregisterFromHealthCheck(endpointName string) {
	if strings.TrimSpace(endpointName) == "" {
		return
	}
	p.healthCheckWatchedMu.Lock()
	delete(p.healthCheckWatched, endpointName)
	p.healthCheckWatchedMu.Unlock()
	logger.Debug("[HEALTHCHECK] Unregistered %s from health check polling", endpointName)
}

// healthCheckInterval returns the configured health check polling interval.
func (p *Proxy) healthCheckInterval() time.Duration {
	if p != nil && p.config != nil {
		failover := p.config.GetFailover()
		if failover != nil && failover.HealthCheckIntervalSec > 0 {
			return time.Duration(failover.HealthCheckIntervalSec) * time.Second
		}
	}
	return defaultHealthCheckInterval
}

// startHealthCheckLoop launches the background health check polling goroutine.
// It is called from StartWithMux and must not be called concurrently.
func (p *Proxy) startHealthCheckLoop() {
	if p.healthCheckCtx != nil {
		return // already running
	}
	if p.healthCheckWake == nil {
		p.healthCheckWake = make(chan struct{}, 1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.healthCheckCtx = ctx
	p.healthCheckCancel = cancel
	p.seedHealthCheckWatchSet()

	go p.runHealthCheckLoop(ctx)
	logger.Info("[HEALTHCHECK] Health check polling started (interval=%s)", p.healthCheckInterval())
}

// stopHealthCheckLoop cancels the background health check goroutine.
func (p *Proxy) stopHealthCheckLoop() {
	if p.healthCheckCancel != nil {
		p.healthCheckCancel()
		p.healthCheckCancel = nil
	}
	p.healthCheckCtx = nil
	logger.Info("[HEALTHCHECK] Health check polling stopped")
}

func (p *Proxy) seedHealthCheckWatchSet() {
	currentName := p.GetCurrentEndpointName()
	p.watchPreferredEndpointsForAutoReturn(currentName)

	if p.storage == nil {
		return
	}
	statuses, err := p.storage.GetEndpointRuntimeStatuses()
	if err != nil {
		logger.Warn("[HEALTHCHECK] Failed to load endpoint runtime statuses: %v", err)
		return
	}
	for name, status := range statuses {
		if status == nil || status.LastFailureAt == nil {
			continue
		}
		if shouldBlockHealthCheckRecoveryReason(status.LastFailureReason) {
			p.setRuntimeBlockedEndpoint(name, status.LastFailureReason)
			p.restoreEndpointCooldownFromRuntimeStatus(name, status.LastFailureReason, *status.LastFailureAt)
			if currentName := p.GetCurrentEndpointName(); currentName == name {
				if endpoint := p.findEnabledEndpoint(name); endpoint != nil {
					p.switchCurrentEndpointAfterFailure(*endpoint, status.LastFailureReason, requestObservability{RequestID: "healthcheck_seed"}, 0)
				}
			}
			p.registerForHealthCheck(name)
			continue
		}
		state := endpointstate.Derive(true, status)
		failureAfterSuccess := !state.Available && state.Availability == endpointstate.Unavailable
		restoredCooldown := false
		if failureAfterSuccess || shouldRestoreDeferredCooldown(status.LastFailureReason, *status.LastFailureAt, p.cooldownDurationForReason(status.LastFailureReason, nil)) {
			restoredCooldown = p.restoreEndpointCooldownFromRuntimeStatus(name, status.LastFailureReason, *status.LastFailureAt)
		}
		if restoredCooldown || failureAfterSuccess {
			p.registerForHealthCheck(name)
		}
	}
}

func (p *Proxy) restoreEndpointCooldownFromRuntimeStatus(endpointName string, reason string, lastFailureAt time.Time) bool {
	duration := p.cooldownDurationForReason(reason, nil)
	if duration <= 0 || lastFailureAt.IsZero() {
		return false
	}

	until := lastFailureAt.Add(duration)
	if !until.After(time.Now()) {
		return false
	}

	p.setEndpointCooldownUntil(endpointName, reason, until)
	if currentName := p.GetCurrentEndpointName(); currentName == endpointName {
		if endpoint := p.findEnabledEndpoint(endpointName); endpoint != nil {
			p.switchCurrentEndpointAfterFailure(*endpoint, reason, requestObservability{RequestID: "healthcheck_seed"}, 0)
		}
	}
	logger.Debug("[HEALTHCHECK] Restored cooldown for %s until=%s cooldown_reason=%s",
		endpointName,
		until.Format(time.RFC3339),
		sanitizeLogField(reason),
	)
	return true
}

func shouldRestoreDeferredCooldown(reason string, lastFailureAt time.Time, duration time.Duration) bool {
	if !shouldDeferHealthCheckForCooldownReason(reason) || duration <= 0 || lastFailureAt.IsZero() {
		return false
	}
	return lastFailureAt.Add(duration).After(time.Now())
}

func (p *Proxy) watchPreferredEndpointsForAutoReturn(currentName string) {
	if strings.TrimSpace(currentName) == "" ||
		p.recoveredEndpointPolicy() != config.RecoveredEndpointPolicyAutoReturn {
		return
	}
	for _, endpoint := range p.getEnabledEndpoints() {
		if endpoint.Name != currentName &&
			p.shouldPreferEndpoint(endpoint.Name, currentName) &&
			!p.hasBlockedHealthCheckRecoveryFailure(endpoint.Name) {
			p.registerForHealthCheck(endpoint.Name)
		}
	}
}

func (p *Proxy) RefreshHealthCheckWatchSet() {
	if p == nil {
		return
	}
	p.seedHealthCheckWatchSet()
	p.kickHealthCheck()
}

func (p *Proxy) kickHealthCheck() {
	if p == nil || p.healthCheckWake == nil {
		return
	}
	select {
	case p.healthCheckWake <- struct{}{}:
	default:
	}
}

// runHealthCheckLoop is the main health check polling loop executed in a goroutine.
func (p *Proxy) runHealthCheckLoop(ctx context.Context) {
	timer := time.NewTimer(p.healthCheckInterval())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.healthCheckWake:
			p.runHealthCheckRound()
			resetHealthCheckTimer(timer, p.healthCheckInterval())
		case <-timer.C:
			p.runHealthCheckRound()
			resetHealthCheckTimer(timer, p.healthCheckInterval())
		}
	}
}

func resetHealthCheckTimer(timer *time.Timer, interval time.Duration) {
	if interval <= 0 {
		interval = defaultHealthCheckInterval
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(interval)
}

// runHealthCheckRound iterates over all watched endpoints and probes each one.
func (p *Proxy) runHealthCheckRound() {
	watched := p.copyWatchedEndpoints()
	if len(watched) == 0 {
		return
	}

	logger.Debug("[HEALTHCHECK] Running health check round for %d endpoint(s)", len(watched))

	for _, name := range watched {
		endpoint := p.findEnabledEndpoint(name)
		if endpoint == nil {
			p.unregisterFromHealthCheck(name)
			continue
		}
		if p.shouldDeferHealthCheckForActiveCooldown(name) {
			continue
		}

		result := p.probeEndpointHealth(endpoint)
		if result.Success {
			if p.shouldKeepRuntimeBlockAfterHealthSuccess(name) {
				logger.Info("[HEALTHCHECK] Endpoint %s probe succeeded but runtime block remains active (reason=%s)", name, sanitizeLogField(p.runtimeBlockedReason(name)))
				continue
			}
			status := p.recordEndpointSuccess(name)
			p.emitEndpointRuntimeEvent(name, "success", status)
			p.clearEndpointCooldown(name)
			p.unregisterFromHealthCheck(name)
			logger.Info("[HEALTHCHECK] Endpoint %s recovered (status=%d), clearing cooldown", name, result.StatusCode)

			// Only auto-return when explicitly configured. The default
			// deprioritize policy keeps the current endpoint stable.
			currentName := p.GetCurrentEndpointName()
			if p.recoveredEndpointPolicy() == config.RecoveredEndpointPolicyAutoReturn &&
				currentName != "" &&
				currentName != name &&
				p.shouldPreferEndpoint(name, currentName) {
				if err := p.SetCurrentEndpoint(name); err != nil {
					logger.Warn("[HEALTHCHECK] Failed to switch back to %s: %v", name, err)
				} else {
					logger.Info("[HEALTHCHECK] Auto-switched back to recovered endpoint %s", name)
				}
			}
		} else if result.Error != "" {
			p.recordHealthCheckFailure(result)
			logger.Warn("[HEALTHCHECK] Endpoint %s still unhealthy (status=%d): %s", name, result.StatusCode, result.Error)
		}
	}
}

func (p *Proxy) shouldKeepRuntimeBlockAfterHealthSuccess(endpointName string) bool {
	if p == nil || strings.TrimSpace(endpointName) == "" {
		return false
	}
	if shouldBlockHealthCheckRecoveryReason(p.runtimeBlockedReason(endpointName)) {
		return true
	}
	if p.storage == nil {
		return false
	}
	statuses, err := p.storage.GetEndpointRuntimeStatuses()
	if err != nil {
		logger.Warn("[HEALTHCHECK] Failed to load runtime status for %s: %v", endpointName, err)
		return false
	}
	status := statuses[endpointName]
	return status != nil && shouldBlockHealthCheckRecoveryReason(status.LastFailureReason)
}

func (p *Proxy) recordHealthCheckFailure(result healthCheckResult) {
	if result.EndpointName == "" || result.Reason == "" {
		return
	}

	reason := result.Reason
	if blockedReason := p.runtimeBlockedReason(result.EndpointName); shouldBlockHealthCheckRecoveryReason(blockedReason) {
		reason = blockedReason
	}

	status := p.recordEndpointFailure(result.EndpointName, reason, result.StatusCode)
	p.emitEndpointRuntimeEvent(result.EndpointName, "failure", status)
	p.markEndpointHealthCheckCooldown(result.EndpointName, reason, result.Headers)
}

func (p *Proxy) hasBlockedHealthCheckRecoveryFailure(endpointName string) bool {
	if p == nil || p.storage == nil || strings.TrimSpace(endpointName) == "" {
		blocked := p.snapshotRuntimeBlockedEndpoints()
		_, ok := blocked[endpointName]
		return ok
	}
	blocked := p.snapshotRuntimeBlockedEndpoints()
	if _, ok := blocked[endpointName]; ok {
		return true
	}
	statuses, err := p.storage.GetEndpointRuntimeStatuses()
	if err != nil {
		logger.Warn("[HEALTHCHECK] Failed to load runtime status for %s: %v", endpointName, err)
		return false
	}
	status := statuses[endpointName]
	if status == nil {
		return false
	}
	if shouldBlockHealthCheckRecoveryReason(status.LastFailureReason) {
		p.setRuntimeBlockedEndpoint(endpointName, status.LastFailureReason)
		return true
	}
	return false
}

func (p *Proxy) shouldDeferHealthCheckForActiveCooldown(endpointName string) bool {
	cooldown, ok := p.endpointCooldown(endpointName)
	if !ok || !cooldown.Until.After(time.Now()) {
		return false
	}
	if !shouldDeferHealthCheckForCooldownReason(cooldown.Reason) {
		return false
	}
	logger.Debug("[HEALTHCHECK] Deferring probe for cooled endpoint %s remaining=%s cooldown_reason=%s",
		endpointName,
		time.Until(cooldown.Until).Round(time.Millisecond),
		sanitizeLogField(cooldown.Reason),
	)
	return true
}

// copyWatchedEndpoints returns a snapshot of the current health check watch set.
func (p *Proxy) copyWatchedEndpoints() []string {
	p.healthCheckWatchedMu.RLock()
	defer p.healthCheckWatchedMu.RUnlock()
	names := make([]string, 0, len(p.healthCheckWatched))
	for name := range p.healthCheckWatched {
		names = append(names, name)
	}
	return names
}

// findEnabledEndpoint returns the enabled endpoint config by name, or nil.
func (p *Proxy) findEnabledEndpoint(name string) *config.Endpoint {
	endpoints := p.getEnabledEndpoints()
	for i := range endpoints {
		if endpoints[i].Name == name {
			return &endpoints[i]
		}
	}
	return nil
}

// shouldPreferEndpoint returns true if 'name' should be preferred over 'currentName'
// (i.e., it appears earlier in the sorted enabled endpoint list).
func (p *Proxy) shouldPreferEndpoint(name, currentName string) bool {
	endpoints := p.getEnabledEndpoints()
	nameIdx := -1
	currentIdx := -1
	for i, ep := range endpoints {
		if ep.Name == name {
			nameIdx = i
		}
		if ep.Name == currentName {
			currentIdx = i
		}
	}
	return nameIdx >= 0 && currentIdx >= 0 && nameIdx < currentIdx
}

// probeEndpointHealth sends a minimal request to an endpoint and returns the result.
func (p *Proxy) probeEndpointHealth(endpoint *config.Endpoint) healthCheckResult {
	result := healthCheckResult{EndpointName: endpoint.Name}

	apiKey, err := p.resolveEndpointAPIKeyForHealthCheck(endpoint)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	reqBody, targetURL, err := p.buildHealthCheckRequest(endpoint)
	if err != nil {
		result.Error = fmt.Sprintf("failed to build health check request: %v", err)
		return result
	}

	req, err := http.NewRequest("POST", targetURL, bytes.NewReader(reqBody))
	if err != nil {
		result.Error = fmt.Sprintf("failed to create health check request: %v", err)
		return result
	}

	req.Header.Set("Content-Type", "application/json")
	p.setHealthCheckAuth(req, endpoint, apiKey)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := sendRequest(ctx, req, p.httpClient, p.config)
	if err != nil {
		result.Error = fmt.Sprintf("health check request failed: %v", err)
		return result
	}
	defer resp.Body.Close()
	result.Headers = resp.Header.Clone()

	expectEventStream := p.healthCheckExpectsEventStream(endpoint, resp.Header.Get("Content-Type"))
	body, readErr := readHealthCheckResponseBody(resp.Body, expectEventStream)
	result.StatusCode = resp.StatusCode
	if readErr != nil {
		result.Error = fmt.Sprintf("failed to read health check response: %v; body: %s", readErr, providercompat.TruncateErrorBody(string(body)))
		return result
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := validateHealthCheckSemanticResponse(body, resp.Header.Get("Content-Type"), expectEventStream); err != nil {
			result.Error = fmt.Sprintf("semantic validation failed: %v; body: %s", err, providercompat.TruncateErrorBody(string(body)))
			return result
		}
		result.Success = true
	} else {
		result.Reason = retryReasonForHTTPStatus(resp.StatusCode, string(body))
		result.Error = fmt.Sprintf("status %d: %s", resp.StatusCode, providercompat.TruncateErrorBody(string(body)))
	}
	return result
}

// buildHealthCheckRequest constructs a minimal health check request body and URL
// appropriate for the endpoint's transformer type.
func (p *Proxy) buildHealthCheckRequest(endpoint *config.Endpoint) ([]byte, string, error) {
	normalizedURL := providercompat.NormalizeBaseURL(endpoint.APIUrl)
	transformer := providercompat.NormalizeTransformer(endpoint.Transformer)
	model := endpoint.Model
	if model == "" {
		model = providercompat.DefaultModel(transformer)
	}

	var reqBody []byte
	var urlPath string
	var err error

	switch transformer {
	case "claude":
		urlPath = "/v1/messages"
		body := map[string]interface{}{
			"model":      model,
			"max_tokens": healthCheckMaxTokens,
			"messages": []map[string]interface{}{
				{"role": "user", "content": healthCheckPrompt},
			},
		}
		if endpoint.ForceStream {
			body["stream"] = true
		}
		reqBody, err = json.Marshal(body)
	case "openai", "deepseek", "kimi":
		urlPath = providercompat.OpenAIChatTargetPath(transformer, normalizedURL)
		body := map[string]interface{}{
			"model":      model,
			"max_tokens": healthCheckMaxTokens,
			"messages": []map[string]interface{}{
				{"role": "user", "content": healthCheckPrompt},
			},
		}
		if effort := normalizeEndpointThinking(endpoint.Thinking); effort != "" {
			body["reasoning_effort"] = effort
		}
		if endpoint.ForceStream {
			body["stream"] = true
			body["stream_options"] = map[string]interface{}{"include_usage": true}
		}
		reqBody, err = json.Marshal(body)
	case "openai2":
		urlPath = "/v1/responses"
		body := map[string]interface{}{
			"model":  model,
			"stream": true,
			"input": []map[string]interface{}{
				{
					"type": "message",
					"role": "user",
					"content": []map[string]interface{}{
						{"type": "input_text", "text": healthCheckPrompt},
					},
				},
			},
		}
		if effort := normalizeEndpointThinking(endpoint.Thinking); effort != "" {
			field, level := config.OpenAI2ThinkingField(normalizedURL, model, effort)
			if level != "" {
				if field == "effortLevel" {
					body["effortLevel"] = level
				} else {
					body["reasoning"] = map[string]interface{}{"effort": level}
				}
			}
		}
		reqBody, err = json.Marshal(body)
	case "gemini":
		urlPath = fmt.Sprintf("/v1beta/models/%s:generateContent", model)
		reqBody, err = json.Marshal(map[string]interface{}{
			"contents": []map[string]interface{}{
				{
					"parts": []map[string]interface{}{
						{"text": healthCheckPrompt},
					},
				},
			},
		})
	default:
		return nil, "", fmt.Errorf("unsupported transformer for health check: %s", endpoint.Transformer)
	}

	if err != nil {
		return nil, "", fmt.Errorf("failed to marshal health check request: %w", err)
	}

	targetURL := providercompat.JoinBaseURLAndPath(normalizedURL, urlPath)
	return reqBody, targetURL, nil
}

// setHealthCheckAuth adds authentication headers to the health check request.
func (p *Proxy) setHealthCheckAuth(req *http.Request, endpoint *config.Endpoint, apiKey string) {
	transformer := providercompat.NormalizeTransformer(endpoint.Transformer)
	switch transformer {
	case "claude":
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	case "openai", "openai2", "deepseek", "kimi":
		req.Header.Set("Authorization", "Bearer "+apiKey)
	case "gemini":
		q := req.URL.Query()
		q.Set("key", apiKey)
		req.URL.RawQuery = q.Encode()
	}
}

// resolveEndpointAPIKeyForHealthCheck resolves the API key for health check probing.
func (p *Proxy) resolveEndpointAPIKeyForHealthCheck(endpoint *config.Endpoint) (string, error) {
	authMode := config.NormalizeAuthMode(endpoint.AuthMode)
	if config.IsTokenPoolAuthMode(authMode) && p.storage != nil {
		cred, err := p.storage.GetUsableEndpointCredential(endpoint.Name, time.Now().UTC())
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

func (p *Proxy) healthCheckExpectsEventStream(endpoint *config.Endpoint, contentType string) bool {
	if strings.Contains(strings.ToLower(strings.TrimSpace(contentType)), "text/event-stream") {
		return true
	}
	if endpoint == nil {
		return false
	}

	transformer := providercompat.NormalizeTransformer(endpoint.Transformer)
	if transformer == "openai2" {
		return true
	}
	if !endpoint.ForceStream {
		return false
	}
	switch transformer {
	case "claude", "openai", "deepseek", "kimi":
		return true
	default:
		return false
	}
}

func readHealthCheckResponseBody(body io.Reader, expectEventStream bool) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	if expectEventStream {
		return readHealthCheckEventStreamBody(body, healthCheckReadLimitBytes)
	}
	return readLimitedHealthCheckBody(body, healthCheckReadLimitBytes)
}

func readLimitedHealthCheckBody(body io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, nil
	}
	limited := &io.LimitedReader{R: body, N: limit + 1}
	data, err := io.ReadAll(limited)
	if int64(len(data)) > limit {
		data = data[:limit]
	}
	return data, err
}

func readHealthCheckEventStreamBody(body io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, nil
	}

	reader := bufio.NewReader(body)
	var buffer bytes.Buffer
	for int64(buffer.Len()) < limit {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			remaining := int(limit) - buffer.Len()
			if len(line) > remaining {
				line = line[:remaining]
			}
			_, _ = buffer.Write(line)
			if inspectSemanticStreamEvent(buffer.Bytes()).Completed {
				return buffer.Bytes(), nil
			}
		}
		if err != nil {
			if err == io.EOF {
				return buffer.Bytes(), nil
			}
			return buffer.Bytes(), err
		}
	}
	return buffer.Bytes(), nil
}

func validateHealthCheckSemanticResponse(body []byte, contentType string, expectEventStream bool) error {
	if expectEventStream || looksLikeSemanticEventStream(contentType, body) {
		inspection := inspectSemanticStreamEvent(body)
		if !inspection.HasOutput {
			emptyKind := inspection.EmptyKind
			if emptyKind == "" {
				emptyKind = emptyKindResponsesEmpty
			}
			return newSemanticEmptyResponseError(emptyKind, 0, 0)
		}
		if !inspection.Completed {
			return fmt.Errorf("semantic_stream_incomplete")
		}
		return nil
	}
	return ValidateSemanticResponseHasOutput(body, contentType)
}

// maybeSwitchCurrentEndpointAfterCooldown checks if the cooled endpoint is the
// current global endpoint and, if so, switches to the next available endpoint.
func (p *Proxy) maybeSwitchCurrentEndpointAfterCooldown(cooled config.Endpoint, reason string, obs requestObservability, attemptNumber int) {
	if strings.TrimSpace(cooled.Name) == "" {
		return
	}

	currentName := p.GetCurrentEndpointName()
	if currentName != cooled.Name {
		return
	}

	// Avoid switching if there are no other endpoints.
	enabled := p.getEnabledEndpoints()
	if len(enabled) <= 1 {
		return
	}

	p.switchCurrentEndpointAfterFailure(cooled, reason, obs, attemptNumber)
}
