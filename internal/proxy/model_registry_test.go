package proxy

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/storage"
)

type fakeEndpointModelStore struct {
	mu     sync.Mutex
	models []storage.EndpointModel
}

func (s *fakeEndpointModelStore) GetVerifiedEndpointModels(modelID string) ([]storage.EndpointModel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := []storage.EndpointModel{}
	now := time.Now().UTC()
	for _, model := range s.models {
		if model.ModelID != modelID || !model.Enabled || model.VerificationStatus != storage.EndpointModelStatusVerified {
			continue
		}
		if model.VerificationExpiresAt != nil && model.VerificationExpiresAt.Before(now) {
			continue
		}
		out = append(out, model)
	}
	return out, nil
}

func (s *fakeEndpointModelStore) GetEndpointModels(endpointName string) ([]storage.EndpointModel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := []storage.EndpointModel{}
	for _, model := range s.models {
		if model.EndpointName == endpointName {
			out = append(out, model)
		}
	}
	return out, nil
}

func (s *fakeEndpointModelStore) UpsertEndpointModel(model *storage.EndpointModel) error {
	if model == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	copied := *model
	for i := range s.models {
		if s.models[i].EndpointName == copied.EndpointName && s.models[i].ModelID == copied.ModelID {
			s.models[i] = copied
			return nil
		}
	}
	s.models = append(s.models, copied)
	return nil
}

func (s *fakeEndpointModelStore) endpointModel(endpointName string, modelID string) (storage.EndpointModel, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, model := range s.models {
		if model.EndpointName == endpointName && model.ModelID == modelID {
			return model, true
		}
	}
	return storage.EndpointModel{}, false
}

func TestModelRegistryReturnsVerifiedEndpointNames(t *testing.T) {
	expires := time.Now().UTC().Add(time.Hour)
	registry := newModelRegistry(&fakeEndpointModelStore{models: []storage.EndpointModel{
		{EndpointName: "A", ModelID: "claude-sonnet-4-5-20250929", Enabled: true, VerificationStatus: storage.EndpointModelStatusVerified, VerificationExpiresAt: &expires, UpstreamTransformer: "claude"},
		{EndpointName: "B", ModelID: "claude-sonnet-4-5-20250929", Enabled: false, VerificationStatus: storage.EndpointModelStatusVerified, VerificationExpiresAt: &expires},
		{EndpointName: "C", ModelID: "claude-sonnet-4-5-20250929", Enabled: true, VerificationStatus: storage.EndpointModelStatusDiscovered, VerificationExpiresAt: &expires},
	}})

	candidates, err := registry.verifiedCandidates("claude-sonnet-4-5-20250929")
	if err != nil {
		t.Fatalf("verified candidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].EndpointName != "A" || candidates[0].UpstreamTransformer != "claude" {
		t.Fatalf("unexpected candidates: %#v", candidates)
	}
}

func TestEnqueueModelVerificationSkipsFailedModelBeforeNextAttempt(t *testing.T) {
	nextAttemptAt := time.Now().UTC().Add(24 * time.Hour)
	store := &fakeEndpointModelStore{models: []storage.EndpointModel{
		{
			EndpointName:       "Primary",
			ModelID:            "claude-opus-4-7",
			Source:             storage.EndpointModelSourceLegacy,
			Enabled:            true,
			VerificationStatus: storage.EndpointModelStatusFailed,
			FailureKind:        "unsupported_model",
			NextAttemptAt:      &nextAttemptAt,
		},
	}}
	p := &Proxy{
		modelRegistry: newModelRegistry(store),
		modelVerifier: newModelVerifier(&http.Client{Timeout: time.Millisecond}),
	}

	p.enqueueModelVerification("claude-opus-4-7", []config.Endpoint{
		{Name: "Primary", APIUrl: "http://127.0.0.1:1", APIKey: "test-key", AuthMode: config.AuthModeAPIKey, Enabled: true, Transformer: "claude"},
	})

	model, ok := store.endpointModel("Primary", "claude-opus-4-7")
	if !ok {
		t.Fatal("expected endpoint model to remain stored")
	}
	if model.VerificationStatus != storage.EndpointModelStatusFailed {
		t.Fatalf("expected failed model to stay failed until next attempt, got %#v", model)
	}
	if model.FailureKind != "unsupported_model" {
		t.Fatalf("expected failure kind to be preserved, got %#v", model)
	}
	if model.NextAttemptAt == nil || !model.NextAttemptAt.Equal(nextAttemptAt) {
		t.Fatalf("expected next attempt to be preserved, got %#v", model.NextAttemptAt)
	}
}

func TestAutoModelVerificationEndpointsOnlyIncludesPrimaryEndpoint(t *testing.T) {
	primary := config.Endpoint{Name: "Primary", APIUrl: "https://primary.example.com", Enabled: true}
	fallback := config.Endpoint{Name: "Fallback", APIUrl: "https://fallback.example.com", Enabled: true}
	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints([]config.Endpoint{primary, fallback})
	p := &Proxy{config: cfg}

	selected := p.autoModelVerificationEndpoints([]config.Endpoint{fallback, primary})
	if len(selected) != 1 || selected[0].Name != "Primary" {
		t.Fatalf("expected only primary endpoint to be auto-verified, got %#v", selected)
	}

	selected = p.autoModelVerificationEndpoints([]config.Endpoint{fallback})
	if len(selected) != 0 {
		t.Fatalf("expected fallback-only candidate list not to auto-verify, got %#v", selected)
	}
}
