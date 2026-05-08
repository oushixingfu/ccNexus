package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/lich0821/ccNexus/internal/config"
)

func TestUpdateConfigPreservesCurrentEndpointByNameAfterReorder(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints([]config.Endpoint{
		failoverPolicyTestEndpoint("A", "https://a.example"),
		failoverPolicyTestEndpoint("B", "https://b.example"),
		failoverPolicyTestEndpoint("C", "https://c.example"),
	})
	p := New(cfg, &noopStatsStorage{}, nil, "test-device")

	if err := p.SetCurrentEndpoint("B"); err != nil {
		t.Fatalf("set current endpoint: %v", err)
	}
	currentName := p.GetCurrentEndpointName()

	cfg.UpdateEndpoints([]config.Endpoint{
		failoverPolicyTestEndpoint("B", "https://b.example"),
		failoverPolicyTestEndpoint("C", "https://c.example"),
		failoverPolicyTestEndpoint("A", "https://a.example"),
	})
	if err := p.UpdateConfigPreservingCurrentName(cfg, currentName); err != nil {
		t.Fatalf("update config: %v", err)
	}

	if got := p.GetCurrentEndpointName(); got != "B" {
		t.Fatalf("expected current endpoint B after reorder, got %q", got)
	}
}

func TestSetCurrentEndpointDoesNotCancelInFlightRequests(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints([]config.Endpoint{
		failoverPolicyTestEndpoint("A", "https://a.example"),
		failoverPolicyTestEndpoint("B", "https://b.example"),
	})
	p := New(cfg, &noopStatsStorage{}, nil, "test-device")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.endpointCtx["A"] = ctx
	p.endpointCancel["A"] = cancel

	if err := p.SetCurrentEndpoint("B"); err != nil {
		t.Fatalf("set current endpoint: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("expected manual default switch not to cancel in-flight endpoint context, got %v", ctx.Err())
	}
}

func TestConfigReorderPreservesEndpointCooldown(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints([]config.Endpoint{
		failoverPolicyTestEndpoint("A", "https://a.example"),
		failoverPolicyTestEndpoint("B", "https://b.example"),
	})
	p := New(cfg, &noopStatsStorage{}, nil, "test-device")

	p.markEndpointCooldown("A", "quota_exhausted", 10*time.Minute, requestObservability{RequestID: "req-cooldown"}, 1)
	currentName := p.GetCurrentEndpointName()
	cfg.UpdateEndpoints([]config.Endpoint{
		failoverPolicyTestEndpoint("B", "https://b.example"),
		failoverPolicyTestEndpoint("A", "https://a.example"),
	})
	if err := p.UpdateConfigPreservingCurrentName(cfg, currentName); err != nil {
		t.Fatalf("update config: %v", err)
	}

	p.cooldownMu.RLock()
	_, stillCooled := p.endpointCooldowns["A"]
	p.cooldownMu.RUnlock()
	if !stillCooled {
		t.Fatal("expected reorder-only config update to preserve endpoint cooldown")
	}
}

func TestConfigIdentityChangeClearsOnlyChangedEndpointCooldown(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints([]config.Endpoint{
		failoverPolicyTestEndpoint("A", "https://a.example"),
		failoverPolicyTestEndpoint("B", "https://b.example"),
	})
	p := New(cfg, &noopStatsStorage{}, nil, "test-device")

	p.markEndpointCooldown("A", "quota_exhausted", 10*time.Minute, requestObservability{RequestID: "req-cooldown-a"}, 1)
	p.markEndpointCooldown("B", "quota_exhausted", 10*time.Minute, requestObservability{RequestID: "req-cooldown-b"}, 1)
	currentName := p.GetCurrentEndpointName()

	changedA := failoverPolicyTestEndpoint("A", "https://a-new.example")
	cfg.UpdateEndpoints([]config.Endpoint{
		changedA,
		failoverPolicyTestEndpoint("B", "https://b.example"),
	})
	if err := p.UpdateConfigPreservingCurrentName(cfg, currentName); err != nil {
		t.Fatalf("update config: %v", err)
	}

	p.cooldownMu.RLock()
	_, aCooled := p.endpointCooldowns["A"]
	_, bCooled := p.endpointCooldowns["B"]
	p.cooldownMu.RUnlock()
	if aCooled {
		t.Fatal("expected identity-changing endpoint update to clear A cooldown")
	}
	if !bCooled {
		t.Fatal("expected unchanged endpoint B cooldown to remain")
	}
}

func TestRequestPlanStartsAtCurrentEndpointAfterFilteringEarlierCooldown(t *testing.T) {
	endpoints := []config.Endpoint{
		failoverPolicyTestEndpoint("A", "https://a.example"),
		failoverPolicyTestEndpoint("B", "https://b.example"),
		failoverPolicyTestEndpoint("C", "https://c.example"),
	}
	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints(endpoints)
	p := New(cfg, &noopStatsStorage{}, nil, "test-device")
	if err := p.SetCurrentEndpoint("B"); err != nil {
		t.Fatalf("set current endpoint: %v", err)
	}
	p.markEndpointCooldown("A", "quota_exhausted", 10*time.Minute, requestObservability{RequestID: "req-plan"}, 1)

	available := p.getRequestPlanEndpoints(endpoints, requestObservability{RequestID: "req-plan"})
	plan := newRequestEndpointPlanForCurrent(available, endpoints, p.GetCurrentEndpointName())
	if got := plan.Current().Name; got != "B" {
		t.Fatalf("expected request plan to start at current endpoint B after filtering A, got %q", got)
	}
}

func TestRequestPlanStartsAtNextEndpointWhenCurrentIsCooled(t *testing.T) {
	endpoints := []config.Endpoint{
		failoverPolicyTestEndpoint("A", "https://a.example"),
		failoverPolicyTestEndpoint("B", "https://b.example"),
		failoverPolicyTestEndpoint("C", "https://c.example"),
	}
	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints(endpoints)
	p := New(cfg, &noopStatsStorage{}, nil, "test-device")
	p.markEndpointCooldown("A", "quota_exhausted", 10*time.Minute, requestObservability{RequestID: "req-plan-current-cooled"}, 1)

	available := p.getRequestPlanEndpoints(endpoints, requestObservability{RequestID: "req-plan-current-cooled"})
	plan := newRequestEndpointPlanForCurrent(available, endpoints, p.GetCurrentEndpointName())
	if got := plan.Current().Name; got != "B" {
		t.Fatalf("expected request plan to start at next available endpoint B when current A is cooled, got %q", got)
	}
}

func TestUpdateConfigFallsBackAndEmitsEventWhenCurrentEndpointDisabled(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.UpdateEndpoints([]config.Endpoint{
		failoverPolicyTestEndpoint("A", "https://a.example"),
		failoverPolicyTestEndpoint("B", "https://b.example"),
		failoverPolicyTestEndpoint("C", "https://c.example"),
	})
	p := New(cfg, &noopStatsStorage{}, nil, "test-device")
	if err := p.SetCurrentEndpoint("B"); err != nil {
		t.Fatalf("set current endpoint: %v", err)
	}

	var events []EndpointCurrentEvent
	p.SetOnCurrentEndpointChanged(func(event EndpointCurrentEvent) {
		events = append(events, event)
	})

	currentName := p.GetCurrentEndpointName()
	disabledB := failoverPolicyTestEndpoint("B", "https://b.example")
	disabledB.Enabled = false
	cfg.UpdateEndpoints([]config.Endpoint{
		failoverPolicyTestEndpoint("A", "https://a.example"),
		disabledB,
		failoverPolicyTestEndpoint("C", "https://c.example"),
	})
	if err := p.UpdateConfigPreservingCurrentName(cfg, currentName); err != nil {
		t.Fatalf("update config: %v", err)
	}

	if got := p.GetCurrentEndpointName(); got != "A" {
		t.Fatalf("expected fallback current endpoint A, got %q", got)
	}
	if len(events) != 1 {
		t.Fatalf("expected one current endpoint event, got %d", len(events))
	}
	if events[0].PreviousName != "B" || events[0].Name != "A" || events[0].Reason != "config_update" {
		t.Fatalf("unexpected current endpoint event: %#v", events[0])
	}
}
