package proxy

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lich0821/ccNexus/internal/config"
	"github.com/lich0821/ccNexus/internal/logger"
)

type requestEndpointPlan struct {
	endpoints []config.Endpoint
	index     int
}

func newRequestEndpointPlan(endpoints []config.Endpoint, startIndex int) *requestEndpointPlan {
	copied := make([]config.Endpoint, len(endpoints))
	copy(copied, endpoints)
	if len(copied) == 0 {
		return &requestEndpointPlan{endpoints: copied, index: 0}
	}
	if startIndex < 0 {
		startIndex = 0
	}
	return &requestEndpointPlan{
		endpoints: copied,
		index:     startIndex % len(copied),
	}
}

func newRequestEndpointPlanForCurrent(available []config.Endpoint, allEnabled []config.Endpoint, currentName string) *requestEndpointPlan {
	return newRequestEndpointPlanForCurrentWithSkip(available, allEnabled, currentName, false)
}

func newRequestEndpointPlanForCurrentWithSkip(available []config.Endpoint, allEnabled []config.Endpoint, currentName string, skipCurrent bool) *requestEndpointPlan {
	if len(available) == 0 {
		return newRequestEndpointPlan(available, 0)
	}

	if currentName == "" {
		return newRequestEndpointPlan(available, 0)
	}

	currentIndex := indexEndpointByName(allEnabled, currentName)
	if currentIndex < 0 {
		if !skipCurrent {
			if availableIndex := indexEndpointByName(available, currentName); availableIndex >= 0 {
				return newRequestEndpointPlan(available, availableIndex)
			}
		}
		return newRequestEndpointPlan(available, 0)
	}

	startOffset := 0
	if skipCurrent {
		startOffset = 1
	}
	for offset := startOffset; offset < len(allEnabled); offset++ {
		candidate := allEnabled[(currentIndex+offset)%len(allEnabled)]
		if availableIndex := indexEndpointByName(available, candidate.Name); availableIndex >= 0 {
			return newRequestEndpointPlan(available, availableIndex)
		}
	}

	if skipCurrent {
		if availableIndex := indexEndpointByName(available, currentName); availableIndex >= 0 {
			return newRequestEndpointPlan(available, availableIndex)
		}
	}

	for offset := 0; offset < startOffset; offset++ {
		candidate := allEnabled[(currentIndex+offset)%len(allEnabled)]
		if availableIndex := indexEndpointByName(available, candidate.Name); availableIndex >= 0 {
			return newRequestEndpointPlan(available, availableIndex)
		}
	}

	return newRequestEndpointPlan(available, 0)
}

func indexEndpointByName(endpoints []config.Endpoint, name string) int {
	name = strings.TrimSpace(name)
	if name == "" {
		return -1
	}
	for i, endpoint := range endpoints {
		if endpoint.Name == name {
			return i
		}
	}
	return -1
}

func (p *requestEndpointPlan) Current() config.Endpoint {
	if p == nil || len(p.endpoints) == 0 {
		return config.Endpoint{}
	}
	return p.endpoints[p.index%len(p.endpoints)]
}

func (p *requestEndpointPlan) Advance() config.Endpoint {
	if p == nil || len(p.endpoints) == 0 {
		return config.Endpoint{}
	}
	p.index = (p.index + 1) % len(p.endpoints)
	return p.Current()
}

func (p *requestEndpointPlan) Len() int {
	if p == nil {
		return 0
	}
	return len(p.endpoints)
}

func (p *Proxy) getCurrentEndpointIndex() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	endpoints := p.getEnabledEndpoints()
	if len(endpoints) == 0 {
		return 0
	}
	return p.currentIndex % len(endpoints)
}

func (p *Proxy) advanceRequestEndpoint(plan *requestEndpointPlan, from config.Endpoint, obs requestObservability, attemptNumber int, reason string) config.Endpoint {
	if plan == nil || plan.Len() == 0 {
		return config.Endpoint{}
	}
	if plan.Len() == 1 {
		return plan.Current()
	}

	to := plan.Advance()
	if strings.TrimSpace(from.Name) != "" && from.Name != to.Name {
		logger.Debug("[FAILOVER] %s → %s %s failover_scope=request_local failover_reason=%s",
			from.Name,
			to.Name,
			requestLogFields(obs, from.Name, attemptNumber, 0, reason),
			sanitizeLogField(reason),
		)
	}
	return to
}

func shouldFailoverAfterSemanticEmpty(useSpecificEndpoint bool, plan *requestEndpointPlan, retry int, maxRetries int) bool {
	return !useSpecificEndpoint &&
		plan != nil &&
		plan.Len() > 1 &&
		retry+1 < maxRetries
}

func semanticEmptyBackoffDuration(attempt int) time.Duration {
	switch {
	case attempt <= 1:
		return 1 * time.Second
	case attempt == 2:
		return 2 * time.Second
	case attempt == 3:
		return 4 * time.Second
	default:
		return 8 * time.Second
	}
}

func (p *Proxy) sleepBeforeRetry(d time.Duration) {
	if d <= 0 {
		return
	}
	if p.retrySleep != nil {
		p.retrySleep(d)
		return
	}
	time.Sleep(d)
}

func rateLimitBackoffDuration(attempt int, headers http.Header) time.Duration {
	if retryAfter := parseRetryAfterHeader(headers.Get("Retry-After")); retryAfter > 0 {
		return retryAfter
	}
	switch attempt {
	case 1:
		return 800 * time.Millisecond
	case 2:
		return 1500 * time.Millisecond
	default:
		return 2500 * time.Millisecond
	}
}

func parseRetryAfterHeader(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if retryAt, err := http.ParseTime(value); err == nil {
		return time.Until(retryAt)
	}
	return 0
}
