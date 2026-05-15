package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/storage"
)

func TestBuildHealthCheckRequestOpenAI2UsesStream(t *testing.T) {
	cfg := config.DefaultConfig()
	p := New(cfg, &noopStatsStorage{}, nil, "test-device")

	endpoint := &config.Endpoint{
		Name:        "gpt",
		APIUrl:      "https://1052.cc.cd:5005",
		Transformer: "openai2",
		Model:       "gpt-5.5",
		Thinking:    "xhigh",
		Enabled:     true,
	}

	bodyBytes, targetURL, err := p.buildHealthCheckRequest(endpoint)
	if err != nil {
		t.Fatalf("buildHealthCheckRequest failed: %v", err)
	}
	if targetURL != "https://1052.cc.cd:5005/v1/responses" {
		t.Fatalf("unexpected target URL: %s", targetURL)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatalf("unmarshal request body failed: %v", err)
	}

	if body["stream"] != true {
		t.Fatalf("expected stream=true for OpenAI2 health check, got %#v", body["stream"])
	}
	if body["effortLevel"] != "xhigh" {
		t.Fatalf("expected effortLevel=xhigh for custom gateway, got %#v", body["effortLevel"])
	}
	if _, ok := body["reasoning"]; ok {
		t.Fatalf("did not expect reasoning for custom gateway health check, got %#v", body["reasoning"])
	}
}

func TestBuildHealthCheckRequestOpenAIForceStreamUsesStream(t *testing.T) {
	cfg := config.DefaultConfig()
	p := New(cfg, &noopStatsStorage{}, nil, "test-device")

	endpoint := &config.Endpoint{
		Name:        "gpt",
		APIUrl:      "https://api.openai.com",
		APIKey:      "test-key",
		AuthMode:    config.AuthModeAPIKey,
		Transformer: "openai",
		Model:       "gpt-5.5",
		ForceStream: true,
		Enabled:     true,
	}

	bodyBytes, _, err := p.buildHealthCheckRequest(endpoint)
	if err != nil {
		t.Fatalf("buildHealthCheckRequest failed: %v", err)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatalf("unmarshal request body failed: %v", err)
	}
	if body["stream"] != true {
		t.Fatalf("expected force-stream health check to use stream=true, got %#v", body["stream"])
	}
	streamOptions, ok := body["stream_options"].(map[string]interface{})
	if !ok || streamOptions["include_usage"] != true {
		t.Fatalf("expected stream_options.include_usage=true, got %#v", body["stream_options"])
	}
}

func TestBuildHealthCheckRequestKimiUsesSemanticProbeBudget(t *testing.T) {
	cfg := config.DefaultConfig()
	p := New(cfg, &noopStatsStorage{}, nil, "test-device")

	endpoint := &config.Endpoint{
		Name:        "kimi",
		APIUrl:      "https://1052.cc.cd:5005",
		APIKey:      "test-key",
		AuthMode:    config.AuthModeAPIKey,
		Transformer: "kimi",
		Model:       "kimi-k2.6",
		Enabled:     true,
	}

	bodyBytes, targetURL, err := p.buildHealthCheckRequest(endpoint)
	if err != nil {
		t.Fatalf("buildHealthCheckRequest failed: %v", err)
	}
	if targetURL != "https://1052.cc.cd:5005/v1/chat/completions" {
		t.Fatalf("unexpected target URL: %s", targetURL)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatalf("unmarshal request body failed: %v", err)
	}
	if body["max_tokens"] != float64(healthCheckMaxTokens) {
		t.Fatalf("expected max_tokens=%d, got %#v", healthCheckMaxTokens, body["max_tokens"])
	}
	messages, ok := body["messages"].([]interface{})
	if !ok || len(messages) != 1 {
		t.Fatalf("expected one message, got %#v", body["messages"])
	}
	first, ok := messages[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected message object, got %#v", messages[0])
	}
	if first["content"] != healthCheckPrompt {
		t.Fatalf("expected semantic health prompt, got %#v", first["content"])
	}
}

func TestHealthCheckRecoveryDoesNotAutoSwitchWhenDeprioritize(t *testing.T) {
	recovered := newHealthyResponsesStreamServer(t)
	defer recovered.Close()

	p := newFailoverPolicyTestProxy([]config.Endpoint{
		failoverPolicyTestEndpoint("A", recovered.URL),
		failoverPolicyTestEndpoint("B", "https://b.example"),
	}, recovered.Client())
	if err := p.SetCurrentEndpoint("B"); err != nil {
		t.Fatalf("set current endpoint: %v", err)
	}

	p.registerForHealthCheck("A")
	p.runHealthCheckRound()

	if got := p.GetCurrentEndpointName(); got != "B" {
		t.Fatalf("expected deprioritize policy to keep current endpoint B, got %q", got)
	}
	p.healthCheckWatchedMu.RLock()
	_, watched := p.healthCheckWatched["A"]
	p.healthCheckWatchedMu.RUnlock()
	if watched {
		t.Fatal("expected recovered endpoint to be removed from health check watch set")
	}
}

func TestHealthCheckRecoveryAutoSwitchesOnlyWhenAutoReturn(t *testing.T) {
	recovered := newHealthyResponsesStreamServer(t)
	defer recovered.Close()

	p := newFailoverPolicyTestProxy([]config.Endpoint{
		failoverPolicyTestEndpoint("A", recovered.URL),
		failoverPolicyTestEndpoint("B", "https://b.example"),
	}, recovered.Client())
	p.config.UpdateFailover(&config.FailoverConfig{RecoveredEndpointPolicy: config.RecoveredEndpointPolicyAutoReturn})
	if err := p.SetCurrentEndpoint("B"); err != nil {
		t.Fatalf("set current endpoint: %v", err)
	}

	p.registerForHealthCheck("A")
	p.runHealthCheckRound()

	if got := p.GetCurrentEndpointName(); got != "A" {
		t.Fatalf("expected auto_return policy to switch current endpoint back to A, got %q", got)
	}
}

func TestManualSwitchToLowerPriorityEndpointWatchesPreferredAutoReturnEndpoint(t *testing.T) {
	recovered := newHealthyResponsesStreamServer(t)
	defer recovered.Close()

	p := newFailoverPolicyTestProxy([]config.Endpoint{
		failoverPolicyTestEndpoint("A", recovered.URL),
		failoverPolicyTestEndpoint("B", "https://b.example"),
	}, recovered.Client())
	p.config.UpdateFailover(&config.FailoverConfig{RecoveredEndpointPolicy: config.RecoveredEndpointPolicyAutoReturn})

	if err := p.SetCurrentEndpoint("B"); err != nil {
		t.Fatalf("set current endpoint: %v", err)
	}
	p.healthCheckWatchedMu.RLock()
	_, watched := p.healthCheckWatched["A"]
	p.healthCheckWatchedMu.RUnlock()
	if !watched {
		t.Fatal("expected preferred endpoint A to be watched after switching to lower-priority B")
	}

	p.runHealthCheckRound()
	if got := p.GetCurrentEndpointName(); got != "A" {
		t.Fatalf("expected auto_return health check to switch current endpoint back to A, got %q", got)
	}
}

func TestHealthCheckDoesNotClearActiveQuotaCooldown(t *testing.T) {
	recovered := newHealthyResponsesStreamServer(t)
	defer recovered.Close()

	p := newFailoverPolicyTestProxy([]config.Endpoint{
		failoverPolicyTestEndpoint("A", recovered.URL),
		failoverPolicyTestEndpoint("B", "https://b.example"),
	}, recovered.Client())
	p.config.UpdateFailover(&config.FailoverConfig{RecoveredEndpointPolicy: config.RecoveredEndpointPolicyAutoReturn})
	if err := p.SetCurrentEndpoint("B"); err != nil {
		t.Fatalf("set current endpoint: %v", err)
	}

	p.markEndpointCooldown("A", "quota_exhausted", time.Hour, requestObservability{RequestID: "quota-cooldown"}, 1)
	p.registerForHealthCheck("A")
	p.runHealthCheckRound()

	if got := p.GetCurrentEndpointName(); got != "B" {
		t.Fatalf("expected active quota cooldown to keep current endpoint B, got %q", got)
	}
	cooldown, ok := p.endpointCooldown("A")
	if !ok || !cooldown.Until.After(time.Now()) || cooldown.Reason != "quota_exhausted" {
		t.Fatalf("expected quota cooldown to remain active, got cooldown=%#v ok=%t", cooldown, ok)
	}
	p.healthCheckWatchedMu.RLock()
	_, watched := p.healthCheckWatched["A"]
	p.healthCheckWatchedMu.RUnlock()
	if !watched {
		t.Fatal("expected quota-cooled endpoint to stay watched for later recovery")
	}
}

func TestSeedHealthCheckRestoresQuotaCooldownAndSwitchesAway(t *testing.T) {
	recovered := newHealthyResponsesStreamServer(t)
	defer recovered.Close()

	store, err := storage.NewSQLiteStorage(filepath.Join(t.TempDir(), "ccnexus.db"))
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	defer store.Close()

	cfg := config.DefaultConfig()
	cfg.UpdateFailover(&config.FailoverConfig{RecoveredEndpointPolicy: config.RecoveredEndpointPolicyAutoReturn})
	cfg.UpdateEndpoints([]config.Endpoint{
		failoverPolicyTestEndpoint("A", recovered.URL),
		failoverPolicyTestEndpoint("B", "https://b.example"),
	})
	p := New(cfg, &noopStatsStorage{}, store, "test-device")
	p.httpClient = recovered.Client()

	failureAt := time.Now().UTC()
	reason := "quota_exhausted"
	if _, err := store.UpsertEndpointRuntimeStatus("A", storage.EndpointRuntimeStatusPatch{
		LastFailureAt:     &failureAt,
		LastFailureReason: &reason,
	}); err != nil {
		t.Fatalf("seed runtime status: %v", err)
	}

	p.seedHealthCheckWatchSet()

	if got := p.GetCurrentEndpointName(); got != "B" {
		t.Fatalf("expected restored quota cooldown to switch current endpoint away from A, got %q", got)
	}
	cooldown, ok := p.endpointCooldown("A")
	if !ok || !cooldown.Until.After(time.Now()) || cooldown.Reason != "quota_exhausted" {
		t.Fatalf("expected restored active quota cooldown, got cooldown=%#v ok=%t", cooldown, ok)
	}

	p.runHealthCheckRound()
	if got := p.GetCurrentEndpointName(); got != "B" {
		t.Fatalf("expected active restored quota cooldown to prevent auto-return to A, got %q", got)
	}
}

func TestSeedHealthCheckRestoresQuotaCooldownDespiteLaterSuccess(t *testing.T) {
	recovered := newHealthyResponsesStreamServer(t)
	defer recovered.Close()

	store, err := storage.NewSQLiteStorage(filepath.Join(t.TempDir(), "ccnexus.db"))
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	defer store.Close()

	cfg := config.DefaultConfig()
	cfg.UpdateFailover(&config.FailoverConfig{RecoveredEndpointPolicy: config.RecoveredEndpointPolicyAutoReturn})
	cfg.UpdateEndpoints([]config.Endpoint{
		failoverPolicyTestEndpoint("A", recovered.URL),
		failoverPolicyTestEndpoint("B", "https://b.example"),
	})
	p := New(cfg, &noopStatsStorage{}, store, "test-device")
	p.httpClient = recovered.Client()

	failureAt := time.Now().Add(-time.Second).UTC()
	successAt := failureAt.Add(500 * time.Millisecond)
	reason := "quota_exhausted"
	if _, err := store.UpsertEndpointRuntimeStatus("A", storage.EndpointRuntimeStatusPatch{
		LastFailureAt:     &failureAt,
		LastFailureReason: &reason,
		LastSuccessAt:     &successAt,
	}); err != nil {
		t.Fatalf("seed runtime status: %v", err)
	}

	p.seedHealthCheckWatchSet()

	if got := p.GetCurrentEndpointName(); got != "B" {
		t.Fatalf("expected active quota cooldown to override later health success and switch away from A, got %q", got)
	}
	if _, ok := p.endpointCooldown("A"); !ok {
		t.Fatal("expected active quota cooldown to be restored")
	}
}

func TestSeedHealthCheckBlocksExpiredQuotaFailureFromRequestPlan(t *testing.T) {
	recovered := newHealthyResponsesStreamServer(t)
	defer recovered.Close()

	store, err := storage.NewSQLiteStorage(filepath.Join(t.TempDir(), "ccnexus.db"))
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	defer store.Close()

	cfg := config.DefaultConfig()
	cfg.UpdateFailover(&config.FailoverConfig{RecoveredEndpointPolicy: config.RecoveredEndpointPolicyAutoReturn})
	cfg.UpdateEndpoints([]config.Endpoint{
		failoverPolicyTestEndpoint("A", recovered.URL),
		failoverPolicyTestEndpoint("B", "https://b.example"),
	})
	p := New(cfg, &noopStatsStorage{}, store, "test-device")
	p.httpClient = recovered.Client()

	failureAt := time.Now().Add(-2 * time.Hour).UTC()
	successAt := failureAt.Add(time.Minute)
	reason := "quota_exhausted"
	if _, err := store.UpsertEndpointRuntimeStatus("A", storage.EndpointRuntimeStatusPatch{
		LastFailureAt:     &failureAt,
		LastFailureReason: &reason,
		LastSuccessAt:     &successAt,
	}); err != nil {
		t.Fatalf("seed runtime status: %v", err)
	}

	p.seedHealthCheckWatchSet()

	available := p.getRequestPlanEndpoints(cfg.GetEndpoints(), requestObservability{RequestID: "req-plan-expired-quota"})
	plan := newRequestEndpointPlanForCurrentWithSkip(available, cfg.GetEndpoints(), p.GetCurrentEndpointName(), p.isEndpointDeprioritized(p.GetCurrentEndpointName()))
	if got := plan.Current().Name; got != "B" {
		t.Fatalf("expected expired quota failure to keep A out of request plan, got %q", got)
	}
}

func TestExpiredQuotaBlockAutoReturnsAfterHealthProbeSucceeds(t *testing.T) {
	recovered := newHealthyResponsesStreamServer(t)
	defer recovered.Close()

	store, err := storage.NewSQLiteStorage(filepath.Join(t.TempDir(), "ccnexus.db"))
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	defer store.Close()

	cfg := config.DefaultConfig()
	cfg.UpdateFailover(&config.FailoverConfig{RecoveredEndpointPolicy: config.RecoveredEndpointPolicyAutoReturn})
	cfg.UpdateEndpoints([]config.Endpoint{
		failoverPolicyTestEndpoint("A", recovered.URL),
		failoverPolicyTestEndpoint("B", "https://b.example"),
	})
	p := New(cfg, &noopStatsStorage{}, store, "test-device")
	p.httpClient = recovered.Client()

	failureAt := time.Now().Add(-2 * time.Hour).UTC()
	reason := "quota_exhausted"
	if _, err := store.UpsertEndpointRuntimeStatus("A", storage.EndpointRuntimeStatusPatch{
		LastFailureAt:     &failureAt,
		LastFailureReason: &reason,
	}); err != nil {
		t.Fatalf("seed runtime status: %v", err)
	}

	p.seedHealthCheckWatchSet()
	if got := p.GetCurrentEndpointName(); got != "B" {
		t.Fatalf("expected seed to switch away from quota-blocked A, got %q", got)
	}

	p.runHealthCheckRound()
	if got := p.GetCurrentEndpointName(); got != "A" {
		t.Fatalf("expected successful quota probe to auto-return to A, got %q", got)
	}
	blocked := p.snapshotRuntimeBlockedEndpoints()
	if _, ok := blocked["A"]; ok {
		t.Fatal("expected successful quota probe to clear runtime block")
	}
}

func TestExpiredQuotaProbeFailureRenewsCooldown(t *testing.T) {
	quotaUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"insufficient_user_quota","message":"quota exhausted"}}`))
	}))
	defer quotaUpstream.Close()

	store, err := storage.NewSQLiteStorage(filepath.Join(t.TempDir(), "ccnexus.db"))
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	defer store.Close()

	cfg := config.DefaultConfig()
	cfg.UpdateFailover(&config.FailoverConfig{RecoveredEndpointPolicy: config.RecoveredEndpointPolicyAutoReturn})
	cfg.UpdateEndpoints([]config.Endpoint{
		failoverPolicyTestEndpoint("A", quotaUpstream.URL),
		failoverPolicyTestEndpoint("B", "https://b.example"),
	})
	p := New(cfg, &noopStatsStorage{}, store, "test-device")
	p.httpClient = quotaUpstream.Client()

	failureAt := time.Now().Add(-2 * time.Hour).UTC()
	reason := "quota_exhausted"
	if _, err := store.UpsertEndpointRuntimeStatus("A", storage.EndpointRuntimeStatusPatch{
		LastFailureAt:     &failureAt,
		LastFailureReason: &reason,
	}); err != nil {
		t.Fatalf("seed runtime status: %v", err)
	}

	p.seedHealthCheckWatchSet()
	p.runHealthCheckRound()

	if got := p.GetCurrentEndpointName(); got != "B" {
		t.Fatalf("expected failed quota probe to keep current endpoint B, got %q", got)
	}
	cooldown, ok := p.endpointCooldown("A")
	if !ok || !cooldown.Until.After(time.Now()) || cooldown.Reason != "quota_exhausted" {
		t.Fatalf("expected failed quota probe to renew quota cooldown, got cooldown=%#v ok=%t", cooldown, ok)
	}
	blocked := p.snapshotRuntimeBlockedEndpoints()
	if blocked["A"] != "quota_exhausted" {
		t.Fatalf("expected failed quota probe to keep runtime block, got %#v", blocked)
	}
}

func TestHealthCheckDefersActiveUpstreamCooldown(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	p := newFailoverPolicyTestProxy([]config.Endpoint{
		failoverPolicyTestEndpoint("A", upstream.URL),
		failoverPolicyTestEndpoint("B", "https://b.example"),
	}, upstream.Client())

	p.markEndpointCooldown("A", "upstream_5xx", time.Hour, requestObservability{RequestID: "upstream-cooldown"}, 1)
	p.registerForHealthCheck("A")
	p.runHealthCheckRound()

	if hits != 0 {
		t.Fatalf("expected active upstream cooldown to defer health probe, got hits=%d", hits)
	}
	cooldown, ok := p.endpointCooldown("A")
	if !ok || !cooldown.Until.After(time.Now()) || cooldown.Reason != "upstream_5xx" {
		t.Fatalf("expected upstream cooldown to remain active, got cooldown=%#v ok=%t", cooldown, ok)
	}
}

func TestQuotaBlockedHealthProbeKeepsBlockOnDifferentFailure(t *testing.T) {
	unavailableUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"model_not_found","message":"No available channel for model claude-opus-4-7 under group 1052 (request id: 202605130302429)"}}`))
	}))
	defer unavailableUpstream.Close()

	store, err := storage.NewSQLiteStorage(filepath.Join(t.TempDir(), "ccnexus.db"))
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	defer store.Close()

	cfg := config.DefaultConfig()
	cfg.UpdateFailover(&config.FailoverConfig{RecoveredEndpointPolicy: config.RecoveredEndpointPolicyAutoReturn})
	cfg.UpdateEndpoints([]config.Endpoint{
		failoverPolicyTestEndpoint("A", unavailableUpstream.URL),
		failoverPolicyTestEndpoint("B", "https://b.example"),
	})
	p := New(cfg, &noopStatsStorage{}, store, "test-device")
	p.httpClient = unavailableUpstream.Client()

	failureAt := time.Now().Add(-2 * time.Hour).UTC()
	reason := "quota_exhausted"
	if _, err := store.UpsertEndpointRuntimeStatus("A", storage.EndpointRuntimeStatusPatch{
		LastFailureAt:     &failureAt,
		LastFailureReason: &reason,
	}); err != nil {
		t.Fatalf("seed runtime status: %v", err)
	}

	p.seedHealthCheckWatchSet()
	p.runHealthCheckRound()

	blocked := p.snapshotRuntimeBlockedEndpoints()
	if blocked["A"] != "quota_exhausted" {
		t.Fatalf("expected non-success health probe to preserve quota runtime block, got %#v", blocked)
	}
	statuses, err := store.GetEndpointRuntimeStatuses()
	if err != nil {
		t.Fatalf("get runtime statuses: %v", err)
	}
	status := statuses["A"]
	if status == nil || status.LastFailureReason != "quota_exhausted" || status.LastFailureStatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected persisted quota block with latest status, got %#v", status)
	}
}

func TestProbeEndpointHealthStopsAfterStreamCompletion(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		upstream.Close()
	}()

	client := upstream.Client()
	client.Timeout = 2 * time.Second
	p := newFailoverPolicyTestProxy([]config.Endpoint{
		failoverPolicyTestEndpoint("A", upstream.URL),
	}, client)
	endpoint := failoverPolicyTestEndpoint("A", upstream.URL)

	start := time.Now()
	result := p.probeEndpointHealth(&endpoint)
	elapsed := time.Since(start)

	if !result.Success {
		t.Fatalf("expected health check success, got result=%#v", result)
	}
	if elapsed > time.Second {
		t.Fatalf("expected health check to return after completion without waiting for stream close, elapsed=%s", elapsed)
	}
}

func TestProbeEndpointHealthRejectsIncompleteStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"))
	}))
	defer upstream.Close()

	p := newFailoverPolicyTestProxy([]config.Endpoint{
		failoverPolicyTestEndpoint("A", upstream.URL),
	}, upstream.Client())
	endpoint := failoverPolicyTestEndpoint("A", upstream.URL)

	result := p.probeEndpointHealth(&endpoint)
	if result.Success {
		t.Fatalf("expected incomplete stream to fail health check, got result=%#v", result)
	}
	if result.Error == "" {
		t.Fatalf("expected incomplete stream error, got result=%#v", result)
	}
}

func TestHealthCheckRecoveryPersistsRuntimeSuccess(t *testing.T) {
	recovered := newHealthyResponsesStreamServer(t)
	defer recovered.Close()

	store, err := storage.NewSQLiteStorage(filepath.Join(t.TempDir(), "ccnexus.db"))
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	defer store.Close()

	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints([]config.Endpoint{
		failoverPolicyTestEndpoint("A", recovered.URL),
	})
	p := New(cfg, &noopStatsStorage{}, store, "test-device")
	p.httpClient = recovered.Client()

	failureAt := time.Now().Add(-time.Minute).UTC()
	reason := "upstream_stream_error"
	if _, err := store.UpsertEndpointRuntimeStatus("A", storage.EndpointRuntimeStatusPatch{
		LastFailureAt:     &failureAt,
		LastFailureReason: &reason,
	}); err != nil {
		t.Fatalf("seed runtime status: %v", err)
	}

	p.registerForHealthCheck("A")
	p.runHealthCheckRound()

	statuses, err := store.GetEndpointRuntimeStatuses()
	if err != nil {
		t.Fatalf("get runtime statuses: %v", err)
	}
	status := statuses["A"]
	if status == nil || status.LastSuccessAt == nil {
		t.Fatalf("expected health check recovery to persist last success, got %#v", status)
	}
	if !status.LastSuccessAt.After(failureAt) {
		t.Fatalf("expected last success after failure, got success=%v failure=%v", status.LastSuccessAt, failureAt)
	}
}

func newHealthyResponsesStreamServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}
