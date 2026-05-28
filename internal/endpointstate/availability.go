package endpointstate

import (
	"strings"

	"github.com/lich0821/ccNexus/internal/storage"
)

const (
	Available   = "available"
	Unavailable = "unavailable"
	Disabled    = "disabled"
	Unknown     = "unknown"
)

type Projection struct {
	Available    bool
	Availability string
	Reason       string
	StatusCode   int
}

func IsHardUnavailableReason(reason string) bool {
	return strings.TrimSpace(reason) == "quota_exhausted"
}

func Derive(enabled bool, status *storage.EndpointRuntimeStatus) Projection {
	return DeriveWithActiveCooldown(enabled, status, "")
}

func DeriveWithActiveCooldown(enabled bool, status *storage.EndpointRuntimeStatus, activeCooldownReason string) Projection {
	if !enabled {
		return Projection{Availability: Disabled}
	}
	if status == nil {
		reason := strings.TrimSpace(activeCooldownReason)
		if reason != "" {
			return Projection{Availability: Unavailable, Reason: reason}
		}
		return Projection{Availability: Unknown}
	}

	reason := strings.TrimSpace(status.LastFailureReason)
	if status.LastFailureAt != nil && reason != "" {
		if IsHardUnavailableReason(reason) {
			return Projection{
				Available:    false,
				Availability: Unavailable,
				Reason:       reason,
				StatusCode:   status.LastFailureStatusCode,
			}
		}
		activeReason := strings.TrimSpace(activeCooldownReason)
		if activeReason != "" {
			return Projection{
				Available:    false,
				Availability: Unavailable,
				Reason:       activeReason,
				StatusCode:   status.LastFailureStatusCode,
			}
		}
		if status.LastSuccessAt != nil && status.LastSuccessAt.After(*status.LastFailureAt) {
			return Projection{Available: true, Availability: Available}
		}
		return Projection{Availability: Unknown}
	}

	if activeReason := strings.TrimSpace(activeCooldownReason); activeReason != "" {
		return Projection{
			Available:    false,
			Availability: Unavailable,
			Reason:       activeReason,
			StatusCode:   status.LastFailureStatusCode,
		}
	}

	if status.LastSuccessAt != nil {
		return Projection{Available: true, Availability: Available}
	}

	return Projection{Availability: Unknown}
}
