package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/storage"
)

type endpointModelRoute struct {
	endpointName string
	modelID      string
	action       string
}

func (h *Handler) handleEndpointModels(w http.ResponseWriter, r *http.Request) {
	route, ok := parseEndpointModelPath(strings.TrimPrefix(r.URL.Path, "/api/endpoints/"))
	if !ok {
		WriteError(w, http.StatusNotFound, "Invalid endpoint model path")
		return
	}
	if exists, err := h.endpointExists(route.endpointName); err != nil {
		WriteError(w, http.StatusInternalServerError, "Failed to load endpoint")
		return
	} else if !exists {
		WriteError(w, http.StatusNotFound, "Endpoint not found")
		return
	}

	switch {
	case route.action == "" && route.modelID == "" && r.Method == http.MethodGet:
		h.listEndpointModels(w, route.endpointName)
	case route.action == "" && route.modelID == "" && r.Method == http.MethodPost:
		h.addEndpointModel(w, r, route.endpointName)
	case route.action == "discover" && r.Method == http.MethodPost:
		h.discoverEndpointModels(w, route.endpointName)
	case route.action == "verify" && r.Method == http.MethodPost:
		h.verifyEndpointModel(w, route.endpointName, route.modelID)
	case route.action == "" && route.modelID != "" && r.Method == http.MethodPut:
		h.updateEndpointModel(w, r, route.endpointName, route.modelID)
	case route.action == "" && route.modelID != "" && r.Method == http.MethodDelete:
		if err := h.storage.DeleteEndpointModel(route.endpointName, route.modelID); err != nil {
			WriteError(w, http.StatusInternalServerError, "Failed to delete endpoint model")
			return
		}
		WriteSuccess(w, map[string]interface{}{"deleted": true})
	default:
		WriteError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (h *Handler) listEndpointModels(w http.ResponseWriter, endpointName string) {
	models, err := h.storage.GetEndpointModels(endpointName)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "Failed to load endpoint models")
		return
	}
	WriteSuccess(w, map[string]interface{}{"models": models})
}

func (h *Handler) addEndpointModel(w http.ResponseWriter, r *http.Request, endpointName string) {
	var req struct {
		ModelID     string `json:"modelId"`
		DisplayName string `json:"displayName"`
		Enabled     *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	model := &storage.EndpointModel{
		EndpointName:       endpointName,
		ModelID:            req.ModelID,
		DisplayName:        req.DisplayName,
		Source:             storage.EndpointModelSourceManual,
		Enabled:            enabled,
		VerificationStatus: storage.EndpointModelStatusUnknown,
	}
	if err := h.storage.UpsertEndpointModel(model); err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	WriteSuccess(w, map[string]interface{}{"model": model})
}

func (h *Handler) updateEndpointModel(w http.ResponseWriter, r *http.Request, endpointName string, modelID string) {
	existing, err := h.findEndpointModel(endpointName, modelID)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "Failed to load endpoint model")
		return
	}
	if existing == nil {
		WriteError(w, http.StatusNotFound, "Endpoint model not found")
		return
	}

	var req struct {
		DisplayName *string `json:"displayName"`
		Enabled     *bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.DisplayName != nil {
		existing.DisplayName = *req.DisplayName
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	if err := h.storage.UpsertEndpointModel(existing); err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	WriteSuccess(w, map[string]interface{}{"model": existing})
}

func (h *Handler) discoverEndpointModels(w http.ResponseWriter, endpointName string) {
	if h.proxy == nil {
		WriteError(w, http.StatusServiceUnavailable, "Proxy is not available")
		return
	}
	endpoint, err := h.getConfigEndpoint(endpointName)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "Failed to load endpoint")
		return
	}
	modelInfos, err := h.proxy.FetchModelsForEndpoint(endpoint)
	if err != nil {
		WriteError(w, http.StatusBadGateway, fmt.Sprintf("Failed to discover models: %v", err))
		return
	}

	existingModels, err := h.storage.GetEndpointModels(endpointName)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "Failed to load endpoint models")
		return
	}
	existingByID := make(map[string]storage.EndpointModel, len(existingModels))
	for _, model := range existingModels {
		existingByID[model.ModelID] = model
	}

	discovered := make([]storage.EndpointModel, 0, len(modelInfos))
	for _, info := range modelInfos {
		model := storage.EndpointModel{
			EndpointName:        endpointName,
			ModelID:             info.ID,
			DisplayName:         info.ID,
			Source:              storage.EndpointModelSourceDiscovered,
			Enabled:             false,
			VerificationStatus:  storage.EndpointModelStatusDiscovered,
			UpstreamTransformer: endpoint.Transformer,
		}
		if existing, ok := existingByID[info.ID]; ok {
			model = existing
			if strings.TrimSpace(model.DisplayName) == "" {
				model.DisplayName = info.ID
			}
			if strings.TrimSpace(model.UpstreamTransformer) == "" {
				model.UpstreamTransformer = endpoint.Transformer
			}
		}
		if err := h.storage.UpsertEndpointModel(&model); err != nil {
			WriteError(w, http.StatusInternalServerError, "Failed to save discovered model")
			return
		}
		discovered = append(discovered, model)
	}
	WriteSuccess(w, map[string]interface{}{"models": discovered})
}

func (h *Handler) verifyEndpointModel(w http.ResponseWriter, endpointName string, modelID string) {
	if strings.TrimSpace(modelID) == "" {
		WriteError(w, http.StatusBadRequest, "Model id required")
		return
	}
	if existing, err := h.findEndpointModel(endpointName, modelID); err != nil {
		WriteError(w, http.StatusInternalServerError, "Failed to load endpoint model")
		return
	} else if existing == nil {
		WriteError(w, http.StatusNotFound, "Endpoint model not found")
		return
	}
	if h.proxy == nil || !h.proxy.QueueModelVerification(endpointName, modelID) {
		WriteError(w, http.StatusServiceUnavailable, "Model verifier is not available")
		return
	}
	WriteSuccess(w, map[string]interface{}{"queued": true})
}

func (h *Handler) endpointExists(endpointName string) (bool, error) {
	endpoints, err := h.storage.GetEndpoints()
	if err != nil {
		return false, err
	}
	for _, endpoint := range endpoints {
		if endpoint.Name == endpointName {
			return true, nil
		}
	}
	return false, nil
}

func (h *Handler) findEndpointModel(endpointName string, modelID string) (*storage.EndpointModel, error) {
	models, err := h.storage.GetEndpointModels(endpointName)
	if err != nil {
		return nil, err
	}
	for i := range models {
		if models[i].ModelID == modelID {
			return &models[i], nil
		}
	}
	return nil, nil
}

func (h *Handler) getConfigEndpoint(endpointName string) (config.Endpoint, error) {
	endpoints, err := h.storage.GetEndpoints()
	if err != nil {
		return config.Endpoint{}, err
	}
	for _, endpoint := range endpoints {
		if endpoint.Name == endpointName {
			return config.Endpoint{
				Name:                    endpoint.Name,
				APIUrl:                  endpoint.APIUrl,
				APIKey:                  endpoint.APIKey,
				AuthMode:                endpoint.AuthMode,
				Enabled:                 endpoint.Enabled,
				Transformer:             endpoint.Transformer,
				Model:                   endpoint.Model,
				Thinking:                endpoint.Thinking,
				ForceStream:             endpoint.ForceStream,
				AutoSelect:              endpoint.AutoSelect,
				SupportsOpenAIResponses: endpoint.SupportsOpenAIResponses,
				SupportsOpenAIChat:      endpoint.SupportsOpenAIChat,
				SupportsClaudeMessages:  endpoint.SupportsClaudeMessages,
				PreferredClaudeUpstream: endpoint.PreferredClaudeUpstream,
				PreferredOpenAIUpstream: endpoint.PreferredOpenAIUpstream,
				Remark:                  endpoint.Remark,
			}, nil
		}
	}
	return config.Endpoint{}, fmt.Errorf("endpoint not found")
}

func parseEndpointModelPath(path string) (endpointModelRoute, bool) {
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[1] != "models" {
		return endpointModelRoute{}, false
	}
	endpointName, err := url.PathUnescape(parts[0])
	if err != nil || strings.TrimSpace(endpointName) == "" {
		return endpointModelRoute{}, false
	}
	route := endpointModelRoute{endpointName: endpointName}
	if len(parts) == 2 {
		return route, true
	}
	if len(parts) == 3 && parts[2] == "discover" {
		route.action = "discover"
		return route, true
	}
	if len(parts) >= 4 && parts[len(parts)-1] == "verify" {
		modelID, err := url.PathUnescape(strings.Join(parts[2:len(parts)-1], "/"))
		if err != nil || strings.TrimSpace(modelID) == "" {
			return endpointModelRoute{}, false
		}
		route.modelID = modelID
		route.action = "verify"
		return route, true
	}
	modelID, err := url.PathUnescape(strings.Join(parts[2:], "/"))
	if err != nil || strings.TrimSpace(modelID) == "" {
		return endpointModelRoute{}, false
	}
	route.modelID = modelID
	return route, true
}
