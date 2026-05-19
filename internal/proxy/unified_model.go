package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/logger"
)

func (p *Proxy) unifiedModelConfig() *config.UnifiedModelConfig {
	if p == nil || p.config == nil {
		return config.DefaultUnifiedModelConfig()
	}
	return p.config.GetUnifiedModel()
}

func (p *Proxy) unifiedModelForRequest(model string) (*config.UnifiedModelConfig, bool) {
	unified := p.unifiedModelConfig()
	if !config.UnifiedModelMatches(unified, model) {
		return unified, false
	}
	return unified, true
}

func unifiedEndpointForUpstream(endpoint config.Endpoint, unified *config.UnifiedModelConfig) config.Endpoint {
	if unified == nil || !unified.Enabled {
		return endpoint
	}
	if strings.TrimSpace(endpoint.Model) != "" {
		return endpoint
	}
	effective := endpoint
	effective.Model = strings.TrimSpace(unified.Name)
	return effective
}

func protocolCooldownKey(endpointName string, clientFormat ClientFormat) string {
	return strings.TrimSpace(endpointName) + "|" + string(clientFormat)
}

func (p *Proxy) markProtocolCooldown(endpointName string, clientFormat ClientFormat, reason string) {
	if p == nil || strings.TrimSpace(endpointName) == "" {
		return
	}
	duration := p.protocolCooldownDurationForReason(reason)
	if duration <= 0 {
		duration = 60 * time.Second
	}
	until := time.Now().Add(duration)
	key := protocolCooldownKey(endpointName, clientFormat)

	p.protocolCooldownMu.Lock()
	if p.protocolCooldowns == nil {
		p.protocolCooldowns = make(map[string]time.Time)
	}
	p.protocolCooldowns[key] = until
	p.protocolCooldownMu.Unlock()

	logger.Debug("[UnifiedModel] Protocol cooldown set endpoint=%s client_format=%s until=%s reason=%s",
		endpointName,
		clientFormat,
		until.Format(time.RFC3339),
		sanitizeLogField(reason),
	)
}

func (p *Proxy) protocolCooldownDurationForReason(reason string) time.Duration {
	switch sanitizeLogField(reason) {
	case "upstream_invalid_request":
		return p.cooldownDurationForReason("build_request_failed", nil)
	default:
		return p.cooldownDurationForReason("upstream_5xx", nil)
	}
}

func (p *Proxy) filterProtocolCooledEndpoints(endpoints []config.Endpoint, clientFormat ClientFormat, obs requestObservability) []config.Endpoint {
	if p == nil || len(endpoints) <= 1 {
		return endpoints
	}

	now := time.Now()
	filtered := make([]config.Endpoint, 0, len(endpoints))

	p.protocolCooldownMu.Lock()
	defer p.protocolCooldownMu.Unlock()
	for _, endpoint := range endpoints {
		key := protocolCooldownKey(endpoint.Name, clientFormat)
		until, ok := p.protocolCooldowns[key]
		if !ok {
			filtered = append(filtered, endpoint)
			continue
		}
		if !until.After(now) {
			delete(p.protocolCooldowns, key)
			filtered = append(filtered, endpoint)
			continue
		}
		logger.Debug("[UnifiedModel] Skipping protocol-cooled endpoint %s client_format=%s remaining=%s %s",
			endpoint.Name,
			clientFormat,
			until.Sub(now).Round(time.Millisecond),
			requestLogFields(obs, endpoint.Name, 0, 0, "endpoint_protocol_cooldown"),
		)
	}
	if len(filtered) == 0 {
		return endpoints
	}
	return filtered
}

func (p *Proxy) clearProtocolCooldowns() {
	if p == nil {
		return
	}
	p.protocolCooldownMu.Lock()
	p.protocolCooldowns = make(map[string]time.Time)
	p.protocolCooldownMu.Unlock()
}

func rewriteJSONModelFields(body []byte, model string) []byte {
	model = strings.TrimSpace(model)
	if model == "" || len(body) == 0 {
		return body
	}

	var value interface{}
	if err := json.Unmarshal(body, &value); err != nil {
		return body
	}
	if !rewriteModelFieldValue(value, model) {
		return body
	}
	rewritten, err := json.Marshal(value)
	if err != nil {
		return body
	}
	return rewritten
}

func rewriteModelFieldValue(value interface{}, model string) bool {
	changed := false
	switch v := value.(type) {
	case map[string]interface{}:
		for key, child := range v {
			if strings.EqualFold(key, "model") {
				if _, ok := child.(string); ok {
					v[key] = model
					changed = true
					continue
				}
			}
			if rewriteModelFieldValue(child, model) {
				changed = true
			}
		}
	case []interface{}:
		for _, child := range v {
			if rewriteModelFieldValue(child, model) {
				changed = true
			}
		}
	}
	return changed
}

func rewriteSSEModelFields(event []byte, model string) []byte {
	model = strings.TrimSpace(model)
	if model == "" || len(event) == 0 {
		return event
	}

	lines := bytes.SplitAfter(event, []byte("\n"))
	changed := false
	for i, line := range lines {
		trimmed := bytes.TrimLeft(line, " \t")
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		prefixLen := len(line) - len(trimmed) + len("data:")
		prefix := line[:prefixLen]
		payloadWithNewline := line[prefixLen:]
		payload := bytes.TrimSpace(payloadWithNewline)
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		rewritten := rewriteJSONModelFields(payload, model)
		if bytes.Equal(rewritten, payload) {
			continue
		}
		newline := []byte{}
		if bytes.HasSuffix(line, []byte("\n")) {
			newline = []byte("\n")
		}
		lines[i] = append(append(append([]byte{}, prefix...), []byte(" ")...), append(rewritten, newline...)...)
		changed = true
	}
	if !changed {
		return event
	}
	return bytes.Join(lines, nil)
}
