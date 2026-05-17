package proxy

import (
	"strings"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/logger"
	"github.com/lich0821/ccNexus/internal/providercompat"
	"github.com/lich0821/ccNexus/internal/storage"
)

func protocolFallbackKey(endpointName string, clientFormat ClientFormat) string {
	return strings.TrimSpace(endpointName) + "|" + string(clientFormat)
}

func protocolFallbackTransformerForHTTPFailure(clientFormat ClientFormat, endpoint config.Endpoint, upstreamEndpoint config.Endpoint, transformerName string, statusCode int, body string) string {
	if statusCode < 400 {
		return ""
	}
	if !endpointAllowsProtocolFallback(endpoint) {
		return ""
	}
	current := providercompat.NormalizeTransformer(upstreamEndpoint.Transformer)
	lower := strings.ToLower(strings.TrimSpace(body))

	if current == providercompat.TransformerOpenAI2 || strings.Contains(strings.ToLower(transformerName), "openai2") {
		if shouldFallbackResponsesToChat(statusCode, lower) {
			return openAIChatFallbackTransformer(endpoint)
		}
	}

	if providercompat.IsOpenAIChatTransformer(current) || isOpenAIChatTransformerName(transformerName) {
		if shouldFallbackChatToResponses(clientFormat, statusCode, lower) {
			return providercompat.TransformerOpenAI2
		}
	}

	return ""
}

func endpointAllowsProtocolFallback(endpoint config.Endpoint) bool {
	if endpoint.AutoSelect {
		return true
	}
	inferred := providercompat.InferProviderTransformer(endpoint.APIUrl, endpoint.Model)
	switch providercompat.NormalizeTransformer(inferred) {
	case providercompat.TransformerKimi, providercompat.TransformerDeepSeek:
		return true
	default:
		return false
	}
}

func shouldFallbackResponsesToChat(statusCode int, lowerBody string) bool {
	if statusCode == 404 || statusCode == 405 {
		return true
	}
	if strings.Contains(lowerBody, "unsupported parameter") &&
		(strings.Contains(lowerBody, "max_output_tokens") ||
			strings.Contains(lowerBody, "input") ||
			strings.Contains(lowerBody, "instructions") ||
			strings.Contains(lowerBody, "store") ||
			strings.Contains(lowerBody, "reasoning")) {
		return true
	}
	if strings.Contains(lowerBody, "bad_response_body") ||
		strings.Contains(lowerBody, "invalid character") ||
		strings.Contains(lowerBody, "looking for beginning of value") {
		return true
	}
	if strings.Contains(lowerBody, "responses") &&
		(strings.Contains(lowerBody, "not supported") || strings.Contains(lowerBody, "unsupported")) {
		return true
	}
	if !strings.Contains(lowerBody, "messages") &&
		!strings.Contains(lowerBody, "api.responses.write") &&
		!strings.Contains(lowerBody, "chat/completions") {
		return false
	}
	return strings.Contains(lowerBody, "missing") ||
		strings.Contains(lowerBody, "required") ||
		strings.Contains(lowerBody, "field required") ||
		strings.Contains(lowerBody, "api.responses.write") ||
		strings.Contains(lowerBody, "chat/completions")
}

// ShouldFallbackResponsesToChat reports whether an OpenAI Responses HTTP
// failure is specific enough to retry the same endpoint through Chat Completions.
func ShouldFallbackResponsesToChat(statusCode int, body string) bool {
	return shouldFallbackResponsesToChat(statusCode, strings.ToLower(strings.TrimSpace(body)))
}

func shouldFallbackChatToResponses(clientFormat ClientFormat, statusCode int, lowerBody string) bool {
	if clientFormat != ClientFormatOpenAIResponses {
		return false
	}
	if statusCode == 404 || statusCode == 405 {
		return true
	}
	if !strings.Contains(lowerBody, "input") && !strings.Contains(lowerBody, "responses") {
		return false
	}
	return strings.Contains(lowerBody, "missing") ||
		strings.Contains(lowerBody, "required") ||
		strings.Contains(lowerBody, "field required") ||
		strings.Contains(lowerBody, "responses")
}

func openAIChatFallbackTransformer(endpoint config.Endpoint) string {
	if inferred := providercompat.InferProviderTransformer(endpoint.APIUrl, endpoint.Model); providercompat.IsOpenAIChatTransformer(inferred) {
		return inferred
	}
	if selected := openAIChatTransformerForEndpoint(endpoint); providercompat.IsOpenAIChatTransformer(selected) {
		return selected
	}
	return providercompat.TransformerOpenAI
}

func (p *Proxy) rememberProtocolFallbackSuccess(endpoint config.Endpoint, upstreamTransformer string) {
	if p == nil || p.config == nil || strings.TrimSpace(endpoint.Name) == "" {
		return
	}

	normalized := providercompat.NormalizeTransformer(upstreamTransformer)
	if normalized == "" || normalized == "auto" {
		return
	}

	endpoints := p.config.GetEndpoints()
	changed := false
	for i := range endpoints {
		if endpoints[i].Name != endpoint.Name {
			continue
		}
		changed = applyProtocolSuccessToEndpoint(&endpoints[i], normalized)
		break
	}
	if !changed {
		return
	}

	p.config.UpdateEndpoints(endpoints)
	if err := p.UpdateConfigPreservingCurrentName(p.config, p.GetCurrentEndpointName()); err != nil {
		logger.Warn("[%s] Failed to refresh proxy config after protocol fallback: %v", endpoint.Name, err)
	}

	if p.storage != nil {
		p.persistProtocolSuccess(endpoint.Name, normalized)
	}
}

func applyProtocolSuccessToEndpoint(endpoint *config.Endpoint, upstreamTransformer string) bool {
	if endpoint == nil {
		return false
	}
	before := *endpoint
	endpoint.AutoSelect = true
	switch providercompat.NormalizeTransformer(upstreamTransformer) {
	case providercompat.TransformerOpenAI, providercompat.TransformerDeepSeek, providercompat.TransformerKimi:
		endpoint.Transformer = providercompat.NormalizeTransformer(upstreamTransformer)
		endpoint.SupportsOpenAIChat = true
		endpoint.SupportsOpenAIResponses = false
		endpoint.PreferredOpenAIUpstream = providercompat.TransformerOpenAI
	case providercompat.TransformerOpenAI2:
		endpoint.SupportsOpenAIResponses = true
		endpoint.PreferredOpenAIUpstream = providercompat.TransformerOpenAI2
	case providercompat.TransformerClaude:
		endpoint.SupportsClaudeMessages = true
		endpoint.PreferredClaudeUpstream = providercompat.TransformerClaude
	default:
		return false
	}
	config.ApplyEndpointAuthModeRules(endpoint)
	return endpoint.AutoSelect != before.AutoSelect ||
		endpoint.Transformer != before.Transformer ||
		endpoint.SupportsOpenAIChat != before.SupportsOpenAIChat ||
		endpoint.SupportsOpenAIResponses != before.SupportsOpenAIResponses ||
		endpoint.SupportsClaudeMessages != before.SupportsClaudeMessages ||
		endpoint.PreferredOpenAIUpstream != before.PreferredOpenAIUpstream ||
		endpoint.PreferredClaudeUpstream != before.PreferredClaudeUpstream
}

func (p *Proxy) persistProtocolSuccess(endpointName string, upstreamTransformer string) {
	endpoints, err := p.storage.GetEndpoints()
	if err != nil {
		logger.Warn("[%s] Failed to load endpoints for protocol fallback persistence: %v", endpointName, err)
		return
	}
	for i := range endpoints {
		if endpoints[i].Name != endpointName {
			continue
		}
		updated := endpoints[i]
		if !applyProtocolSuccessToStorageEndpoint(&updated, upstreamTransformer) {
			return
		}
		if err := p.storage.UpdateEndpoint(&updated); err != nil {
			logger.Warn("[%s] Failed to persist protocol fallback capabilities: %v", endpointName, err)
		}
		return
	}
}

func applyProtocolSuccessToStorageEndpoint(endpoint *storage.Endpoint, upstreamTransformer string) bool {
	if endpoint == nil {
		return false
	}
	before := *endpoint
	endpoint.AutoSelect = true
	switch providercompat.NormalizeTransformer(upstreamTransformer) {
	case providercompat.TransformerOpenAI, providercompat.TransformerDeepSeek, providercompat.TransformerKimi:
		endpoint.Transformer = providercompat.NormalizeTransformer(upstreamTransformer)
		endpoint.SupportsOpenAIChat = true
		endpoint.SupportsOpenAIResponses = false
		endpoint.PreferredOpenAIUpstream = providercompat.TransformerOpenAI
	case providercompat.TransformerOpenAI2:
		endpoint.SupportsOpenAIResponses = true
		endpoint.PreferredOpenAIUpstream = providercompat.TransformerOpenAI2
	case providercompat.TransformerClaude:
		endpoint.SupportsClaudeMessages = true
		endpoint.PreferredClaudeUpstream = providercompat.TransformerClaude
	default:
		return false
	}
	return endpoint.AutoSelect != before.AutoSelect ||
		endpoint.Transformer != before.Transformer ||
		endpoint.SupportsOpenAIChat != before.SupportsOpenAIChat ||
		endpoint.SupportsOpenAIResponses != before.SupportsOpenAIResponses ||
		endpoint.SupportsClaudeMessages != before.SupportsClaudeMessages ||
		endpoint.PreferredOpenAIUpstream != before.PreferredOpenAIUpstream ||
		endpoint.PreferredClaudeUpstream != before.PreferredClaudeUpstream
}
