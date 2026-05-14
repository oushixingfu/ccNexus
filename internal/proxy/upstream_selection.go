package proxy

import (
	"strings"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/providercompat"
)

type endpointUpstreamCapabilities struct {
	claudeResponses bool
	openAIChat      bool
	openAIResponses bool
}

func endpointForClientFormat(clientFormat ClientFormat, endpoint config.Endpoint) config.Endpoint {
	if !endpoint.AutoSelect {
		return endpoint
	}

	selected := selectEndpointUpstreamTransformer(clientFormat, endpoint)
	if selected == "" {
		return endpoint
	}

	effective := endpoint
	effective.Transformer = selected
	return effective
}

// EffectiveUpstreamTransformerForClientFormat returns the upstream protocol that
// ccNexus will use for a given client format without mutating the endpoint.
func EffectiveUpstreamTransformerForClientFormat(clientFormat ClientFormat, endpoint config.Endpoint) string {
	if !endpoint.AutoSelect {
		return providercompat.NormalizeTransformer(endpoint.Transformer)
	}
	return selectEndpointUpstreamTransformer(clientFormat, endpoint)
}

func selectEndpointUpstreamTransformer(clientFormat ClientFormat, endpoint config.Endpoint) string {
	caps := capabilitiesForEndpoint(endpoint)
	native := providercompat.NormalizeTransformer(endpoint.Transformer)
	if native == "" {
		native = providercompat.TransformerClaude
	}

	switch clientFormat {
	case ClientFormatClaude:
		if selected := selectPreferredUpstream(endpoint.PreferredClaudeUpstream, endpoint, caps); selected != "" {
			return selected
		}
		if native == providercompat.TransformerClaude && supportsUpstreamTransformer(providercompat.TransformerClaude, caps) {
			return providercompat.TransformerClaude
		}
		for _, candidate := range []string{
			openAIChatTransformerForEndpoint(endpoint),
			providercompat.TransformerOpenAI2,
			providercompat.TransformerClaude,
			native,
		} {
			if supportsUpstreamTransformer(candidate, caps) {
				return candidate
			}
		}
	case ClientFormatOpenAIResponses:
		if selected := selectPreferredUpstream(endpoint.PreferredOpenAIUpstream, endpoint, caps); selected != "" {
			return selected
		}
		for _, candidate := range []string{
			providercompat.TransformerOpenAI2,
			openAIChatTransformerForEndpoint(endpoint),
			providercompat.TransformerClaude,
		} {
			if supportsUpstreamTransformer(candidate, caps) {
				return candidate
			}
		}
	case ClientFormatOpenAIChat:
		if selected := selectPreferredUpstream(endpoint.PreferredOpenAIUpstream, endpoint, caps); selected != "" {
			return selected
		}
		for _, candidate := range []string{
			openAIChatTransformerForEndpoint(endpoint),
			providercompat.TransformerOpenAI2,
			providercompat.TransformerClaude,
		} {
			if supportsUpstreamTransformer(candidate, caps) {
				return candidate
			}
		}
	}

	return native
}

func selectPreferredUpstream(preference string, endpoint config.Endpoint, caps endpointUpstreamCapabilities) string {
	preferred := config.NormalizeEndpointUpstreamPreference(preference)
	if preferred == "" {
		return ""
	}
	if preferred == providercompat.TransformerOpenAI {
		preferred = openAIChatTransformerForEndpoint(endpoint)
	}
	if supportsUpstreamTransformer(preferred, caps) {
		return preferred
	}
	return ""
}

func capabilitiesForEndpoint(endpoint config.Endpoint) endpointUpstreamCapabilities {
	native := providercompat.NormalizeTransformer(endpoint.Transformer)
	return endpointUpstreamCapabilities{
		claudeResponses: endpoint.SupportsClaudeMessages || native == providercompat.TransformerClaude,
		openAIChat:      endpoint.SupportsOpenAIChat || providercompat.IsOpenAIChatTransformer(native),
		openAIResponses: endpoint.SupportsOpenAIResponses || native == providercompat.TransformerOpenAI2,
	}
}

func supportsUpstreamTransformer(transformerName string, caps endpointUpstreamCapabilities) bool {
	switch providercompat.NormalizeTransformer(transformerName) {
	case providercompat.TransformerClaude:
		return caps.claudeResponses
	case providercompat.TransformerOpenAI2:
		return caps.openAIResponses
	case providercompat.TransformerOpenAI, providercompat.TransformerDeepSeek, providercompat.TransformerKimi:
		return caps.openAIChat
	default:
		return false
	}
}

func openAIChatTransformerForEndpoint(endpoint config.Endpoint) string {
	switch providercompat.NormalizeTransformer(endpoint.Transformer) {
	case providercompat.TransformerDeepSeek:
		return providercompat.TransformerDeepSeek
	case providercompat.TransformerKimi:
		return providercompat.TransformerKimi
	default:
		return providercompat.TransformerOpenAI
	}
}

func isResponsesCompactPath(rawPath string) bool {
	trimmed := strings.TrimRight(strings.TrimSpace(rawPath), "/")
	return strings.HasSuffix(trimmed, "/responses/compact")
}
