package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/endpointstate"
	"github.com/lich0821/ccNexus/internal/logger"
	"github.com/lich0821/ccNexus/internal/providercompat"
	proxypkg "github.com/lich0821/ccNexus/internal/proxy"
	"github.com/lich0821/ccNexus/internal/storage"
)

type endpointResponse struct {
	storage.Endpoint
	RuntimeStatus                    *storage.EndpointRuntimeStatus `json:"runtimeStatus,omitempty"`
	Available                        bool                           `json:"available"`
	Availability                     string                         `json:"availability"`
	AvailabilityReason               string                         `json:"availabilityReason,omitempty"`
	AvailabilityStatusCode           int                            `json:"availabilityStatusCode,omitempty"`
	EffectiveClaudeUpstream          string                         `json:"effectiveClaudeUpstream,omitempty"`
	EffectiveOpenAIChatUpstream      string                         `json:"effectiveOpenAIChatUpstream,omitempty"`
	EffectiveOpenAIResponsesUpstream string                         `json:"effectiveOpenAIResponsesUpstream,omitempty"`
}

// handleEndpoints handles GET (list) and POST (create) for endpoints
func (h *Handler) handleEndpoints(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.listEndpoints(w, r)
	case http.MethodPost:
		h.createEndpoint(w, r)
	default:
		WriteError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

// handleEndpointByName handles GET, PUT, DELETE, PATCH for specific endpoint
func (h *Handler) handleEndpointByName(w http.ResponseWriter, r *http.Request) {
	// Extract endpoint name from path
	path := strings.TrimPrefix(r.URL.Path, "/api/endpoints/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		WriteError(w, http.StatusBadRequest, "Endpoint name required")
		return
	}

	name := parts[0]

	// Handle /test and /toggle sub-paths
	if len(parts) > 1 {
		switch parts[1] {
		case "test":
			h.testEndpoint(w, r, name)
			return
		case "toggle":
			h.toggleEndpoint(w, r, name)
			return
		case "credentials":
			h.handleEndpointCredentials(w, r, name, parts[2:])
			return
		}
	}

	switch r.Method {
	case http.MethodGet:
		h.getEndpoint(w, r, name)
	case http.MethodPut:
		h.updateEndpoint(w, r, name)
	case http.MethodDelete:
		h.deleteEndpoint(w, r, name)
	default:
		WriteError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

// listEndpoints returns all endpoints
func (h *Handler) listEndpoints(w http.ResponseWriter, r *http.Request) {
	items, tokenPools, err := h.loadEndpointListPayload()
	if err != nil {
		logger.Error("Failed to get endpoints: %v", err)
		WriteError(w, http.StatusInternalServerError, "Failed to get endpoints")
		return
	}

	WriteSuccess(w, map[string]interface{}{
		"endpoints":  items,
		"tokenPools": tokenPools,
	})
}

func (h *Handler) loadEndpointListPayload() ([]endpointResponse, map[string]storage.TokenPoolStats, error) {
	endpoints, err := h.storage.GetEndpoints()
	if err != nil {
		return nil, nil, err
	}

	runtimeStatuses, err := h.storage.GetEndpointRuntimeStatuses()
	if err != nil {
		logger.Warn("Failed to get endpoint runtime statuses: %v", err)
		runtimeStatuses = map[string]*storage.EndpointRuntimeStatus{}
	}

	items := make([]endpointResponse, 0, len(endpoints))
	for i := range endpoints {
		endpoints[i].APIKey = maskAPIKey(endpoints[i].APIKey)
		items = append(items, buildEndpointResponse(endpoints[i], runtimeStatuses[endpoints[i].Name]))
	}

	tokenPools, err := h.storage.GetAllTokenPoolStats()
	if err != nil {
		logger.Warn("Failed to get token pool stats: %v", err)
		tokenPools = map[string]storage.TokenPoolStats{}
	}

	return items, tokenPools, nil
}

// getEndpoint returns a specific endpoint
func (h *Handler) getEndpoint(w http.ResponseWriter, r *http.Request, name string) {
	endpoints, err := h.storage.GetEndpoints()
	if err != nil {
		logger.Error("Failed to get endpoints: %v", err)
		WriteError(w, http.StatusInternalServerError, "Failed to get endpoints")
		return
	}

	for _, ep := range endpoints {
		if ep.Name == name {
			runtimeStatuses, err := h.storage.GetEndpointRuntimeStatuses()
			if err != nil {
				logger.Warn("Failed to get endpoint runtime statuses: %v", err)
				runtimeStatuses = map[string]*storage.EndpointRuntimeStatus{}
			}
			ep.APIKey = maskAPIKey(ep.APIKey)
			WriteSuccess(w, buildEndpointResponse(ep, runtimeStatuses[ep.Name]))
			return
		}
	}

	WriteError(w, http.StatusNotFound, "Endpoint not found")
}

func buildEndpointResponse(endpoint storage.Endpoint, status *storage.EndpointRuntimeStatus) endpointResponse {
	state := endpointstate.Derive(endpoint.Enabled, status)
	effective := effectiveEndpointUpstreams(endpoint)
	return endpointResponse{
		Endpoint:                         endpoint,
		RuntimeStatus:                    status,
		Available:                        state.Available,
		Availability:                     state.Availability,
		AvailabilityReason:               state.Reason,
		AvailabilityStatusCode:           state.StatusCode,
		EffectiveClaudeUpstream:          effective["claude"],
		EffectiveOpenAIChatUpstream:      effective["openai_chat"],
		EffectiveOpenAIResponsesUpstream: effective["openai_responses"],
	}
}

func effectiveEndpointUpstreams(endpoint storage.Endpoint) map[string]string {
	configEndpoint := configEndpointFromStorage(endpoint)
	return map[string]string{
		"claude":           proxypkg.EffectiveUpstreamTransformerForClientFormat(proxypkg.ClientFormatClaude, configEndpoint),
		"openai_chat":      proxypkg.EffectiveUpstreamTransformerForClientFormat(proxypkg.ClientFormatOpenAIChat, configEndpoint),
		"openai_responses": proxypkg.EffectiveUpstreamTransformerForClientFormat(proxypkg.ClientFormatOpenAIResponses, configEndpoint),
	}
}

func configEndpointFromStorage(endpoint storage.Endpoint) config.Endpoint {
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
	}
}

// createEndpoint creates a new endpoint
func (h *Handler) createEndpoint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name                    string `json:"name"`
		APIUrl                  string `json:"apiUrl"`
		APIKey                  string `json:"apiKey"`
		AuthMode                string `json:"authMode"`
		Enabled                 bool   `json:"enabled"`
		Transformer             string `json:"transformer"`
		Model                   string `json:"model"`
		Thinking                string `json:"thinking"`
		ForceStream             *bool  `json:"forceStream"`
		AutoSelect              *bool  `json:"autoSelect"`
		SupportsOpenAIResponses *bool  `json:"supportsOpenAIResponses"`
		SupportsOpenAIChat      *bool  `json:"supportsOpenAIChat"`
		SupportsClaudeMessages  *bool  `json:"supportsClaudeMessages"`
		PreferredClaudeUpstream string `json:"preferredClaudeUpstream"`
		PreferredOpenAIUpstream string `json:"preferredOpenAIUpstream"`
		Remark                  string `json:"remark"`
		CloneFrom               string `json:"cloneFrom"` // Clone from existing endpoint name
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	// If cloning, get API key from source endpoint
	if req.CloneFrom != "" && req.APIKey == "" {
		endpoints, err := h.storage.GetEndpoints()
		if err == nil {
			for _, ep := range endpoints {
				if ep.Name == req.CloneFrom {
					req.APIKey = ep.APIKey
					if req.Thinking == "" {
						req.Thinking = ep.Thinking
					}
					if req.ForceStream == nil {
						forceStream := ep.ForceStream
						req.ForceStream = &forceStream
					}
					break
				}
			}
		}
	}

	authMode := config.NormalizeAuthMode(req.AuthMode)
	forceStream := false
	if req.ForceStream != nil {
		forceStream = *req.ForceStream
	}
	normalizedEndpoint := config.Endpoint{
		APIUrl:      normalizeAPIUrl(req.APIUrl),
		APIKey:      req.APIKey,
		AuthMode:    authMode,
		Transformer: "auto",
		Model:       req.Model,
		Thinking:    req.Thinking,
		ForceStream: forceStream,
		Remark:      req.Remark,
	}
	autoConfigureEndpointForSave(&normalizedEndpoint)
	authMode = normalizedEndpoint.AuthMode
	req.APIUrl = normalizedEndpoint.APIUrl
	req.APIKey = normalizedEndpoint.APIKey
	req.Transformer = normalizedEndpoint.Transformer
	req.Thinking = normalizedEndpoint.Thinking
	forceStream = normalizedEndpoint.ForceStream
	autoSelect := normalizedEndpoint.AutoSelect
	supportsOpenAIResponses := normalizedEndpoint.SupportsOpenAIResponses
	supportsOpenAIChat := normalizedEndpoint.SupportsOpenAIChat
	supportsClaudeMessages := normalizedEndpoint.SupportsClaudeMessages
	req.PreferredClaudeUpstream = normalizedEndpoint.PreferredClaudeUpstream
	req.PreferredOpenAIUpstream = normalizedEndpoint.PreferredOpenAIUpstream

	// Validate required fields
	if req.Name == "" || req.APIUrl == "" {
		WriteError(w, http.StatusBadRequest, "Name and apiUrl are required")
		return
	}
	if authMode == config.AuthModeAPIKey && req.APIKey == "" {
		WriteError(w, http.StatusBadRequest, "apiKey is required in api_key mode")
		return
	}
	if config.IsTokenPoolAuthMode(authMode) {
		req.APIKey = ""
	}

	// Get current endpoints to determine sort order
	endpoints, err := h.storage.GetEndpoints()
	if err != nil {
		logger.Error("Failed to get endpoints: %v", err)
		WriteError(w, http.StatusInternalServerError, "Failed to get endpoints")
		return
	}

	// Check if endpoint with same name exists
	for _, ep := range endpoints {
		if ep.Name == req.Name {
			WriteError(w, http.StatusConflict, "Endpoint with this name already exists")
			return
		}
	}

	// Create new endpoint
	endpoint := &storage.Endpoint{
		Name:                    req.Name,
		APIUrl:                  normalizeAPIUrl(req.APIUrl),
		APIKey:                  req.APIKey,
		AuthMode:                authMode,
		Enabled:                 req.Enabled,
		Transformer:             req.Transformer,
		Model:                   req.Model,
		Thinking:                req.Thinking,
		ForceStream:             forceStream,
		AutoSelect:              autoSelect,
		SupportsOpenAIResponses: supportsOpenAIResponses,
		SupportsOpenAIChat:      supportsOpenAIChat,
		SupportsClaudeMessages:  supportsClaudeMessages,
		PreferredClaudeUpstream: req.PreferredClaudeUpstream,
		PreferredOpenAIUpstream: req.PreferredOpenAIUpstream,
		Remark:                  req.Remark,
		SortOrder:               len(endpoints),
		CreatedAt:               time.Now(),
		UpdatedAt:               time.Now(),
	}

	if err := h.storage.SaveEndpoint(endpoint); err != nil {
		logger.Error("Failed to save endpoint: %v", err)
		WriteError(w, http.StatusInternalServerError, "Failed to save endpoint")
		return
	}

	// Update proxy config
	if err := h.reloadConfig(); err != nil {
		logger.Error("Failed to reload config: %v", err)
	}

	endpoint.APIKey = maskAPIKey(endpoint.APIKey)
	WriteSuccess(w, endpoint)
}

// updateEndpoint updates an existing endpoint
func (h *Handler) updateEndpoint(w http.ResponseWriter, r *http.Request, name string) {
	var req struct {
		Name                    string `json:"name"`
		APIUrl                  string `json:"apiUrl"`
		APIKey                  string `json:"apiKey"`
		AuthMode                string `json:"authMode"`
		Enabled                 bool   `json:"enabled"`
		Transformer             string `json:"transformer"`
		Model                   string `json:"model"`
		Thinking                string `json:"thinking"`
		ForceStream             *bool  `json:"forceStream"`
		AutoSelect              *bool  `json:"autoSelect"`
		SupportsOpenAIResponses *bool  `json:"supportsOpenAIResponses"`
		SupportsOpenAIChat      *bool  `json:"supportsOpenAIChat"`
		SupportsClaudeMessages  *bool  `json:"supportsClaudeMessages"`
		PreferredClaudeUpstream string `json:"preferredClaudeUpstream"`
		PreferredOpenAIUpstream string `json:"preferredOpenAIUpstream"`
		Remark                  string `json:"remark"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	// Get existing endpoint
	endpoints, err := h.storage.GetEndpoints()
	if err != nil {
		logger.Error("Failed to get endpoints: %v", err)
		WriteError(w, http.StatusInternalServerError, "Failed to get endpoints")
		return
	}

	var existing *storage.Endpoint
	existingIndex := -1
	for i := range endpoints {
		if endpoints[i].Name == name {
			existing = &endpoints[i]
			existingIndex = i
			break
		}
	}

	if existing == nil {
		WriteError(w, http.StatusNotFound, "Endpoint not found")
		return
	}

	// Update fields
	if strings.TrimSpace(req.Name) != "" {
		existing.Name = strings.TrimSpace(req.Name)
	} else if req.Name != "" {
		WriteError(w, http.StatusBadRequest, "Name is required")
		return
	}
	if req.APIUrl != "" {
		existing.APIUrl = normalizeAPIUrl(req.APIUrl)
	}
	if req.APIKey != "" {
		existing.APIKey = req.APIKey
	}
	if req.AuthMode != "" {
		existing.AuthMode = config.NormalizeAuthMode(req.AuthMode)
	}
	if existing.AuthMode == "" {
		existing.AuthMode = config.AuthModeAPIKey
	}
	normalizedEndpoint := config.Endpoint{
		Name:        existing.Name,
		APIUrl:      existing.APIUrl,
		APIKey:      existing.APIKey,
		AuthMode:    existing.AuthMode,
		Enabled:     existing.Enabled,
		Transformer: "auto",
		Model:       existing.Model,
		Thinking:    existing.Thinking,
		ForceStream: existing.ForceStream,
		Remark:      existing.Remark,
	}
	if req.ForceStream != nil {
		normalizedEndpoint.ForceStream = *req.ForceStream
	}
	if req.Model != "" {
		normalizedEndpoint.Model = req.Model
	}
	if req.Thinking != "" {
		normalizedEndpoint.Thinking = req.Thinking
	}
	autoConfigureEndpointForSave(&normalizedEndpoint)
	existing.APIUrl = normalizedEndpoint.APIUrl
	existing.APIKey = normalizedEndpoint.APIKey
	existing.AuthMode = normalizedEndpoint.AuthMode
	existing.Transformer = normalizedEndpoint.Transformer
	existing.Thinking = normalizedEndpoint.Thinking
	existing.ForceStream = normalizedEndpoint.ForceStream
	existing.AutoSelect = normalizedEndpoint.AutoSelect
	existing.SupportsOpenAIResponses = normalizedEndpoint.SupportsOpenAIResponses
	existing.SupportsOpenAIChat = normalizedEndpoint.SupportsOpenAIChat
	existing.SupportsClaudeMessages = normalizedEndpoint.SupportsClaudeMessages
	existing.PreferredClaudeUpstream = normalizedEndpoint.PreferredClaudeUpstream
	existing.PreferredOpenAIUpstream = normalizedEndpoint.PreferredOpenAIUpstream
	if existing.AuthMode == config.AuthModeAPIKey && existing.APIKey == "" {
		WriteError(w, http.StatusBadRequest, "apiKey is required in api_key mode")
		return
	}
	existing.Enabled = req.Enabled
	existing.Transformer = normalizedEndpoint.Transformer
	if req.Model != "" {
		existing.Model = req.Model
	}
	if req.Thinking != "" {
		existing.Thinking = config.NormalizeThinkingEffort(req.Thinking)
	}
	if req.ForceStream != nil {
		existing.ForceStream = *req.ForceStream
	}
	existing.Remark = req.Remark
	existing.UpdatedAt = time.Now()

	if existing.Name != name {
		for i, ep := range endpoints {
			if i != existingIndex && ep.Name == existing.Name {
				WriteError(w, http.StatusConflict, "Endpoint with this name already exists")
				return
			}
		}
	}

	if err := h.storage.UpdateEndpointByName(name, existing); err != nil {
		logger.Error("Failed to update endpoint: %v", err)
		WriteError(w, http.StatusInternalServerError, "Failed to update endpoint")
		return
	}

	// Update proxy config
	if err := h.reloadConfig(); err != nil {
		logger.Error("Failed to reload config: %v", err)
	}

	existing.APIKey = maskAPIKey(existing.APIKey)
	WriteSuccess(w, existing)
}

// deleteEndpoint deletes an endpoint
func (h *Handler) deleteEndpoint(w http.ResponseWriter, r *http.Request, name string) {
	if err := h.storage.DeleteEndpoint(name); err != nil {
		logger.Error("Failed to delete endpoint: %v", err)
		WriteError(w, http.StatusInternalServerError, "Failed to delete endpoint")
		return
	}

	// Update proxy config
	if err := h.reloadConfig(); err != nil {
		logger.Error("Failed to reload config: %v", err)
	}

	WriteSuccess(w, map[string]interface{}{
		"message": "Endpoint deleted successfully",
	})
}

// toggleEndpoint enables or disables an endpoint
func (h *Handler) toggleEndpoint(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPatch && r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req struct {
		Enabled bool `json:"enabled"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	// Get existing endpoint
	endpoints, err := h.storage.GetEndpoints()
	if err != nil {
		logger.Error("Failed to get endpoints: %v", err)
		WriteError(w, http.StatusInternalServerError, "Failed to get endpoints")
		return
	}

	var existing *storage.Endpoint
	for i := range endpoints {
		if endpoints[i].Name == name {
			existing = &endpoints[i]
			break
		}
	}

	if existing == nil {
		WriteError(w, http.StatusNotFound, "Endpoint not found")
		return
	}

	existing.Enabled = req.Enabled
	existing.UpdatedAt = time.Now()

	if err := h.storage.UpdateEndpoint(existing); err != nil {
		logger.Error("Failed to update endpoint: %v", err)
		WriteError(w, http.StatusInternalServerError, "Failed to update endpoint")
		return
	}

	// Update proxy config
	if err := h.reloadConfig(); err != nil {
		logger.Error("Failed to reload config: %v", err)
	}

	WriteSuccess(w, map[string]interface{}{
		"enabled": existing.Enabled,
	})
}

// handleCurrentEndpoint returns the current active endpoint
func (h *Handler) handleCurrentEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	currentEndpoint := h.proxy.GetCurrentEndpointName()
	if currentEndpoint == "" {
		WriteError(w, http.StatusNotFound, "No endpoints configured")
		return
	}

	WriteSuccess(w, map[string]interface{}{
		"name": currentEndpoint,
	})
}

// handleSwitchEndpoint switches to a specific endpoint
func (h *Handler) handleSwitchEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req struct {
		Name string `json:"name"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		WriteError(w, http.StatusBadRequest, "Endpoint name required")
		return
	}

	if err := h.proxy.SetCurrentEndpoint(req.Name); err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "not enabled") || strings.Contains(err.Error(), "no enabled endpoints") {
			status = http.StatusNotFound
		}
		WriteError(w, status, err.Error())
		return
	}

	WriteSuccess(w, map[string]interface{}{
		"message": "Endpoint switched successfully",
		"name":    req.Name,
	})
}

// handleReorderEndpoints reorders endpoints
func (h *Handler) handleReorderEndpoints(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req struct {
		Names []string `json:"names"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	// Get all endpoints
	endpoints, err := h.storage.GetEndpoints()
	if err != nil {
		logger.Error("Failed to get endpoints: %v", err)
		WriteError(w, http.StatusInternalServerError, "Failed to get endpoints")
		return
	}

	// Create a map for quick lookup
	endpointMap := make(map[string]*storage.Endpoint)
	for i := range endpoints {
		endpointMap[endpoints[i].Name] = &endpoints[i]
	}

	// Update sort order
	for i, name := range req.Names {
		if ep, ok := endpointMap[name]; ok {
			ep.SortOrder = i
			ep.UpdatedAt = time.Now()
			if err := h.storage.UpdateEndpoint(ep); err != nil {
				logger.Error("Failed to update endpoint sort order: %v", err)
			}
		}
	}

	// Update proxy config
	if err := h.reloadConfig(); err != nil {
		logger.Error("Failed to reload config: %v", err)
	}

	WriteSuccess(w, map[string]interface{}{
		"message": "Endpoints reordered successfully",
	})
}

// reloadConfig reloads the configuration from storage and updates the proxy
func (h *Handler) reloadConfig() error {
	adapter := storage.NewConfigStorageAdapter(h.storage)
	cfg, err := config.LoadFromStorage(adapter)
	if err != nil {
		return err
	}

	h.config = cfg
	return h.proxy.UpdateConfig(cfg)
}

// maskAPIKey masks an API key, showing only the last 4 characters
func maskAPIKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 4 {
		return "****"
	}
	return "****" + key[len(key)-4:]
}

// normalizeAPIUrl ensures the API URL has the correct format
func normalizeAPIUrl(apiUrl string) string {
	return strings.TrimSuffix(apiUrl, "/")
}

func autoConfigureEndpointForSave(endpoint *config.Endpoint) {
	if endpoint == nil {
		return
	}

	if !providercompat.IsAutoTransformer(endpoint.Transformer) {
		endpoint.Transformer = "auto"
	}
	config.ApplyEndpointAuthModeRules(endpoint)
	endpoint.AutoSelect = true
	endpoint.SupportsOpenAIResponses = false
	endpoint.SupportsOpenAIChat = false
	endpoint.SupportsClaudeMessages = false
	endpoint.PreferredClaudeUpstream = ""
	endpoint.PreferredOpenAIUpstream = ""

	switch providercompat.NormalizeTransformer(endpoint.Transformer) {
	case providercompat.TransformerOpenAI2:
		endpoint.SupportsOpenAIResponses = true
	case providercompat.TransformerOpenAI, providercompat.TransformerDeepSeek, providercompat.TransformerKimi:
		endpoint.SupportsOpenAIChat = true
	case providercompat.TransformerClaude:
		endpoint.SupportsClaudeMessages = true
	}
}
