package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/logger"
)

func TestHTTPFailureFallbackIsRequestLocalAndDoesNotSwitchGlobalEndpoint(t *testing.T) {
	logger.GetLogger().Clear()
	logger.GetLogger().SetMinLevel(logger.DEBUG)

	var primaryHits int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"Too Many Requests"}}`))
	}))
	defer primary.Close()

	var fallbackHits int
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-fallback","usage":{"input_tokens":1,"output_tokens":2},"output":[]}`))
	}))
	defer fallback.Close()

	p := newFailoverPolicyTestProxy([]config.Endpoint{
		failoverPolicyTestEndpoint("Primary", primary.URL),
		failoverPolicyTestEndpoint("Fallback", fallback.URL),
	}, primary.Client())

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":false,"input":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerCCNexusRequestID, "req-local-fallback")
	rec := httptest.NewRecorder()

	p.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected request-local fallback to succeed, got status=%d body=%q", rec.Code, rec.Body.String())
	}
	if primaryHits != endpointSlowFailoverAttempts {
		t.Fatalf("expected primary to be tried %d times before request-local fallback, got %d", endpointSlowFailoverAttempts, primaryHits)
	}
	if fallbackHits != 1 {
		t.Fatalf("expected fallback to be hit once, got %d", fallbackHits)
	}
	if got := rec.Header().Get(headerCCNexusEndpoint); got != "Fallback" {
		t.Fatalf("expected response endpoint header Fallback, got %q", got)
	}
	if got := p.GetCurrentEndpointName(); got != "Primary" {
		t.Fatalf("expected global current endpoint to remain Primary, got %q", got)
	}

	logs := joinedProxyLogs()
	if !strings.Contains(logs, "failover_scope=request_local") {
		t.Fatalf("expected request-local failover log, got logs:\n%s", logs)
	}
	if strings.Contains(logs, "[SWITCH]") {
		t.Fatalf("expected no global switch log during request-local fallback, got logs:\n%s", logs)
	}
}

func TestRequestLocalFallbackDoesNotAffectNextRequest(t *testing.T) {
	logger.GetLogger().Clear()
	logger.GetLogger().SetMinLevel(logger.DEBUG)

	primaryHitsByRequest := map[string]int{}
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get(headerCCNexusRequestID)
		primaryHitsByRequest[requestID]++
		w.Header().Set("Content-Type", "application/json")
		if requestID == "req-a" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"Too Many Requests"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp-primary","usage":{"input_tokens":1,"output_tokens":2},"output":[]}`))
	}))
	defer primary.Close()

	var fallbackHits int
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-fallback","usage":{"input_tokens":1,"output_tokens":2},"output":[]}`))
	}))
	defer fallback.Close()

	p := newFailoverPolicyTestProxy([]config.Endpoint{
		failoverPolicyTestEndpoint("Primary", primary.URL),
		failoverPolicyTestEndpoint("Fallback", fallback.URL),
	}, primary.Client())

	recA := issueFailoverPolicyTestRequest(p, "req-a")
	if recA.Code != http.StatusOK {
		t.Fatalf("expected req-a to succeed via request-local fallback, got status=%d body=%q", recA.Code, recA.Body.String())
	}
	if got := recA.Header().Get(headerCCNexusEndpoint); got != "Fallback" {
		t.Fatalf("expected req-a to complete on Fallback, got %q", got)
	}

	recB := issueFailoverPolicyTestRequest(p, "req-b")
	if recB.Code != http.StatusOK {
		t.Fatalf("expected req-b to succeed, got status=%d body=%q", recB.Code, recB.Body.String())
	}
	if got := recB.Header().Get(headerCCNexusEndpoint); got != "Primary" {
		t.Fatalf("expected req-b to still start on Primary after req-a fallback, got %q", got)
	}
	if got := primaryHitsByRequest["req-b"]; got != 1 {
		t.Fatalf("expected req-b to hit Primary once, got %d", got)
		t.Fatalf("expected req-b to hit Primary once, got %d", got)
	}
	if got := p.GetCurrentEndpointName(); got != "Primary" {
		t.Fatalf("expected global current endpoint to remain Primary, got %q", got)
	}
	if fallbackHits != 1 {
		t.Fatalf("expected only req-a to use fallback once, got fallback hits=%d", fallbackHits)
	}
}

func TestConcurrentRequestLocalFallbackDoesNotAffectOtherRequest(t *testing.T) {
	logger.GetLogger().Clear()
	logger.GetLogger().SetMinLevel(logger.DEBUG)

	var mu sync.Mutex
	primaryHitsByRequest := map[string]int{}
	fallbackHitsByRequest := map[string]int{}
	fallbackStarted := make(chan struct{})
	releaseFallback := make(chan struct{})
	var fallbackStartedOnce sync.Once
	var releaseFallbackOnce sync.Once
	t.Cleanup(func() {
		releaseFallbackOnce.Do(func() { close(releaseFallback) })
	})

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get(headerCCNexusRequestID)
		mu.Lock()
		primaryHitsByRequest[requestID]++
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if requestID == "req-a-concurrent" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{\"error\":{\"message\":\"Too Many Requests\"}}`))
			return
		}
		_, _ = w.Write([]byte(`{\"id\":\"resp-primary\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2},\"output\":[]}`))
	}))
	defer primary.Close()

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get(headerCCNexusRequestID)
		mu.Lock()
		fallbackHitsByRequest[requestID]++
		mu.Unlock()

		if requestID == "req-a-concurrent" {
			fallbackStartedOnce.Do(func() { close(fallbackStarted) })
			select {
			case <-releaseFallback:
			case <-time.After(3 * time.Second):
				http.Error(w, "timed out waiting to release fallback response", http.StatusGatewayTimeout)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{\"id\":\"resp-fallback\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2},\"output\":[]}`))
	}))
	defer fallback.Close()

	p := newFailoverPolicyTestProxy([]config.Endpoint{
		failoverPolicyTestEndpoint("Primary", primary.URL),
		failoverPolicyTestEndpoint("Fallback", fallback.URL),
	}, primary.Client())
	p.retrySleep = func(time.Duration) {}

	var recA *httptest.ResponseRecorder
	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		recA = issueFailoverPolicyTestRequest(p, "req-a-concurrent")
	}()

	select {
	case <-fallbackStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for req-a to reach request-local fallback")
	}

	recB := issueFailoverPolicyTestRequest(p, "req-b-concurrent")
	if recB.Code != http.StatusOK {
		t.Fatalf("expected concurrent req-b to succeed, got status=%d body=%q", recB.Code, recB.Body.String())
	}
	if got := recB.Header().Get(headerCCNexusEndpoint); got != "Primary" {
		t.Fatalf("expected concurrent req-b to remain on Primary while req-a is on Fallback, got %q", got)
	}

	releaseFallbackOnce.Do(func() { close(releaseFallback) })
	select {
	case <-doneA:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for req-a to finish")
	}
	if recA == nil || recA.Code != http.StatusOK {
		if recA == nil {
			t.Fatal("expected req-a recorder, got nil")
		}
		t.Fatalf("expected req-a to succeed via fallback, got status=%d body=%q", recA.Code, recA.Body.String())
	}
	if got := recA.Header().Get(headerCCNexusEndpoint); got != "Fallback" {
		t.Fatalf("expected req-a to complete on Fallback, got %q", got)
	}

	mu.Lock()
	primaryBHits := primaryHitsByRequest["req-b-concurrent"]
	fallbackBHits := fallbackHitsByRequest["req-b-concurrent"]
	primaryAHits := primaryHitsByRequest["req-a-concurrent"]
	fallbackAHits := fallbackHitsByRequest["req-a-concurrent"]
	mu.Unlock()
	if primaryAHits != endpointSlowFailoverAttempts || fallbackAHits != 1 {
		t.Fatalf("expected req-a to try Primary %d times then Fallback once, got primary=%d fallback=%d", endpointSlowFailoverAttempts, primaryAHits, fallbackAHits)
	}
	if primaryBHits != 1 || fallbackBHits != 0 {
		t.Fatalf("expected req-b to use only Primary once, got primary=%d fallback=%d", primaryBHits, fallbackBHits)
	}
	if got := p.GetCurrentEndpointName(); got != "Primary" {
		t.Fatalf("expected global current endpoint to remain Primary, got %q", got)
	}

	logs := joinedProxyLogs()
	if !strings.Contains(logs, "request_id=req-a-concurrent") || !strings.Contains(logs, "failover_scope=request_local") {
		t.Fatalf("expected req-a request-local failover log, got logs:\n%s", logs)
	}
	if strings.Contains(logs, "[SWITCH]") {
		t.Fatalf("expected no global switch log during concurrent request-local fallback, got logs:\n%s", logs)
	}
}

func TestRateLimitedRetryUsesBackoffBeforeSameEndpointRetry(t *testing.T) {
	logger.GetLogger().Clear()
	logger.GetLogger().SetMinLevel(logger.DEBUG)

	primaryHits := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits++
		w.Header().Set("Content-Type", "application/json")
		if primaryHits == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"Too Many Requests"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp-primary","usage":{"input_tokens":1,"output_tokens":2},"output":[]}`))
	}))
	defer primary.Close()

	p := newFailoverPolicyTestProxy([]config.Endpoint{
		failoverPolicyTestEndpoint("Primary", primary.URL),
	}, primary.Client())
	var sleeps []time.Duration
	p.retrySleep = func(d time.Duration) {
		sleeps = append(sleeps, d)
	}

	rec := issueFailoverPolicyTestRequest(p, "req-backoff")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected request to succeed after backoff retry, got status=%d body=%q", rec.Code, rec.Body.String())
	}
	if primaryHits != 2 {
		t.Fatalf("expected one rate-limited attempt and one success attempt, got %d hits", primaryHits)
	}
	if len(sleeps) != 1 {
		t.Fatalf("expected exactly one backoff sleep, got %d", len(sleeps))
	}
	if sleeps[0] != 800*time.Millisecond {
		t.Fatalf("expected first rate-limit backoff to be 800ms, got %s", sleeps[0])
	}
}

func issueFailoverPolicyTestRequest(p *Proxy, requestID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":false,"input":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerCCNexusRequestID, requestID)
	rec := httptest.NewRecorder()
	p.handleProxy(rec, req)
	return rec
}

func joinedProxyLogs() string {
	logs := logger.GetLogger().GetLogs()
	var builder strings.Builder
	for _, entry := range logs {
		builder.WriteString(entry.Message)
		builder.WriteByte('\n')
	}
	return builder.String()
}
