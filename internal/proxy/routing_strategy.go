package proxy

import (
	"strings"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/providercompat"
)

const (
	routingPreferenceNone   = ""
	routingPreferenceClaude = "claude"
	routingPreferenceCodex  = "codex"
)

func (p *Proxy) routingPreferenceForRequest(clientFormat ClientFormat, model string) string {
	if p == nil || p.config == nil {
		return routingPreferenceNone
	}

	switch p.config.GetRoutingStrategy() {
	case config.RoutingStrategyClaude:
		return routingPreferenceClaude
	case config.RoutingStrategyCodex:
		return routingPreferenceCodex
	}

	if providercompat.IsOpenAIResponsesModel(model) {
		return routingPreferenceCodex
	}
	if clientFormat == ClientFormatClaude && config.IsClaudeModelName(model) {
		return routingPreferenceClaude
	}
	return routingPreferenceNone
}

func (p *Proxy) applyRoutingStrategyToRequestPlan(endpoints []config.Endpoint, preference string) []config.Endpoint {
	if len(endpoints) <= 1 || strings.TrimSpace(preference) == routingPreferenceNone {
		return endpoints
	}

	primary := make([]config.Endpoint, 0, len(endpoints))
	fallback := make([]config.Endpoint, 0, len(endpoints))
	other := make([]config.Endpoint, 0, len(endpoints))
	deprioritizedPrimary := make([]config.Endpoint, 0)
	deprioritizedFallback := make([]config.Endpoint, 0)
	deprioritizedOther := make([]config.Endpoint, 0)

	for _, endpoint := range endpoints {
		class := classifyEndpointForRoutingPreference(endpoint, preference)
		deprioritized := p != nil && p.isEndpointDeprioritized(endpoint.Name)
		switch {
		case class == routingPreferencePrimary && deprioritized:
			deprioritizedPrimary = append(deprioritizedPrimary, endpoint)
		case class == routingPreferencePrimary:
			primary = append(primary, endpoint)
		case class == routingPreferenceFallback && deprioritized:
			deprioritizedFallback = append(deprioritizedFallback, endpoint)
		case class == routingPreferenceFallback:
			fallback = append(fallback, endpoint)
		case deprioritized:
			deprioritizedOther = append(deprioritizedOther, endpoint)
		default:
			other = append(other, endpoint)
		}
	}

	routed := make([]config.Endpoint, 0, len(endpoints))
	routed = append(routed, primary...)
	routed = append(routed, fallback...)
	routed = append(routed, other...)
	routed = append(routed, deprioritizedPrimary...)
	routed = append(routed, deprioritizedFallback...)
	routed = append(routed, deprioritizedOther...)
	return routed
}

const (
	routingPreferenceOther = iota
	routingPreferenceFallback
	routingPreferencePrimary
)

func classifyEndpointForRoutingPreference(endpoint config.Endpoint, preference string) int {
	switch strings.TrimSpace(preference) {
	case routingPreferenceClaude:
		switch {
		case isClaudeCapableEndpoint(endpoint):
			return routingPreferencePrimary
		case isCodexCapableEndpoint(endpoint):
			return routingPreferenceFallback
		default:
			return routingPreferenceOther
		}
	case routingPreferenceCodex:
		switch {
		case isCodexCapableEndpoint(endpoint):
			return routingPreferencePrimary
		case isClaudeCapableEndpoint(endpoint):
			return routingPreferenceFallback
		default:
			return routingPreferenceOther
		}
	default:
		return routingPreferenceOther
	}
}

func endpointMatchesRoutingPreference(endpoint config.Endpoint, preference string) bool {
	switch strings.TrimSpace(preference) {
	case routingPreferenceClaude:
		return isClaudeCapableEndpoint(endpoint)
	case routingPreferenceCodex:
		return isCodexCapableEndpoint(endpoint)
	default:
		return false
	}
}

func isClaudeCapableEndpoint(endpoint config.Endpoint) bool {
	native := providercompat.NormalizeTransformer(endpoint.Transformer)
	return native == providercompat.TransformerClaude ||
		endpoint.SupportsClaudeMessages ||
		config.IsClaudeModelName(endpoint.Model)
}

func isCodexCapableEndpoint(endpoint config.Endpoint) bool {
	native := providercompat.NormalizeTransformer(endpoint.Transformer)
	return native == providercompat.TransformerOpenAI2 ||
		endpoint.SupportsOpenAIResponses
}

func findEndpointByName(endpoints []config.Endpoint, name string) (config.Endpoint, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return config.Endpoint{}, false
	}
	for _, endpoint := range endpoints {
		if endpoint.Name == name {
			return endpoint, true
		}
	}
	return config.Endpoint{}, false
}
