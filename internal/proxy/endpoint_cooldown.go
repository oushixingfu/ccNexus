package proxy

import (
	"strings"
	"time"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/logger"
)

const endpointQuotaExhaustedCooldown = 10 * time.Minute

type endpointCooldown struct {
	Reason string
	Until  time.Time
}

func (p *Proxy) markEndpointCooldown(endpointName string, reason string, duration time.Duration, obs requestObservability, attemptNumber int) {
	if endpointName == "" || duration <= 0 {
		return
	}
	until := time.Now().Add(duration)

	p.cooldownMu.Lock()
	if p.endpointCooldowns == nil {
		p.endpointCooldowns = make(map[string]endpointCooldown)
	}
	p.endpointCooldowns[endpointName] = endpointCooldown{Reason: sanitizeLogField(reason), Until: until}
	p.cooldownMu.Unlock()

	logger.Debug("[COOLDOWN] %s cooldown=%s until=%s %s cooldown_reason=%s",
		endpointName,
		duration.Round(time.Millisecond),
		until.Format(time.RFC3339),
		requestLogFields(obs, endpointName, attemptNumber, 0, reason),
		sanitizeLogField(reason),
	)
}

func (p *Proxy) clearEndpointCooldown(endpointName string) {
	if endpointName == "" {
		return
	}
	p.cooldownMu.Lock()
	defer p.cooldownMu.Unlock()
	delete(p.endpointCooldowns, endpointName)
}

func (p *Proxy) clearEndpointCooldowns() {
	p.cooldownMu.Lock()
	defer p.cooldownMu.Unlock()
	p.endpointCooldowns = make(map[string]endpointCooldown)
}

func (p *Proxy) clearEndpointCooldownsForConfigChange(oldEndpoints []config.Endpoint, newEndpoints []config.Endpoint) {
	if len(oldEndpoints) == 0 {
		return
	}

	oldByName := make(map[string]config.Endpoint, len(oldEndpoints))
	for _, endpoint := range oldEndpoints {
		oldByName[endpoint.Name] = endpoint
	}

	newByName := make(map[string]config.Endpoint, len(newEndpoints))
	for _, endpoint := range newEndpoints {
		newByName[endpoint.Name] = endpoint
	}

	toClear := make([]string, 0)
	for oldName := range oldByName {
		if _, ok := newByName[oldName]; !ok {
			toClear = append(toClear, oldName)
		}
	}
	for newName, newEndpoint := range newByName {
		oldEndpoint, ok := oldByName[newName]
		if !ok {
			continue
		}
		if endpointCooldownIdentityChanged(oldEndpoint, newEndpoint) {
			toClear = append(toClear, newName)
		}
	}
	if len(toClear) == 0 {
		return
	}

	p.cooldownMu.Lock()
	for _, name := range toClear {
		delete(p.endpointCooldowns, name)
	}
	p.cooldownMu.Unlock()

	logger.Debug("[CONFIG UPDATE] Cleared endpoint cooldowns for changed endpoints: %v", toClear)
}

func endpointCooldownIdentityChanged(oldEndpoint config.Endpoint, newEndpoint config.Endpoint) bool {
	return strings.TrimSpace(oldEndpoint.APIUrl) != strings.TrimSpace(newEndpoint.APIUrl) ||
		strings.TrimSpace(oldEndpoint.APIKey) != strings.TrimSpace(newEndpoint.APIKey) ||
		config.NormalizeAuthMode(oldEndpoint.AuthMode) != config.NormalizeAuthMode(newEndpoint.AuthMode) ||
		strings.TrimSpace(strings.ToLower(oldEndpoint.Transformer)) != strings.TrimSpace(strings.ToLower(newEndpoint.Transformer)) ||
		strings.TrimSpace(oldEndpoint.Model) != strings.TrimSpace(newEndpoint.Model) ||
		strings.TrimSpace(oldEndpoint.Thinking) != strings.TrimSpace(newEndpoint.Thinking) ||
		oldEndpoint.ForceStream != newEndpoint.ForceStream
}

func (p *Proxy) getRequestPlanEndpoints(endpoints []config.Endpoint, obs requestObservability) []config.Endpoint {
	if len(endpoints) <= 1 {
		return endpoints
	}

	now := time.Now()
	available := make([]config.Endpoint, 0, len(endpoints))

	p.cooldownMu.Lock()
	defer p.cooldownMu.Unlock()

	for _, endpoint := range endpoints {
		cooldown, ok := p.endpointCooldowns[endpoint.Name]
		if !ok {
			available = append(available, endpoint)
			continue
		}
		if !cooldown.Until.After(now) {
			delete(p.endpointCooldowns, endpoint.Name)
			available = append(available, endpoint)
			continue
		}
		logger.Debug("[COOLDOWN] Skipping cooled endpoint for request plan: %s remaining=%s %s cooldown_reason=%s",
			endpoint.Name,
			cooldown.Until.Sub(now).Round(time.Millisecond),
			requestLogFields(obs, endpoint.Name, 0, 0, cooldown.Reason),
			sanitizeLogField(cooldown.Reason),
		)
	}

	if len(available) == 0 {
		logger.Debug("[COOLDOWN] All enabled endpoints are cooled; using full endpoint list %s", requestLogFields(obs, "", 0, 0, "all_endpoints_cooled"))
		return endpoints
	}
	return available
}
