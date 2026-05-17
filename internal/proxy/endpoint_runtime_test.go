package proxy

import (
	"testing"
	"time"
)

func TestMarkEndpointAvailableClearsCooldownAndRuntimeBlock(t *testing.T) {
	p := &Proxy{
		endpointCooldowns:       make(map[string]endpointCooldown),
		runtimeBlockedEndpoints: make(map[string]string),
	}
	p.setEndpointCooldownUntil("Primary", "upstream_5xx", time.Now().Add(time.Hour))
	p.setRuntimeBlockedEndpoint("Primary", retryReasonSemanticEmptyResponse)

	p.MarkEndpointAvailable("Primary")

	if _, cooled := p.endpointCooldown("Primary"); cooled {
		t.Fatal("expected cooldown to be cleared")
	}
	if reason := p.runtimeBlockedReason("Primary"); reason != "" {
		t.Fatalf("expected runtime block to be cleared, got %q", reason)
	}
}
