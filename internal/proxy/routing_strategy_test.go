package proxy

import (
	"testing"
	"time"

	"github.com/lich0821/ccNexus/internal/config"
)

func TestRoutingPreferenceForRequest(t *testing.T) {
	tests := []struct {
		name         string
		strategy     string
		clientFormat ClientFormat
		model        string
		want         string
	}{
		{
			name:         "auto claude code claude model",
			strategy:     config.RoutingStrategyAuto,
			clientFormat: ClientFormatClaude,
			model:        "claude-sonnet-4-5-20250929",
			want:         routingPreferenceClaude,
		},
		{
			name:         "auto responses gpt model",
			strategy:     config.RoutingStrategyAuto,
			clientFormat: ClientFormatOpenAIResponses,
			model:        "gpt-5.5",
			want:         routingPreferenceCodex,
		},
		{
			name:         "auto claude protocol gpt model",
			strategy:     config.RoutingStrategyAuto,
			clientFormat: ClientFormatClaude,
			model:        "gpt-5.5",
			want:         routingPreferenceCodex,
		},
		{
			name:         "auto unknown model",
			strategy:     config.RoutingStrategyAuto,
			clientFormat: ClientFormatOpenAIResponses,
			model:        "llama-3.3",
			want:         routingPreferenceNone,
		},
		{
			name:         "forced claude",
			strategy:     config.RoutingStrategyClaude,
			clientFormat: ClientFormatOpenAIResponses,
			model:        "gpt-5.5",
			want:         routingPreferenceClaude,
		},
		{
			name:         "forced codex",
			strategy:     config.RoutingStrategyCodex,
			clientFormat: ClientFormatClaude,
			model:        "claude-sonnet-4-5-20250929",
			want:         routingPreferenceCodex,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.UpdateRoutingStrategy(tt.strategy)
			p := New(cfg, &noopStatsStorage{}, nil, "test-device")

			if got := p.routingPreferenceForRequest(tt.clientFormat, tt.model); got != tt.want {
				t.Fatalf("expected preference %q, got %q", tt.want, got)
			}
		})
	}
}

func TestApplyRoutingStrategyKeepsOrderWithinPreferredAndFallbackClasses(t *testing.T) {
	endpoints := []config.Endpoint{
		routingTestEndpoint("other", "openai", "llama-3.3"),
		routingTestEndpoint("codex-a", "openai2", "gpt-5.5"),
		routingTestEndpoint("claude", "claude", "claude-sonnet-4-5-20250929"),
		routingTestEndpoint("codex-b", "openai2", "gpt-4.1"),
	}
	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints(endpoints)
	p := New(cfg, &noopStatsStorage{}, nil, "test-device")

	routed := p.applyRoutingStrategyToRequestPlan(endpoints, routingPreferenceCodex)
	assertEndpointOrder(t, routed, []string{"codex-a", "codex-b", "claude", "other"})
}

func TestApplyRoutingStrategyPrefersOppositeCompatibleClassBeforeOtherEndpoints(t *testing.T) {
	tests := []struct {
		name       string
		preference string
		endpoints  []config.Endpoint
		want       []string
	}{
		{
			name:       "gpt request prefers claude fallback before other endpoints",
			preference: routingPreferenceCodex,
			endpoints: []config.Endpoint{
				routingTestEndpoint("other", "openai", "llama-3.3"),
				routingTestEndpoint("claude", "claude", "claude-sonnet-4-5-20250929"),
				routingTestEndpoint("codex", "openai2", "gpt-5.5"),
			},
			want: []string{"codex", "claude", "other"},
		},
		{
			name:       "claude request prefers codex fallback before other endpoints",
			preference: routingPreferenceClaude,
			endpoints: []config.Endpoint{
				routingTestEndpoint("other", "openai", "llama-3.3"),
				routingTestEndpoint("codex", "openai2", "gpt-5.5"),
				routingTestEndpoint("claude", "claude", "claude-sonnet-4-5-20250929"),
			},
			want: []string{"claude", "codex", "other"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.UpdateEndpoints(tt.endpoints)
			p := New(cfg, &noopStatsStorage{}, nil, "test-device")

			routed := p.applyRoutingStrategyToRequestPlan(tt.endpoints, tt.preference)
			assertEndpointOrder(t, routed, tt.want)
		})
	}
}

func TestApplyRoutingStrategyKeepsPreferredFallbackClassWhenOnlyOppositeEndpointsExist(t *testing.T) {
	tests := []struct {
		name       string
		preference string
		endpoints  []config.Endpoint
		want       []string
	}{
		{
			name:       "gpt request falls back to claude endpoints",
			preference: routingPreferenceCodex,
			endpoints: []config.Endpoint{
				routingTestEndpoint("claude-a", "claude", "claude-sonnet-4-5-20250929"),
				routingTestEndpoint("claude-b", "claude", "claude-opus-4-20250514"),
			},
			want: []string{"claude-a", "claude-b"},
		},
		{
			name:       "claude request falls back to codex endpoints",
			preference: routingPreferenceClaude,
			endpoints: []config.Endpoint{
				routingTestEndpoint("codex-a", "openai2", "gpt-5.5"),
				routingTestEndpoint("codex-b", "openai2", "gpt-4.1"),
			},
			want: []string{"codex-a", "codex-b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.UpdateEndpoints(tt.endpoints)
			p := New(cfg, &noopStatsStorage{}, nil, "test-device")

			routed := p.applyRoutingStrategyToRequestPlan(tt.endpoints, tt.preference)
			assertEndpointOrder(t, routed, tt.want)
		})
	}
}

func TestApplyRoutingStrategyDoesNotTreatOpenAIChatAsCodexCapableOnlyByModel(t *testing.T) {
	endpoints := []config.Endpoint{
		routingTestEndpoint("openai-chat", "openai", "gpt-5.5"),
		routingTestEndpoint("claude", "claude", "claude-sonnet-4-5-20250929"),
		routingTestEndpoint("codex", "openai2", "gpt-5.5"),
	}
	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints(endpoints)
	p := New(cfg, &noopStatsStorage{}, nil, "test-device")

	routed := p.applyRoutingStrategyToRequestPlan(endpoints, routingPreferenceCodex)
	assertEndpointOrder(t, routed, []string{"codex", "claude", "openai-chat"})
}

func TestApplyRoutingStrategyKeepsRecoveredDeprioritizedEndpointsLast(t *testing.T) {
	endpoints := []config.Endpoint{
		routingTestEndpoint("recovered-codex", "openai2", "gpt-5.5"),
		routingTestEndpoint("other", "openai", "llama-3.3"),
		routingTestEndpoint("codex", "openai2", "gpt-4.1"),
	}
	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints(endpoints)
	p := New(cfg, &noopStatsStorage{}, nil, "test-device")
	p.cooldownMu.Lock()
	p.endpointCooldowns["recovered-codex"] = endpointCooldown{Reason: "quota_exhausted", Until: time.Now().Add(-time.Second)}
	p.cooldownMu.Unlock()

	available := p.getRequestPlanEndpoints(endpoints, requestObservability{RequestID: "routing-recovered"})
	routed := p.applyRoutingStrategyToRequestPlan(available, routingPreferenceCodex)
	assertEndpointOrder(t, routed, []string{"codex", "other", "recovered-codex"})
}

func TestRoutingStrategyStartsAtPreferredEndpointWhenCurrentDoesNotMatch(t *testing.T) {
	endpoints := []config.Endpoint{
		routingTestEndpoint("other", "openai", "llama-3.3"),
		routingTestEndpoint("codex", "openai2", "gpt-5.5"),
		routingTestEndpoint("claude", "claude", "claude-sonnet-4-5-20250929"),
	}
	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints(endpoints)
	p := New(cfg, &noopStatsStorage{}, nil, "test-device")
	if err := p.SetCurrentEndpoint("other"); err != nil {
		t.Fatalf("set current endpoint: %v", err)
	}

	available := p.getRequestPlanEndpoints(endpoints, requestObservability{RequestID: "routing-current"})
	preference := p.routingPreferenceForRequest(ClientFormatOpenAIResponses, "gpt-5.5")
	routed := p.applyRoutingStrategyToRequestPlan(available, preference)
	currentName := p.GetCurrentEndpointName()
	if currentEndpoint, ok := findEndpointByName(routed, currentName); !ok || !endpointMatchesRoutingPreference(currentEndpoint, preference) {
		currentName = ""
	}
	plan := newRequestEndpointPlanForCurrentWithSkip(routed, routed, currentName, p.isEndpointDeprioritized(currentName))

	if got := plan.Current().Name; got != "codex" {
		t.Fatalf("expected request plan to start at codex, got %q", got)
	}
}

func routingTestEndpoint(name string, transformer string, model string) config.Endpoint {
	return config.Endpoint{
		Name:        name,
		APIUrl:      "https://" + name + ".example.com",
		APIKey:      "test-key",
		AuthMode:    config.AuthModeAPIKey,
		Enabled:     true,
		Transformer: transformer,
		Model:       model,
		AutoSelect:  true,
	}
}

func assertEndpointOrder(t *testing.T, endpoints []config.Endpoint, want []string) {
	t.Helper()
	if len(endpoints) != len(want) {
		t.Fatalf("expected %d endpoints, got %d: %#v", len(want), len(endpoints), endpoints)
	}
	for i, name := range want {
		if endpoints[i].Name != name {
			t.Fatalf("expected endpoint[%d] %q, got %q", i, name, endpoints[i].Name)
		}
	}
}
