package proxy

import (
	"strings"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/endpointstate"
	"github.com/lich0821/ccNexus/internal/providercompat"
)

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
		for _, candidate := range claudeClientAutoUpstreamCandidates(endpoint, native) {
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

func claudeClientAutoUpstreamCandidates(endpoint config.Endpoint, native string) []string {
	if shouldPreferResponsesForClaudeClient(endpoint, native) {
		return []string{
			providercompat.TransformerOpenAI2,
			openAIChatTransformerForEndpoint(endpoint),
			providercompat.TransformerClaude,
			native,
		}
	}
	return []string{
		openAIChatTransformerForEndpoint(endpoint),
		providercompat.TransformerOpenAI2,
		providercompat.TransformerClaude,
		native,
	}
}

func shouldPreferResponsesForClaudeClient(endpoint config.Endpoint, native string) bool {
	return native == providercompat.TransformerOpenAI2 || providercompat.IsOpenAIResponsesModel(endpoint.Model)
}

func selectPreferredUpstream(preference string, endpoint config.Endpoint, caps endpointstate.Capabilities) string {
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

func capabilitiesForEndpoint(endpoint config.Endpoint) endpointstate.Capabilities {
	return endpointstate.DeriveCapabilities(
		endpoint.Transformer,
		endpoint.SupportsClaudeMessages,
		endpoint.SupportsOpenAIChat,
		endpoint.SupportsOpenAIResponses,
	)
}

func supportsUpstreamTransformer(transformerName string, caps endpointstate.Capabilities) bool {
	return caps.SupportsTransformer(transformerName)
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
