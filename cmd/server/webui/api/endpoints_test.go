package api

import (
	"testing"
	"time"

	"github.com/lich0821/ccNexus/internal/storage"
)

func TestDeriveEndpointAvailabilityUsesRuntimeFailure(t *testing.T) {
	failureAt := time.Now().UTC()
	status := &storage.EndpointRuntimeStatus{
		LastFailureAt:         &failureAt,
		LastFailureReason:     "quota_exhausted",
		LastFailureStatusCode: 403,
	}

	available, availability, reason, statusCode := deriveEndpointAvailability(true, status)
	if available || availability != "unavailable" || reason != "quota_exhausted" || statusCode != 403 {
		t.Fatalf("expected unavailable quota status, got available=%t availability=%q reason=%q statusCode=%d", available, availability, reason, statusCode)
	}
}

func TestDeriveEndpointAvailabilityClearsAfterLaterSuccess(t *testing.T) {
	failureAt := time.Now().Add(-time.Minute).UTC()
	successAt := time.Now().UTC()
	status := &storage.EndpointRuntimeStatus{
		LastSuccessAt:         &successAt,
		LastFailureAt:         &failureAt,
		LastFailureReason:     "upstream_5xx",
		LastFailureStatusCode: 503,
	}

	available, availability, reason, statusCode := deriveEndpointAvailability(true, status)
	if !available || availability != "available" || reason != "" || statusCode != 0 {
		t.Fatalf("expected available after later success, got available=%t availability=%q reason=%q statusCode=%d", available, availability, reason, statusCode)
	}
}

func TestDeriveEndpointAvailabilityDisabled(t *testing.T) {
	available, availability, reason, statusCode := deriveEndpointAvailability(false, nil)
	if available || availability != "disabled" || reason != "" || statusCode != 0 {
		t.Fatalf("expected disabled status, got available=%t availability=%q reason=%q statusCode=%d", available, availability, reason, statusCode)
	}
}

func TestBuildEndpointResponseIncludesEffectiveUpstreams(t *testing.T) {
	endpoint := storage.Endpoint{
		Name:                    "gateway",
		Enabled:                 true,
		Transformer:             "openai2",
		Model:                   "gpt-5.5",
		AutoSelect:              true,
		SupportsOpenAIResponses: true,
		SupportsOpenAIChat:      true,
	}

	response := buildEndpointResponse(endpoint, nil)
	if response.EffectiveClaudeUpstream != "openai" {
		t.Fatalf("expected Claude Code effective upstream openai, got %q", response.EffectiveClaudeUpstream)
	}
	if response.EffectiveOpenAIChatUpstream != "openai" {
		t.Fatalf("expected OpenAI Chat effective upstream openai, got %q", response.EffectiveOpenAIChatUpstream)
	}
	if response.EffectiveOpenAIResponsesUpstream != "openai2" {
		t.Fatalf("expected OpenAI Responses effective upstream openai2, got %q", response.EffectiveOpenAIResponsesUpstream)
	}
}
