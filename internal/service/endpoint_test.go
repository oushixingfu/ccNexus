package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/proxy"
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

func TestEndpointServiceManualTestFailureUsesProxyFailoverState(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"quota exhausted"}}`))
	}))
	defer upstream.Close()

	store := newEndpointServiceTestStorage(t)
	defer store.Close()
	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints([]config.Endpoint{
		endpointServiceTestEndpoint("Primary", upstream.URL),
		endpointServiceTestEndpoint("Fallback", "https://fallback.example.com"),
	})
	p := proxy.New(cfg, noopStatsStorage{}, store, "test-device")
	service := NewEndpointService(cfg, p, store)

	result := decodeEndpointServiceTestResult(t, service.TestEndpoint(0))
	if result["success"] != false {
		t.Fatalf("expected failed endpoint test result, got %#v", result)
	}
	if current := p.GetCurrentEndpointName(); current != "Fallback" {
		t.Fatalf("expected proxy current endpoint to fail over to Fallback, got %q", current)
	}

	statuses, err := store.GetEndpointRuntimeStatuses()
	if err != nil {
		t.Fatalf("get runtime statuses: %v", err)
	}
	status := statuses["Primary"]
	if status == nil || status.LastFailureReason != "quota_exhausted" || status.LastFailureStatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected classified persisted runtime failure, got %#v", status)
	}
}

func TestEndpointServiceManualTestSuccessClearsStaleFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	store := newEndpointServiceTestStorage(t)
	defer store.Close()
	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints([]config.Endpoint{endpointServiceTestEndpoint("Primary", upstream.URL)})
	p := proxy.New(cfg, noopStatsStorage{}, store, "test-device")
	service := NewEndpointService(cfg, p, store)

	failureAt := time.Now().Add(-time.Minute).UTC()
	reason := "upstream_5xx"
	statusCode := http.StatusBadGateway
	if _, err := store.UpsertEndpointRuntimeStatus("Primary", storage.EndpointRuntimeStatusPatch{
		LastFailureAt:         &failureAt,
		LastFailureReason:     &reason,
		LastFailureStatusCode: &statusCode,
	}); err != nil {
		t.Fatalf("seed runtime failure: %v", err)
	}

	result := decodeEndpointServiceTestResult(t, service.TestEndpoint(0))
	if result["success"] != true {
		t.Fatalf("expected successful endpoint test result, got %#v", result)
	}

	statuses, err := store.GetEndpointRuntimeStatuses()
	if err != nil {
		t.Fatalf("get runtime statuses: %v", err)
	}
	status := statuses["Primary"]
	if status == nil || status.LastSuccessAt == nil || !status.LastSuccessAt.After(failureAt) {
		t.Fatalf("expected later success status, got %#v", status)
	}
	if status.LastFailureReason != "" || status.LastFailureStatusCode != 0 {
		t.Fatalf("expected stale failure details cleared, got reason=%q status=%d", status.LastFailureReason, status.LastFailureStatusCode)
	}
}

func TestEndpointServiceTestFailureReasonClassifiesUnauthorized(t *testing.T) {
	if got := endpointServiceTestFailureReason("", "API returned status 401: invalid api key"); got != "endpoint_auth_failed" {
		t.Fatalf("expected endpoint_auth_failed, got %q", got)
	}
}

func endpointServiceTestEndpoint(name, apiURL string) config.Endpoint {
	return config.Endpoint{
		Name:               name,
		APIUrl:             apiURL,
		APIKey:             "sk-test",
		AuthMode:           config.AuthModeAPIKey,
		Enabled:            true,
		Transformer:        "openai",
		Model:              "gpt-5.5",
		AutoSelect:         true,
		SupportsOpenAIChat: true,
	}
}

func newEndpointServiceTestStorage(t *testing.T) *storage.SQLiteStorage {
	t.Helper()

	store, err := storage.NewSQLiteStorage(filepath.Join(t.TempDir(), "ccnexus-test.db"))
	if err != nil {
		t.Fatalf("new sqlite storage: %v", err)
	}
	return store
}

func decodeEndpointServiceTestResult(t *testing.T, raw string) map[string]interface{} {
	t.Helper()

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("decode endpoint service test result: %v body=%q", err, raw)
	}
	return result
}
