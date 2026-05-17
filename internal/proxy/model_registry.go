package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/logger"
	"github.com/lich0821/ccNexus/internal/providercompat"
	"github.com/lich0821/ccNexus/internal/storage"
)

type endpointModelReader interface {
	GetVerifiedEndpointModels(modelID string) ([]storage.EndpointModel, error)
}

type endpointModelWriter interface {
	UpsertEndpointModel(model *storage.EndpointModel) error
}

type endpointModelLister interface {
	GetEndpointModels(endpointName string) ([]storage.EndpointModel, error)
}

type modelRegistry struct {
	store    endpointModelReader
	inFlight map[string]struct{}
	mu       sync.Mutex
}

func newModelRegistry(store endpointModelReader) *modelRegistry {
	return &modelRegistry{store: store, inFlight: make(map[string]struct{})}
}

func (r *modelRegistry) verifiedCandidates(modelID string) ([]storage.EndpointModel, error) {
	if r == nil || r.store == nil {
		return nil, nil
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil, nil
	}
	return r.store.GetVerifiedEndpointModels(modelID)
}

func (r *modelRegistry) modelsForEndpoint(endpointName string) ([]storage.EndpointModel, bool, error) {
	if r == nil || r.store == nil {
		return nil, false, nil
	}
	lister, ok := r.store.(endpointModelLister)
	if !ok || lister == nil {
		return nil, false, nil
	}
	models, err := lister.GetEndpointModels(strings.TrimSpace(endpointName))
	if err != nil {
		return nil, true, err
	}
	return models, true, nil
}

func verifiedEndpointModels(models []storage.EndpointModel, now time.Time) []storage.EndpointModel {
	out := make([]storage.EndpointModel, 0, len(models))
	for _, model := range models {
		if !model.Enabled || model.VerificationStatus != storage.EndpointModelStatusVerified {
			continue
		}
		if model.VerificationExpiresAt != nil && !model.VerificationExpiresAt.After(now) {
			continue
		}
		out = append(out, model)
	}
	return out
}

func (r *modelRegistry) hasEndpointModelRows(endpoints []config.Endpoint) bool {
	if r == nil {
		return false
	}
	for _, endpoint := range endpoints {
		models, ok, err := r.modelsForEndpoint(endpoint.Name)
		if err != nil {
			return true
		}
		if !ok {
			return true
		}
		if len(models) > 0 {
			return true
		}
	}
	return false
}

func endpointModelCandidateMap(candidates []storage.EndpointModel) map[string]storage.EndpointModel {
	byEndpoint := make(map[string]storage.EndpointModel, len(candidates))
	for _, candidate := range candidates {
		byEndpoint[candidate.EndpointName] = candidate
	}
	return byEndpoint
}

func filterEndpointsByVerifiedModel(endpoints []config.Endpoint, candidates []storage.EndpointModel) ([]config.Endpoint, map[string]storage.EndpointModel) {
	candidateMap := endpointModelCandidateMap(candidates)
	filtered := make([]config.Endpoint, 0, len(endpoints))
	filteredMap := make(map[string]storage.EndpointModel, len(candidates))
	for _, endpoint := range endpoints {
		candidate, ok := candidateMap[endpoint.Name]
		if !ok {
			continue
		}
		filtered = append(filtered, endpoint)
		filteredMap[endpoint.Name] = candidate
	}
	return filtered, filteredMap
}

func (p *Proxy) enqueueModelVerification(modelID string, endpoints []config.Endpoint) {
	if p == nil || p.modelRegistry == nil || p.modelVerifier == nil {
		return
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return
	}
	writer, ok := p.modelRegistry.store.(endpointModelWriter)
	if !ok || writer == nil {
		return
	}
	for _, endpoint := range endpoints {
		if strings.TrimSpace(endpoint.Name) == "" || !endpoint.Enabled {
			continue
		}
		now := time.Now().UTC()
		due, err := p.modelRegistry.verificationDue(endpoint.Name, modelID, now)
		if err != nil {
			logger.Warn("[%s] Failed to inspect existing model verification for %s: %v", endpoint.Name, modelID, err)
		}
		if !due {
			continue
		}
		if !p.modelRegistry.beginVerification(endpoint.Name, modelID) {
			continue
		}
		if err := writer.UpsertEndpointModel(&storage.EndpointModel{
			EndpointName:       endpoint.Name,
			ModelID:            modelID,
			Source:             storage.EndpointModelSourceDiscovered,
			Enabled:            true,
			VerificationStatus: storage.EndpointModelStatusVerifying,
			LastAttemptAt:      &now,
		}); err != nil {
			p.modelRegistry.finishVerification(endpoint.Name, modelID)
			logger.Warn("[%s] Failed to enqueue model verification for %s: %v", endpoint.Name, modelID, err)
			continue
		}
		go p.verifyAndStoreEndpointModel(endpoint, modelID)
	}
}

func (p *Proxy) QueueModelVerification(endpointName string, modelID string) bool {
	if p == nil || p.config == nil || p.modelRegistry == nil || p.modelVerifier == nil {
		return false
	}
	endpointName = strings.TrimSpace(endpointName)
	modelID = strings.TrimSpace(modelID)
	if endpointName == "" || modelID == "" {
		return false
	}
	for _, endpoint := range p.config.GetEndpoints() {
		if endpoint.Enabled && endpoint.Name == endpointName {
			p.enqueueModelVerification(modelID, []config.Endpoint{endpoint})
			return true
		}
	}
	return false
}

func (r *modelRegistry) verificationDue(endpointName string, modelID string, now time.Time) (bool, error) {
	if r == nil {
		return true, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	modelID = strings.TrimSpace(modelID)
	models, ok, err := r.modelsForEndpoint(endpointName)
	if err != nil || !ok {
		return true, err
	}
	for _, model := range models {
		if strings.TrimSpace(model.ModelID) != modelID {
			continue
		}
		return endpointModelVerificationDue(model, now), nil
	}
	return true, nil
}

func endpointModelVerificationDue(model storage.EndpointModel, now time.Time) bool {
	if !model.Enabled {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if model.VerificationStatus == storage.EndpointModelStatusVerified &&
		(model.VerificationExpiresAt == nil || model.VerificationExpiresAt.After(now)) {
		return false
	}
	if model.NextAttemptAt != nil && model.NextAttemptAt.After(now) {
		return false
	}
	if model.VerificationStatus == storage.EndpointModelStatusVerifying &&
		model.LastAttemptAt != nil &&
		now.Sub(*model.LastAttemptAt) < 10*time.Minute {
		return false
	}
	return true
}

func (r *modelRegistry) beginVerification(endpointName string, modelID string) bool {
	if r == nil {
		return false
	}
	key := endpointName + "\x00" + modelID
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.inFlight[key]; ok {
		return false
	}
	r.inFlight[key] = struct{}{}
	return true
}

func (r *modelRegistry) finishVerification(endpointName string, modelID string) {
	if r == nil {
		return
	}
	key := endpointName + "\x00" + modelID
	r.mu.Lock()
	delete(r.inFlight, key)
	r.mu.Unlock()
}

func (p *Proxy) verifyAndStoreEndpointModel(endpoint config.Endpoint, modelID string) {
	defer p.modelRegistry.finishVerification(endpoint.Name, modelID)

	writer, ok := p.modelRegistry.store.(endpointModelWriter)
	if !ok || writer == nil {
		return
	}

	if config.IsTokenPoolAuthMode(endpoint.AuthMode) {
		credential, err := p.selectCredential(endpoint.Name)
		if err != nil {
			logger.Warn("[%s] Failed to select credential for model verification %s: %v", endpoint.Name, modelID, err)
		}
		if credential != nil {
			endpoint.APIKey = strings.TrimSpace(credential.AccessToken)
		}
	}

	now := time.Now().UTC()
	result := p.modelVerifier.verifyEndpointModel(endpoint, modelID)
	ttl := result.VerifiedTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	retryTTL := result.RetryTTL
	if retryTTL <= 0 {
		retryTTL = 10 * time.Minute
	}

	model := &storage.EndpointModel{
		EndpointName:        endpoint.Name,
		ModelID:             modelID,
		Source:              storage.EndpointModelSourceDiscovered,
		Enabled:             true,
		VerificationStatus:  result.Status,
		UpstreamTransformer: result.UpstreamTransformer,
		FailureKind:         result.FailureKind,
		FailureMessage:      result.FailureMessage,
		LastAttemptAt:       &now,
	}
	if model.VerificationStatus == "" {
		model.VerificationStatus = storage.EndpointModelStatusFailed
	}
	if model.VerificationStatus == storage.EndpointModelStatusVerified {
		expiresAt := now.Add(ttl)
		model.LastVerifiedAt = &now
		model.VerificationExpiresAt = &expiresAt
	} else {
		nextAttemptAt := now.Add(retryTTL)
		model.NextAttemptAt = &nextAttemptAt
	}
	if err := writer.UpsertEndpointModel(model); err != nil {
		logger.Warn("[%s] Failed to store model verification result for %s: %v", endpoint.Name, modelID, err)
	}
}

func (p *Proxy) markEndpointModelUnsupported(candidate storage.EndpointModel, message string) bool {
	if p == nil || p.modelRegistry == nil {
		return false
	}
	writer, ok := p.modelRegistry.store.(endpointModelWriter)
	if !ok || writer == nil {
		return false
	}

	endpointName := strings.TrimSpace(candidate.EndpointName)
	modelID := strings.TrimSpace(candidate.ModelID)
	if endpointName == "" || modelID == "" {
		return false
	}

	now := time.Now().UTC()
	nextAttemptAt := now.Add(7 * 24 * time.Hour)
	updated := candidate
	updated.EndpointName = endpointName
	updated.ModelID = modelID
	if updated.Source == "" {
		updated.Source = storage.EndpointModelSourceDiscovered
	}
	updated.Enabled = true
	updated.VerificationStatus = storage.EndpointModelStatusFailed
	updated.FailureKind = "unsupported_model"
	updated.FailureMessage = providercompat.TruncateErrorBody(message)
	updated.LastVerifiedAt = nil
	updated.VerificationExpiresAt = nil
	updated.LastAttemptAt = &now
	updated.NextAttemptAt = &nextAttemptAt

	if err := writer.UpsertEndpointModel(&updated); err != nil {
		logger.Warn("[%s] Failed to mark endpoint model %s unsupported: %v", endpointName, modelID, err)
		return false
	}
	return true
}

func writeModelNotVerifiedError(w http.ResponseWriter, modelID string) {
	message := fmt.Sprintf("model is not verified or available: %s", strings.TrimSpace(modelID))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    "invalid_request_error",
			"code":    "model_not_verified",
		},
	})
}
