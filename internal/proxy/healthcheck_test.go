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
