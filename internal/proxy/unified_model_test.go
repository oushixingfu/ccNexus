package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lich0821/ccNexus/internal/config"
)

func TestUnifiedModelRoutesBySortedEndpointsAndUsesEndpointModelUpstream(t *testing.T) {
	primaryHits := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"temporary primary failure"}}`))
	}))
	defer primary.Close()

	fallbackHits := 0
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits++
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if got := payload["model"]; got != "real-fallback-model" {
			t.Fatalf("expected fallback upstream to receive endpoint model, got %#v", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-fallback","model":"real-fallback-model","usage":{"input_tokens":1,"output_tokens":2},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
	}))
	defer fallback.Close()

	primaryEndpoint := failoverPolicyTestEndpoint("Primary", primary.URL)
	primaryEndpoint.Model = "real-primary-model"
	fallbackEndpoint := failoverPolicyTestEndpoint("Fallback", fallback.URL)
	fallbackEndpoint.Model = "real-fallback-model"
	p := newFailoverPolicyTestProxy([]config.Endpoint{primaryEndpoint, fallbackEndpoint}, primary.Client())
	p.config.UpdateUnifiedModel(&config.UnifiedModelConfig{
		Enabled:                          true,
		Name:                             "gpt-5.5",
		AdvertiseOnlyUnifiedModel:        true,
		EndpointScope:                    config.UnifiedModelEndpointScopeAllEnabled,
		HotStandby:                       true,
		PreserveExplicitEndpointOverride: true,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":false,"input":[]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected unified model fallback to succeed, got status=%d body=%q", rec.Code, rec.Body.String())
	}
	if primaryHits != endpointSlowFailoverAttempts {
		t.Fatalf("expected primary to be tried before fallback, got %d", primaryHits)
	}
	if fallbackHits != 1 {
		t.Fatalf("expected fallback to be used once, got %d", fallbackHits)
	}
	var response map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode downstream response: %v", err)
	}
	if got := response["model"]; got != "gpt-5.5" {
		t.Fatalf("expected downstream model to be rewritten to unified model, got %#v body=%s", got, rec.Body.String())
	}
}

func TestUnifiedModelUsesLiveEndpointOrderAfterReorder(t *testing.T) {
	hits := map[string]int{}
	newServer := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits[name]++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-` + name + `","model":"real-` + name + `","usage":{"input_tokens":1,"output_tokens":2},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
		}))
	}
	first := newServer("first")
	defer first.Close()
	second := newServer("second")
	defer second.Close()

	firstEndpoint := failoverPolicyTestEndpoint("First", first.URL)
	firstEndpoint.Model = "real-first"
	secondEndpoint := failoverPolicyTestEndpoint("Second", second.URL)
	secondEndpoint.Model = "real-second"
	p := newFailoverPolicyTestProxy([]config.Endpoint{firstEndpoint, secondEndpoint}, first.Client())
	p.config.UpdateUnifiedModel(&config.UnifiedModelConfig{
		Enabled:                          true,
		Name:                             "gpt-5.5",
		AdvertiseOnlyUnifiedModel:        true,
		EndpointScope:                    config.UnifiedModelEndpointScopeAllEnabled,
		HotStandby:                       true,
		PreserveExplicitEndpointOverride: true,
	})

	issueUnifiedResponsesRequest(t, p)
	if hits["first"] != 1 || hits["second"] != 0 {
		t.Fatalf("expected initial sorted order to use first endpoint, got hits=%#v", hits)
	}

	p.config.UpdateEndpoints([]config.Endpoint{secondEndpoint, firstEndpoint})
	p.configEndpointsSnapshot = cloneEndpoints(p.config.GetEndpoints())
	issueUnifiedResponsesRequest(t, p)
	if hits["second"] != 1 {
		t.Fatalf("expected reordered live endpoint order to use second endpoint, got hits=%#v", hits)
	}
}

func TestUnifiedModelInvalidUpstreamRequestTriesNextEndpoint(t *testing.T) {
	invalidHits := 0
	invalid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		invalidHits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"field messages is required","type":"new_api_error","code":"invalid_request"}}`))
	}))
	defer invalid.Close()

	successHits := 0
	success := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		successHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-success","model":"real-success","usage":{"input_tokens":1,"output_tokens":2},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
	}))
	defer success.Close()

	invalidEndpoint := failoverPolicyTestEndpoint("Invalid", invalid.URL)
	invalidEndpoint.Model = "real-invalid"
	successEndpoint := failoverPolicyTestEndpoint("Success", success.URL)
	successEndpoint.Model = "real-success"
	p := newFailoverPolicyTestProxy([]config.Endpoint{invalidEndpoint, successEndpoint}, invalid.Client())
	p.config.UpdateUnifiedModel(&config.UnifiedModelConfig{
		Enabled:                          true,
		Name:                             "gpt-5.5",
		AdvertiseOnlyUnifiedModel:        true,
		EndpointScope:                    config.UnifiedModelEndpointScopeAllEnabled,
		HotStandby:                       true,
		PreserveExplicitEndpointOverride: true,
	})

	issueUnifiedResponsesRequest(t, p)
	if invalidHits != 1 || successHits != 1 {
		t.Fatalf("expected unified route to skip invalid upstream and try next endpoint, got invalid=%d success=%d", invalidHits, successHits)
	}

	issueUnifiedResponsesRequest(t, p)
	if invalidHits != 1 || successHits != 2 {
		t.Fatalf("expected protocol cooldown to skip invalid endpoint on next request, got invalid=%d success=%d", invalidHits, successHits)
	}
}

func TestUnifiedModelProtocolCooldownUsesConfigErrorWindowForInvalidRequest(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.UpdateFailover(&config.FailoverConfig{
		RecoveredEndpointPolicy: config.RecoveredEndpointPolicyAutoReturn,
		Cooldowns: &config.FailoverCooldownConfig{
			QuotaExhaustedSec:   11,
			RateLimitedSec:      12,
			UpstreamErrorSec:    13,
			NetworkErrorSec:     14,
			TokenUnavailableSec: 15,
			ConfigErrorSec:      16,
		},
	})
	p := New(cfg, &noopStatsStorage{}, nil, "test-device")

	if got := p.protocolCooldownDurationForReason("upstream_invalid_request"); got != 16*time.Second {
		t.Fatalf("expected invalid upstream request to use config-error cooldown, got %s", got)
	}
	if got := p.protocolCooldownDurationForReason("upstream_5xx"); got != 13*time.Second {
		t.Fatalf("expected generic upstream protocol cooldown to use upstream-error cooldown, got %s", got)
	}
}

func issueUnifiedResponsesRequest(t *testing.T, p *Proxy) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":false,"input":[]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.handleProxy(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected unified request to succeed, got status=%d body=%q", rec.Code, rec.Body.String())
	}
}
