package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/storage"
)

func TestLoadModelsUsesConfiguredEndpointModelWithoutFetchingUpstream(t *testing.T) {
	var modelRequests int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" || r.URL.Path == "/models" {
			atomic.AddInt32(&modelRequests, 1)
		}
		writeProxyJSON(w, map[string]interface{}{
			"object": "list",
			"data": []map[string]interface{}{
				{"id": "upstream-model-a", "object": "model", "owned_by": "upstream"},
				{"id": "upstream-model-b", "object": "model", "owned_by": "upstream"},
			},
		})
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints([]config.Endpoint{
		{
			Name:        "ConfiguredOnly",
			APIUrl:      upstream.URL,
			APIKey:      "test-key",
			AuthMode:    config.AuthModeAPIKey,
			Enabled:     true,
			Transformer: "openai",
			Model:       "configured-model",
		},
	})

	p := &Proxy{
		config:      cfg,
		httpClient:  upstream.Client(),
		modelsCache: NewModelsCache(30),
	}

	models := p.loadModelsForResponse(true)

	if got := atomic.LoadInt32(&modelRequests); got != 0 {
		t.Fatalf("expected no upstream model requests, got %d", got)
	}
	if len(models) != 1 {
		t.Fatalf("expected one configured model, got %#v", models)
	}
	if models[0].ID != "configured-model" {
		t.Fatalf("expected configured model, got %#v", models[0])
	}
	if models[0].EndpointID != "ConfiguredOnly" {
		t.Fatalf("expected endpoint id ConfiguredOnly, got %q", models[0].EndpointID)
	}
}

func TestLoadModelsFetchesUpstreamWhenEndpointModelIsEmpty(t *testing.T) {
	var modelRequests int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" && r.URL.Path != "/models" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		atomic.AddInt32(&modelRequests, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"object": "list",
			"data": []map[string]interface{}{
				{"id": "upstream-model-a", "object": "model", "owned_by": "upstream"},
				{"id": "upstream-model-b", "object": "model", "owned_by": "upstream"},
			},
		})
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints([]config.Endpoint{
		{
			Name:        "Discoverable",
			APIUrl:      upstream.URL,
			APIKey:      "test-key",
			AuthMode:    config.AuthModeAPIKey,
			Enabled:     true,
			Transformer: "openai",
		},
	})

	p := &Proxy{
		config:      cfg,
		httpClient:  upstream.Client(),
		modelsCache: NewModelsCache(30),
	}

	models := p.loadModelsForResponse(true)

	if got := atomic.LoadInt32(&modelRequests); got != 1 {
		t.Fatalf("expected one upstream model request, got %d", got)
	}
	if len(models) != 2 {
		t.Fatalf("expected upstream models, got %#v", models)
	}
	if models[0].ID != "upstream-model-a" || models[1].ID != "upstream-model-b" {
		t.Fatalf("unexpected upstream models: %#v", models)
	}
}

func TestLoadModelsReturnsOnlyVerifiedEndpointModels(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints([]config.Endpoint{
		{Name: "ClaudeA", APIUrl: "https://a.example.com", APIKey: "test-key", AuthMode: config.AuthModeAPIKey, Enabled: true, Transformer: "claude", Model: "legacy-default"},
		{Name: "ClaudeB", APIUrl: "https://b.example.com", APIKey: "test-key", AuthMode: config.AuthModeAPIKey, Enabled: true, Transformer: "claude", Model: "legacy-default"},
	})
	store := &fakeEndpointModelStore{models: []storage.EndpointModel{
		{EndpointName: "ClaudeA", ModelID: "claude-sonnet-4-5-20250929", Enabled: true, VerificationStatus: storage.EndpointModelStatusVerified, UpstreamTransformer: "claude"},
		{EndpointName: "ClaudeB", ModelID: "claude-sonnet-4-5-20250929", Enabled: true, VerificationStatus: storage.EndpointModelStatusDiscovered, UpstreamTransformer: "claude"},
	}}
	p := &Proxy{config: cfg, modelsCache: NewModelsCache(30), modelRegistry: newModelRegistry(store)}

	models := p.loadModelsForResponse(true)

	if len(models) != 1 {
		t.Fatalf("expected one verified model, got %#v", models)
	}
	if models[0].ID != "claude-sonnet-4-5-20250929" || models[0].EndpointID != "ClaudeA" {
		t.Fatalf("unexpected verified model response: %#v", models[0])
	}
}

func TestLoadModelsReturnsUnifiedModelWhenAdvertised(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints([]config.Endpoint{
		{Name: "A", APIUrl: "https://a.example.com", APIKey: "test-key", AuthMode: config.AuthModeAPIKey, Enabled: true, Transformer: "openai2", Model: "real-a"},
		{Name: "B", APIUrl: "https://b.example.com", APIKey: "test-key", AuthMode: config.AuthModeAPIKey, Enabled: true, Transformer: "openai2", Model: "real-b"},
	})
	cfg.UpdateUnifiedModel(&config.UnifiedModelConfig{
		Enabled:                          true,
		Name:                             "gpt-5.5",
		AdvertiseOnlyUnifiedModel:        true,
		EndpointScope:                    config.UnifiedModelEndpointScopeAllEnabled,
		HotStandby:                       true,
		PreserveExplicitEndpointOverride: true,
	})
	p := &Proxy{config: cfg, modelsCache: NewModelsCache(30)}

	models := p.loadModelsForResponse(true)

	if len(models) != 1 {
		t.Fatalf("expected one unified model, got %#v", models)
	}
	if models[0].ID != "gpt-5.5" || models[0].EndpointID != "unified" {
		t.Fatalf("unexpected unified model response: %#v", models[0])
	}
}
