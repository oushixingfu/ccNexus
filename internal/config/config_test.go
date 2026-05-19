package config

import "testing"

func TestNormalizeThinkingEffortPreservesProviderDefault(t *testing.T) {
	tests := map[string]string{
		"":        "",
		" ":       "",
		"default": "",
		"auto":    "",
		"inherit": "",
		"off":     "off",
		"low":     "low",
		"medium":  "medium",
		"high":    "high",
		"xhigh":   "xhigh",
		"invalid": "off",
	}

	for input, want := range tests {
		if got := NormalizeThinkingEffort(input); got != want {
			t.Fatalf("NormalizeThinkingEffort(%q) = %q, want %q", input, got, want)
		}
	}
}

type fakeConfigStorage struct {
	endpoints []StorageEndpoint
	configs   map[string]string
}

func newFakeConfigStorage() *fakeConfigStorage {
	return &fakeConfigStorage{configs: make(map[string]string)}
}

func (s *fakeConfigStorage) GetEndpoints() ([]StorageEndpoint, error) {
	endpoints := make([]StorageEndpoint, len(s.endpoints))
	copy(endpoints, s.endpoints)
	return endpoints, nil
}

func (s *fakeConfigStorage) SaveEndpoint(ep *StorageEndpoint) error {
	s.endpoints = append(s.endpoints, *ep)
	return nil
}

func (s *fakeConfigStorage) UpdateEndpoint(ep *StorageEndpoint) error {
	for i := range s.endpoints {
		if s.endpoints[i].Name == ep.Name {
			s.endpoints[i] = *ep
			return nil
		}
	}
	s.endpoints = append(s.endpoints, *ep)
	return nil
}

func (s *fakeConfigStorage) DeleteEndpoint(name string) error {
	for i := range s.endpoints {
		if s.endpoints[i].Name == name {
			s.endpoints = append(s.endpoints[:i], s.endpoints[i+1:]...)
			return nil
		}
	}
	return nil
}

func (s *fakeConfigStorage) GetConfig(key string) (string, error) {
	return s.configs[key], nil
}

func (s *fakeConfigStorage) SetConfig(key, value string) error {
	s.configs[key] = value
	return nil
}

func TestLoadFromStorageUsesDefaultFailover(t *testing.T) {
	store := newFakeConfigStorage()

	cfg, err := LoadFromStorage(store)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	failover := cfg.GetFailover()
	if failover.RecoveredEndpointPolicy != RecoveredEndpointPolicyAutoReturn {
		t.Fatalf("expected default policy %q, got %q", RecoveredEndpointPolicyAutoReturn, failover.RecoveredEndpointPolicy)
	}
	if failover.Cooldowns.QuotaExhaustedSec != 3600 ||
		failover.Cooldowns.RateLimitedSec != 120 ||
		failover.Cooldowns.UpstreamErrorSec != 60 ||
		failover.Cooldowns.NetworkErrorSec != 30 ||
		failover.Cooldowns.TokenUnavailableSec != 600 ||
		failover.Cooldowns.ConfigErrorSec != 1800 {
		t.Fatalf("unexpected default cooldowns: %#v", failover.Cooldowns)
	}
}

func TestRoutingStrategyPersistsAndNormalizes(t *testing.T) {
	store := newFakeConfigStorage()
	cfg := DefaultConfig()
	cfg.UpdateEndpoints(nil)
	cfg.UpdateRoutingStrategy(RoutingStrategyClaude)

	if err := cfg.SaveToStorage(store); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if got := store.configs["routingStrategy"]; got != RoutingStrategyClaude {
		t.Fatalf("expected stored routing strategy %q, got %q", RoutingStrategyClaude, got)
	}

	reloaded, err := LoadFromStorage(store)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if got := reloaded.GetRoutingStrategy(); got != RoutingStrategyClaude {
		t.Fatalf("expected routing strategy %q, got %q", RoutingStrategyClaude, got)
	}

	cfg.UpdateRoutingStrategy("bad-strategy")
	if got := cfg.GetRoutingStrategy(); got != RoutingStrategyAuto {
		t.Fatalf("expected invalid strategy to normalize to %q, got %q", RoutingStrategyAuto, got)
	}
}

func TestFailoverConfigPersistsAndNormalizes(t *testing.T) {
	store := newFakeConfigStorage()
	cfg := DefaultConfig()
	cfg.UpdateEndpoints(nil)
	cfg.UpdateFailover(&FailoverConfig{
		RecoveredEndpointPolicy: RecoveredEndpointPolicyAutoReturn,
		Cooldowns: &FailoverCooldownConfig{
			QuotaExhaustedSec:   0,
			RateLimitedSec:      7,
			UpstreamErrorSec:    8,
			NetworkErrorSec:     9,
			TokenUnavailableSec: 10,
			ConfigErrorSec:      11,
		},
	})

	if err := cfg.SaveToStorage(store); err != nil {
		t.Fatalf("save config: %v", err)
	}
	reloaded, err := LoadFromStorage(store)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	failover := reloaded.GetFailover()
	if failover.RecoveredEndpointPolicy != RecoveredEndpointPolicyAutoReturn {
		t.Fatalf("expected persisted policy auto_return, got %q", failover.RecoveredEndpointPolicy)
	}
	if failover.Cooldowns.QuotaExhaustedSec != 0 ||
		failover.Cooldowns.RateLimitedSec != 7 ||
		failover.Cooldowns.UpstreamErrorSec != 8 ||
		failover.Cooldowns.NetworkErrorSec != 9 ||
		failover.Cooldowns.TokenUnavailableSec != 10 ||
		failover.Cooldowns.ConfigErrorSec != 11 {
		t.Fatalf("unexpected persisted cooldowns: %#v", failover.Cooldowns)
	}

	cfg.UpdateFailover(&FailoverConfig{
		RecoveredEndpointPolicy: "bad-policy",
		Cooldowns: &FailoverCooldownConfig{
			QuotaExhaustedSec: -1,
		},
	})
	failover = cfg.GetFailover()
	if failover.RecoveredEndpointPolicy != RecoveredEndpointPolicyAutoReturn {
		t.Fatalf("expected invalid policy to normalize to auto_return, got %q", failover.RecoveredEndpointPolicy)
	}
	if failover.Cooldowns.QuotaExhaustedSec != 3600 {
		t.Fatalf("expected negative cooldown to normalize to default, got %d", failover.Cooldowns.QuotaExhaustedSec)
	}
}

func TestLegacyDeprioritizeFailoverPolicyNormalizesToAutoReturn(t *testing.T) {
	store := newFakeConfigStorage()
	store.configs["failover_recoveredEndpointPolicy"] = RecoveredEndpointPolicyDeprioritize

	cfg, err := LoadFromStorage(store)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if got := cfg.GetFailover().RecoveredEndpointPolicy; got != RecoveredEndpointPolicyAutoReturn {
		t.Fatalf("expected legacy deprioritize policy to normalize to auto_return, got %q", got)
	}
}

func TestUnifiedModelConfigPersistsAndMatchesAliases(t *testing.T) {
	store := newFakeConfigStorage()
	cfg := DefaultConfig()
	cfg.UpdateEndpoints(nil)
	cfg.UpdateUnifiedModel(&UnifiedModelConfig{
		Enabled:                          true,
		Name:                             "gpt-auto",
		Aliases:                          []string{"gpt-5.5", " GPT-5.5 ", "gpt-auto", ""},
		AdvertiseOnlyUnifiedModel:        true,
		EndpointScope:                    UnifiedModelEndpointScopeAllEnabled,
		HotStandby:                       true,
		PreserveExplicitEndpointOverride: true,
	})

	if err := cfg.SaveToStorage(store); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if got := store.configs["unifiedModel_enabled"]; got != "true" {
		t.Fatalf("expected unified model enabled to persist true, got %q", got)
	}
	if got := store.configs["unifiedModel_name"]; got != "gpt-auto" {
		t.Fatalf("expected unified model name to persist, got %q", got)
	}

	reloaded, err := LoadFromStorage(store)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	unified := reloaded.GetUnifiedModel()
	if !unified.Enabled || unified.Name != "gpt-auto" {
		t.Fatalf("unexpected unified model config: %#v", unified)
	}
	if len(unified.Aliases) != 1 || unified.Aliases[0] != "gpt-5.5" {
		t.Fatalf("expected duplicate/self aliases to be normalized, got %#v", unified.Aliases)
	}
	if !UnifiedModelMatches(unified, "GPT-5.5") || !UnifiedModelMatches(unified, "gpt-auto") {
		t.Fatalf("expected unified model name and aliases to match")
	}
	if UnifiedModelMatches(unified, "other-model") {
		t.Fatalf("did not expect unrelated model to match")
	}
}

func TestReplaceFromUpdatesConfigWithoutCopyingLock(t *testing.T) {
	current := DefaultConfig()
	current.UpdatePort(3021)
	current.UpdateEndpoints([]Endpoint{{Name: "old", APIUrl: "https://old.example.com", APIKey: "sk-old", AuthMode: AuthModeAPIKey, Enabled: true, Transformer: "openai", Model: "gpt-4"}})

	next := DefaultConfig()
	next.UpdatePort(3022)
	next.UpdateRoutingStrategy(RoutingStrategyClaude)
	next.UpdateEndpoints([]Endpoint{{Name: "new", APIUrl: "https://new.example.com", APIKey: "sk-new", AuthMode: AuthModeAPIKey, Enabled: true, Transformer: "openai2", Model: "gpt-5.5"}})
	next.UpdateUnifiedModel(&UnifiedModelConfig{Enabled: true, Name: "gpt-auto", AdvertiseOnlyUnifiedModel: true, EndpointScope: UnifiedModelEndpointScopeAllEnabled, HotStandby: true, PreserveExplicitEndpointOverride: true})

	current.ReplaceFrom(next)

	if got := current.GetPort(); got != 3022 {
		t.Fatalf("expected replaced port 3022, got %d", got)
	}
	if got := current.GetRoutingStrategy(); got != RoutingStrategyClaude {
		t.Fatalf("expected replaced routing strategy %q, got %q", RoutingStrategyClaude, got)
	}
	endpoints := current.GetEndpoints()
	if len(endpoints) != 1 || endpoints[0].Name != "new" || endpoints[0].Model != "gpt-5.5" {
		t.Fatalf("expected replaced endpoints, got %#v", endpoints)
	}
	if unified := current.GetUnifiedModel(); !unified.Enabled || unified.Name != "gpt-auto" {
		t.Fatalf("expected replaced unified model config, got %#v", unified)
	}
}
