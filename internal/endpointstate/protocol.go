package endpointstate

import (
	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/providercompat"
	"github.com/lich0821/ccNexus/internal/storage"
)

type Capabilities struct {
	ClaudeMessages  bool
	OpenAIChat      bool
	OpenAIResponses bool
}

type ProtocolSuccessOptions struct {
	EnableAutoSelect  bool
	RequireAutoSelect bool
}

type protocolState struct {
	autoSelect              bool
	transformer             string
	apiURL                  string
	model                   string
	supportsOpenAIResponses bool
	supportsOpenAIChat      bool
	supportsClaudeMessages  bool
	preferredClaudeUpstream string
	preferredOpenAIUpstream string
}

func DeriveCapabilities(transformer string, supportsClaudeMessages bool, supportsOpenAIChat bool, supportsOpenAIResponses bool) Capabilities {
	native := providercompat.NormalizeTransformer(transformer)
	return Capabilities{
		ClaudeMessages:  supportsClaudeMessages || native == providercompat.TransformerClaude,
		OpenAIChat:      supportsOpenAIChat || providercompat.IsOpenAIChatTransformer(native),
		OpenAIResponses: supportsOpenAIResponses || native == providercompat.TransformerOpenAI2,
	}
}

func (c Capabilities) SupportsTransformer(transformerName string) bool {
	switch providercompat.NormalizeTransformer(transformerName) {
	case providercompat.TransformerClaude:
		return c.ClaudeMessages
	case providercompat.TransformerOpenAI2:
		return c.OpenAIResponses
	case providercompat.TransformerOpenAI, providercompat.TransformerDeepSeek, providercompat.TransformerKimi:
		return c.OpenAIChat
	default:
		return false
	}
}

func ApplyProtocolSuccessToConfigEndpoint(endpoint *config.Endpoint, upstreamTransformer string, options ProtocolSuccessOptions) bool {
	if endpoint == nil {
		return false
	}

	before := protocolStateFromConfigEndpoint(*endpoint)
	after, ok := applyProtocolSuccess(before, upstreamTransformer, options)
	if !ok {
		return false
	}

	applyProtocolStateToConfigEndpoint(endpoint, after)
	config.ApplyEndpointAuthModeRules(endpoint)
	return protocolStateChanged(before, protocolStateFromConfigEndpoint(*endpoint))
}

func ApplyProtocolSuccessToStorageEndpoint(endpoint *storage.Endpoint, upstreamTransformer string, options ProtocolSuccessOptions) bool {
	if endpoint == nil {
		return false
	}

	before := protocolStateFromStorageEndpoint(*endpoint)
	after, ok := applyProtocolSuccess(before, upstreamTransformer, options)
	if !ok {
		return false
	}

	applyProtocolStateToStorageEndpoint(endpoint, after)
	return protocolStateChanged(before, protocolStateFromStorageEndpoint(*endpoint))
}

func applyProtocolSuccess(state protocolState, upstreamTransformer string, options ProtocolSuccessOptions) (protocolState, bool) {
	if options.RequireAutoSelect && !state.autoSelect {
		return state, false
	}

	normalized := providercompat.NormalizeTransformer(upstreamTransformer)
	switch normalized {
	case providercompat.TransformerOpenAI, providercompat.TransformerDeepSeek, providercompat.TransformerKimi:
		if options.EnableAutoSelect {
			state.autoSelect = true
		}
		state.supportsOpenAIChat = true
		if shouldPreserveResponsesAfterChatSuccess(state) {
			state.supportsOpenAIResponses = true
		} else {
			state.transformer = normalized
			state.supportsOpenAIResponses = false
			state.preferredOpenAIUpstream = providercompat.TransformerOpenAI
		}
	case providercompat.TransformerOpenAI2:
		if options.EnableAutoSelect {
			state.autoSelect = true
		}
		state.supportsOpenAIResponses = true
		state.preferredOpenAIUpstream = providercompat.TransformerOpenAI2
	case providercompat.TransformerClaude:
		if options.EnableAutoSelect {
			state.autoSelect = true
		}
		state.supportsClaudeMessages = true
		state.preferredClaudeUpstream = providercompat.TransformerClaude
	default:
		return state, false
	}

	return state, true
}

func shouldPreserveResponsesAfterChatSuccess(state protocolState) bool {
	if state.supportsOpenAIResponses {
		return true
	}
	native := providercompat.NormalizeTransformer(state.transformer)
	if native != providercompat.TransformerOpenAI2 {
		return false
	}
	inferred := providercompat.NormalizeTransformer(providercompat.InferProviderTransformer(state.apiURL, state.model))
	return !providercompat.IsOpenAIChatTransformer(inferred)
}

func protocolStateChanged(before protocolState, after protocolState) bool {
	return before.autoSelect != after.autoSelect ||
		before.transformer != after.transformer ||
		before.supportsOpenAIChat != after.supportsOpenAIChat ||
		before.supportsOpenAIResponses != after.supportsOpenAIResponses ||
		before.supportsClaudeMessages != after.supportsClaudeMessages ||
		before.preferredOpenAIUpstream != after.preferredOpenAIUpstream ||
		before.preferredClaudeUpstream != after.preferredClaudeUpstream
}

func protocolStateFromConfigEndpoint(endpoint config.Endpoint) protocolState {
	return protocolState{
		autoSelect:              endpoint.AutoSelect,
		transformer:             endpoint.Transformer,
		apiURL:                  endpoint.APIUrl,
		model:                   endpoint.Model,
		supportsOpenAIResponses: endpoint.SupportsOpenAIResponses,
		supportsOpenAIChat:      endpoint.SupportsOpenAIChat,
		supportsClaudeMessages:  endpoint.SupportsClaudeMessages,
		preferredClaudeUpstream: endpoint.PreferredClaudeUpstream,
		preferredOpenAIUpstream: endpoint.PreferredOpenAIUpstream,
	}
}

func applyProtocolStateToConfigEndpoint(endpoint *config.Endpoint, state protocolState) {
	endpoint.AutoSelect = state.autoSelect
	endpoint.Transformer = state.transformer
	endpoint.SupportsOpenAIResponses = state.supportsOpenAIResponses
	endpoint.SupportsOpenAIChat = state.supportsOpenAIChat
	endpoint.SupportsClaudeMessages = state.supportsClaudeMessages
	endpoint.PreferredClaudeUpstream = state.preferredClaudeUpstream
	endpoint.PreferredOpenAIUpstream = state.preferredOpenAIUpstream
}

func protocolStateFromStorageEndpoint(endpoint storage.Endpoint) protocolState {
	return protocolState{
		autoSelect:              endpoint.AutoSelect,
		transformer:             endpoint.Transformer,
		apiURL:                  endpoint.APIUrl,
		model:                   endpoint.Model,
		supportsOpenAIResponses: endpoint.SupportsOpenAIResponses,
		supportsOpenAIChat:      endpoint.SupportsOpenAIChat,
		supportsClaudeMessages:  endpoint.SupportsClaudeMessages,
		preferredClaudeUpstream: endpoint.PreferredClaudeUpstream,
		preferredOpenAIUpstream: endpoint.PreferredOpenAIUpstream,
	}
}

func applyProtocolStateToStorageEndpoint(endpoint *storage.Endpoint, state protocolState) {
	endpoint.AutoSelect = state.autoSelect
	endpoint.Transformer = state.transformer
	endpoint.SupportsOpenAIResponses = state.supportsOpenAIResponses
	endpoint.SupportsOpenAIChat = state.supportsOpenAIChat
	endpoint.SupportsClaudeMessages = state.supportsClaudeMessages
	endpoint.PreferredClaudeUpstream = state.preferredClaudeUpstream
	endpoint.PreferredOpenAIUpstream = state.preferredOpenAIUpstream
}
