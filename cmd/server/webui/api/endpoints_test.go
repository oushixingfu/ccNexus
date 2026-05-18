package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/lich0821/ccNexus/internal/config"
	proxypkg "github.com/lich0821/ccNexus/internal/proxy"
	"github.com/lich0821/ccNexus/internal/storage"
)

type noopStatsStorage struct{}

func (noopStatsStorage) RecordDailyStat(stat interface{}) error { return nil }
func (noopStatsStorage) GetTotalStats() (int, map[string]interface{}, error) {
	return 0, map[string]interface{}{}, nil
}
func (noopStatsStorage) GetDailyStats(endpointName, startDate, endDate string) ([]interface{}, error) {
	return nil, nil
}
func (noopStatsStorage) GetPeriodStatsAggregated(startDate, endDate string) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

func TestRealtimeEventPayloadIncludesEndpointAvailability(t *testing.T) {
	store := newEndpointAPITestStorage(t)
	defer store.Close()

	if err := store.SaveEndpoint(&storage.Endpoint{
		Name:        "Primary",
		APIUrl:      "https://api.example.com",
		APIKey:      "sk-test",
		AuthMode:    config.AuthModeAPIKey,
		Enabled:     true,
		Transformer: "openai",
		Model:       "gpt-5.5",
	}); err != nil {
		t.Fatalf("save endpoint: %v", err)
	}
	failureAt := time.Now().UTC()
	failureReason := "quota_exhausted"
	failureStatus := http.StatusTooManyRequests
	if _, err := store.UpsertEndpointRuntimeStatus("Primary", storage.EndpointRuntimeStatusPatch{
		LastFailureAt:         &failureAt,
		LastFailureReason:     &failureReason,
		LastFailureStatusCode: &failureStatus,
	}); err != nil {
		t.Fatalf("seed runtime status: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.BasicAuthEnabled = false
	p := proxypkg.New(cfg, noopStatsStorage{}, store, "test-device")
	handler := NewHandler(cfg, p, store)

	payload, err := handler.buildRealtimeEventPayload(time.Unix(123, 0))
	if err != nil {
		t.Fatalf("build realtime event payload: %v", err)
	}
	endpoints, ok := payload["endpoints"].([]endpointResponse)
	if !ok || len(endpoints) != 1 {
		t.Fatalf("expected one endpoint response in realtime payload, got %#v", payload["endpoints"])
	}
	endpoint := endpoints[0]
	if endpoint.Available || endpoint.Availability != "unavailable" || endpoint.AvailabilityReason != failureReason || endpoint.AvailabilityStatusCode != failureStatus {
		t.Fatalf("expected realtime payload to match endpoint availability, got %#v", endpoint)
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
	if response.EffectiveClaudeUpstream != "openai2" {
		t.Fatalf("expected Claude Code effective upstream openai2, got %q", response.EffectiveClaudeUpstream)
	}
	if response.EffectiveOpenAIChatUpstream != "openai" {
		t.Fatalf("expected OpenAI Chat effective upstream openai, got %q", response.EffectiveOpenAIChatUpstream)
	}
	if response.EffectiveOpenAIResponsesUpstream != "openai2" {
		t.Fatalf("expected OpenAI Responses effective upstream openai2, got %q", response.EffectiveOpenAIResponsesUpstream)
	}
}

func TestUpdateEndpointAutoInfersProtocolFromModelAndIgnoresManualProtocolFields(t *testing.T) {
	probeHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	store := newEndpointAPITestStorage(t)
	defer store.Close()

	if err := store.SaveEndpoint(&storage.Endpoint{
		Name:               "manual",
		APIUrl:             upstream.URL,
		APIKey:             "sk-test",
		AuthMode:           config.AuthModeAPIKey,
		Enabled:            true,
		Transformer:        "openai",
		Model:              "gpt-5.5",
		AutoSelect:         true,
		SupportsOpenAIChat: true,
		SortOrder:          0,
	}); err != nil {
		t.Fatalf("save endpoint: %v", err)
	}

	cfg, err := config.LoadFromStorage(storage.NewConfigStorageAdapter(store))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.BasicAuthEnabled = false
	proxy := proxypkg.New(cfg, noopStatsStorage{}, store, "test-device")
	handler := NewHandler(cfg, proxy, store)

	body := []byte(`{
		"name":"manual",
		"apiUrl":"` + upstream.URL + `",
		"apiKey":"sk-test",
		"authMode":"api_key",
		"enabled":true,
		"transformer":"openai",
		"model":"gpt-5.5",
		"autoSelect":true,
		"supportsOpenAIResponses":false,
		"supportsOpenAIChat":true,
		"supportsClaudeMessages":false,
		"preferredClaudeUpstream":"openai",
		"preferredOpenAIUpstream":"openai",
		"remark":"protocol is inferred"
	}`)
	req := httptest.NewRequest(http.MethodPut, "/api/endpoints/manual", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	if probeHits != 0 {
		t.Fatalf("expected endpoint save not to probe upstream, got %d probe hits", probeHits)
	}

	endpoints, err := store.GetEndpoints()
	if err != nil {
		t.Fatalf("get endpoints: %v", err)
	}
	if len(endpoints) != 1 {
		t.Fatalf("expected one endpoint, got %d", len(endpoints))
	}
	updated := endpoints[0]
	if updated.Transformer != "openai2" {
		t.Fatalf("expected transformer openai2, got %q", updated.Transformer)
	}
	if !updated.AutoSelect || !updated.SupportsOpenAIResponses || updated.SupportsOpenAIChat || updated.SupportsClaudeMessages {
		t.Fatalf("expected inferred responses capability, got auto=%t responses=%t chat=%t claude=%t",
			updated.AutoSelect,
			updated.SupportsOpenAIResponses,
			updated.SupportsOpenAIChat,
			updated.SupportsClaudeMessages,
		)
	}
	if updated.PreferredClaudeUpstream != "" || updated.PreferredOpenAIUpstream != "" {
		t.Fatalf("expected preferred upstreams to stay automatic, got %q/%q", updated.PreferredClaudeUpstream, updated.PreferredOpenAIUpstream)
	}
}

func newEndpointAPITestStorage(t *testing.T) *storage.SQLiteStorage {
	t.Helper()

	store, err := storage.NewSQLiteStorage(filepath.Join(t.TempDir(), "ccnexus-test.db"))
	if err != nil {
		t.Fatalf("new sqlite storage: %v", err)
	}
	return store
}
