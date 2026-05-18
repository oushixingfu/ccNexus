package endpointstate

import (
	"testing"

	"github.com/lich0821/ccNexus/internal/providercompat"
	"github.com/lich0821/ccNexus/internal/storage"
)

func TestApplyProtocolSuccessPreservesNativeResponsesOnChatFallback(t *testing.T) {
	endpoint := &storage.Endpoint{
		Transformer:             providercompat.TransformerOpenAI2,
		Model:                   "gpt-5.5",
		AutoSelect:              true,
		SupportsOpenAIResponses: true,
	}

	changed := ApplyProtocolSuccessToStorageEndpoint(endpoint, providercompat.TransformerOpenAI, ProtocolSuccessOptions{RequireAutoSelect: true})
	if !changed {
		t.Fatal("expected protocol success to add chat capability")
	}
	if endpoint.Transformer != providercompat.TransformerOpenAI2 || !endpoint.SupportsOpenAIResponses || !endpoint.SupportsOpenAIChat {
		t.Fatalf("expected chat fallback to preserve responses endpoint, got transformer=%q responses=%t chat=%t",
			endpoint.Transformer,
			endpoint.SupportsOpenAIResponses,
			endpoint.SupportsOpenAIChat,
		)
	}
	if endpoint.PreferredOpenAIUpstream != "" {
		t.Fatalf("expected automatic OpenAI preference to remain unset, got %q", endpoint.PreferredOpenAIUpstream)
	}
}

func TestApplyProtocolSuccessDemotesInferredChatGateway(t *testing.T) {
	endpoint := &storage.Endpoint{
		Transformer: providercompat.TransformerOpenAI2,
		Model:       "kimi-k2.6",
	}

	changed := ApplyProtocolSuccessToStorageEndpoint(endpoint, providercompat.TransformerKimi, ProtocolSuccessOptions{EnableAutoSelect: true})
	if !changed {
		t.Fatal("expected protocol success to persist chat gateway capability")
	}
	if !endpoint.AutoSelect || endpoint.Transformer != providercompat.TransformerKimi || !endpoint.SupportsOpenAIChat || endpoint.SupportsOpenAIResponses {
		t.Fatalf("expected inferred chat gateway to become Kimi chat, got auto=%t transformer=%q chat=%t responses=%t",
			endpoint.AutoSelect,
			endpoint.Transformer,
			endpoint.SupportsOpenAIChat,
			endpoint.SupportsOpenAIResponses,
		)
	}
	if endpoint.PreferredOpenAIUpstream != providercompat.TransformerOpenAI {
		t.Fatalf("expected OpenAI upstream preference to use chat, got %q", endpoint.PreferredOpenAIUpstream)
	}
}

func TestApplyProtocolSuccessRequiresAutoSelectWhenRequested(t *testing.T) {
	endpoint := &storage.Endpoint{
		Transformer: providercompat.TransformerOpenAI2,
		Model:       "gpt-5.5",
		AutoSelect:  false,
	}

	changed := ApplyProtocolSuccessToStorageEndpoint(endpoint, providercompat.TransformerOpenAI, ProtocolSuccessOptions{RequireAutoSelect: true})
	if changed {
		t.Fatal("expected manual endpoint to ignore protocol success")
	}
	if endpoint.SupportsOpenAIChat || endpoint.Transformer != providercompat.TransformerOpenAI2 {
		t.Fatalf("expected manual endpoint to remain unchanged, got transformer=%q chat=%t", endpoint.Transformer, endpoint.SupportsOpenAIChat)
	}
}

func TestDeriveCapabilitiesInfersNativeTransformerSupport(t *testing.T) {
	responsesCaps := DeriveCapabilities(providercompat.TransformerOpenAI2, false, false, false)
	if !responsesCaps.SupportsTransformer(providercompat.TransformerOpenAI2) || responsesCaps.SupportsTransformer(providercompat.TransformerOpenAI) {
		t.Fatalf("unexpected responses capabilities: %#v", responsesCaps)
	}

	chatCaps := DeriveCapabilities(providercompat.TransformerKimi, false, false, false)
	if !chatCaps.SupportsTransformer(providercompat.TransformerKimi) || !chatCaps.SupportsTransformer(providercompat.TransformerOpenAI) || chatCaps.SupportsTransformer(providercompat.TransformerOpenAI2) {
		t.Fatalf("unexpected chat capabilities: %#v", chatCaps)
	}
}
